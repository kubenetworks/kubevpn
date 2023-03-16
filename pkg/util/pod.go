package util

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/docker/docker/api/types/mount"
	"github.com/moby/term"
	"github.com/spf13/cobra"
	"golang.org/x/exp/constraints"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/cp"
)

func PrintStatus(pod *corev1.Pod, writer io.Writer) {
	w := tabwriter.NewWriter(writer, 1, 1, 1, ' ', 0)
	defer w.Flush()
	show := func(name string, v1, v2 any) {
		_, _ = fmt.Fprintf(w, "%s\t%v\t%v\n", name, v1, v2)
	}

	show("Container", "Reason", "Message")
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil {
			show(status.Name, status.State.Waiting.Reason, status.State.Waiting.Message)
		}
		if status.State.Running != nil {
			show(status.Name, "ContainerRunning", "")
		}
		if status.State.Terminated != nil {
			show(status.Name, status.State.Terminated.Reason, status.State.Terminated.Message)
		}
	}
}

func PrintStatusInline(pod *corev1.Pod) string {
	var sb = bytes.NewBuffer(nil)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	show := func(v1, v2 any) {
		_, _ = fmt.Fprintf(w, "%v\t\t%v", v1, v2)
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil {
			show(status.State.Waiting.Reason, status.State.Waiting.Message)
		}
		if status.State.Running != nil {
			show("ContainerRunning", "")
		}
		if status.State.Terminated != nil {
			show(status.State.Terminated.Reason, status.State.Terminated.Message)
		}
	}
	_ = w.Flush()
	return sb.String()
}

func max[T constraints.Ordered](a T, b T) T {
	if a > b {
		return a
	}
	return b
}

func GetEnv(ctx context.Context, f util.Factory, ns, pod string) (map[string][]string, error) {
	set, err2 := f.KubernetesClientSet()
	if err2 != nil {
		return nil, err2
	}
	client, err2 := f.RESTClient()
	if err2 != nil {
		return nil, err2
	}
	config, err2 := f.ToRESTConfig()
	if err2 != nil {
		return nil, err2
	}
	get, err := set.CoreV1().Pods(ns).Get(ctx, pod, v1.GetOptions{})
	if err != nil {
		return nil, err
	}
	result := map[string][]string{}
	for _, c := range get.Spec.Containers {
		env, err := Shell(set, client, config, pod, c.Name, ns, []string{"env"})
		if err != nil {
			return nil, err
		}
		split := strings.Split(env, "\n")
		result[c.Name] = split
	}
	return result, nil
}

// GetVolume key format: [container name]-[volume mount name]
func GetVolume(ctx context.Context, f util.Factory, ns, pod string) (map[string][]mount.Mount, error) {
	clientSet, err := f.KubernetesClientSet()
	if err != nil {
		return nil, err
	}
	var get *corev1.Pod
	get, err = clientSet.CoreV1().Pods(ns).Get(ctx, pod, v1.GetOptions{})
	if err != nil {
		return nil, err
	}
	result := map[string][]mount.Mount{}
	for _, c := range get.Spec.Containers {
		// if container name is vpn or envoy-proxy, not need to download volume
		if c.Name == config.ContainerSidecarVPN || c.Name == config.ContainerSidecarEnvoyProxy {
			continue
		}
		var m []mount.Mount
		for _, volumeMount := range c.VolumeMounts {
			if volumeMount.MountPath == "/tmp" {
				continue
			}
			join := filepath.Join(os.TempDir(), strconv.Itoa(rand.Int()))
			err = os.MkdirAll(join, 0755)
			if err != nil {
				return nil, err
			}
			if volumeMount.SubPath != "" {
				join = filepath.Join(join, volumeMount.SubPath)
			}
			// pod-namespace/pod-name:path
			remotePath := fmt.Sprintf("%s/%s:%s", ns, pod, volumeMount.MountPath)
			stdIn, stdOut, stdErr := term.StdStreams()
			copyOptions := cp.NewCopyOptions(genericclioptions.IOStreams{In: stdIn, Out: stdOut, ErrOut: stdErr})
			copyOptions.Container = c.Name
			copyOptions.MaxTries = 10
			err = copyOptions.Complete(f, &cobra.Command{}, []string{remotePath, join})
			if err != nil {
				return nil, err
			}
			err = copyOptions.Run()
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "failed to download volume %s path %s to %s, err: %v, ignore...\n", volumeMount.Name, remotePath, join, err)
				continue
			}
			m = append(m, mount.Mount{
				Type:   mount.TypeBind,
				Source: join,
				Target: volumeMount.MountPath,
			})
			fmt.Printf("%s:%s\n", join, volumeMount.MountPath)
		}
		result[c.Name] = m
	}
	return result, nil
}
