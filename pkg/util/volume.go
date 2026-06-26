package util

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"

	"github.com/docker/docker/api/types/mount"
	"github.com/moby/term"
	"github.com/spf13/cobra"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/cp"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// GetVolume key format: [container name]-[volume mount name]
func GetVolume(ctx context.Context, clientset kubernetes.Interface, f util.Factory, ns, podName string) (map[string][]mount.Mount, error) {
	pod, err := clientset.CoreV1().Pods(ns).Get(ctx, podName, v12.GetOptions{})
	if err != nil {
		return nil, err
	}
	result := map[string][]mount.Mount{}
	for _, container := range pod.Spec.Containers {
		// if container name is vpn or envoy-proxy, not need to download volume
		if container.Name == config.ContainerSidecarVPN || container.Name == config.ContainerSidecarEnvoyProxy {
			continue
		}
		var m []mount.Mount
		for _, volumeMount := range container.VolumeMounts {
			if volumeMount.MountPath == "/tmp" {
				continue
			}
			localPath := filepath.Join(os.TempDir(), strconv.Itoa(rand.Int()))
			err = os.MkdirAll(localPath, 0755)
			if err != nil {
				return nil, err
			}
			if volumeMount.SubPath != "" {
				localPath = filepath.Join(localPath, volumeMount.SubPath)
			}
			// pod-namespace/pod-name:path
			remotePath := fmt.Sprintf("%s/%s:%s", ns, podName, volumeMount.MountPath)
			stdIn, stdOut, stdErr := term.StdStreams()
			copyOptions := cp.NewCopyOptions(genericiooptions.IOStreams{In: stdIn, Out: stdOut, ErrOut: stdErr})
			copyOptions.Container = container.Name
			copyOptions.MaxTries = 10
			err = copyOptions.Complete(f, &cobra.Command{}, []string{remotePath, localPath})
			if err != nil {
				return nil, err
			}
			err = copyOptions.Run()
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Failed to download volume %s path %s to %s, err: %v, ignore...\n", volumeMount.Name, remotePath, localPath, err)
				continue
			}
			m = append(m, mount.Mount{
				Type:   mount.TypeBind,
				Source: localPath,
				Target: volumeMount.MountPath,
			})
			plog.G(ctx).Infof("%s:%s", localPath, volumeMount.MountPath)
		}
		result[Join(ns, container.Name)] = m
	}
	return result, nil
}

// RemoveDir removes all source directories referenced by the given Docker mounts map.
func RemoveDir(volume map[string][]mount.Mount) error {
	var errs []error
	for _, mounts := range volume {
		for _, m := range mounts {
			err := os.RemoveAll(m.Source)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("failed to delete dir %s: %w", m.Source, err))
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}
