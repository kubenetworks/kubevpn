package util

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
	"github.com/moby/term"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/api/core/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/cp"
)

// GetVolume key format: [container name]-[volume mount name]
func GetVolume(ctx context.Context, f util.Factory, ns, podName string) (map[string][]mount.Mount, error) {
	clientSet, err := f.KubernetesClientSet()
	if err != nil {
		return nil, err
	}
	var pod *v1.Pod
	pod, err = clientSet.CoreV1().Pods(ns).Get(ctx, podName, v12.GetOptions{})
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
				_, _ = fmt.Fprintf(os.Stderr, "failed to download volume %s path %s to %s, err: %v, ignore...\n", volumeMount.Name, remotePath, localPath, err)
				continue
			}
			m = append(m, mount.Mount{
				Type:   mount.TypeBind,
				Source: localPath,
				Target: volumeMount.MountPath,
			})
			logrus.Infof("%s:%s", localPath, volumeMount.MountPath)
		}
		result[container.Name] = m
	}
	return result, nil
}

func RemoveDir(volume map[string][]mount.Mount) error {
	var errs []error
	for _, mounts := range volume {
		for _, m := range mounts {
			err := os.RemoveAll(m.Source)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to delete dir %s: %v", m.Source, err))
			}
		}
	}
	return errors.NewAggregate(errs)
}

func CopyVolumeIntoContainer(ctx context.Context, volume []mount.Mount, cli *client.Client, id string) error {
	// copy volume into container
	for _, v := range volume {
		target, err := CreateFolder(ctx, cli, id, v.Source, v.Target)
		if err != nil {
			logrus.Debugf("create folder %s previoully failed, err: %v", target, err)
		}
		logrus.Debugf("from %s to %s", v.Source, v.Target)
		srcInfo, err := archive.CopyInfoSourcePath(v.Source, true)
		if err != nil {
			logrus.Errorf("copy info source path, err: %v", err)
			return err
		}
		srcArchive, err := archive.TarResource(srcInfo)
		if err != nil {
			logrus.Errorf("tar resource failed, err: %v", err)
			return err
		}
		dstDir, preparedArchive, err := archive.PrepareArchiveCopy(srcArchive, srcInfo, archive.CopyInfo{Path: v.Target})
		if err != nil {
			logrus.Errorf("can not prepare archive copy, err: %v", err)
			return err
		}

		err = cli.CopyToContainer(ctx, id, dstDir, preparedArchive, types.CopyToContainerOptions{
			AllowOverwriteDirWithFile: true,
			CopyUIDGID:                true,
		})
		if err != nil {
			logrus.Infof("can not copy %s to container %s:%s, err: %v", v.Source, id, v.Target, err)
			return err
		}
	}
	return nil
}

func CreateFolder(ctx context.Context, cli *client.Client, id string, src string, target string) (string, error) {
	lstat, err := os.Lstat(src)
	if err != nil {
		return "", err
	}
	if !lstat.IsDir() {
		target = filepath.Dir(target)
	}
	var create types.IDResponse
	create, err = cli.ContainerExecCreate(ctx, id, types.ExecConfig{
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          []string{"mkdir", "-p", target},
	})
	if err != nil {
		logrus.Errorf("create folder %s previoully failed, err: %v", target, err)
		return "", err
	}
	err = cli.ContainerExecStart(ctx, create.ID, types.ExecStartCheck{})
	if err != nil {
		logrus.Errorf("create folder %s previoully failed, err: %v", target, err)
		return "", err
	}
	logrus.Infof("wait create folder %s in container %s to be done...", target, id)
	chanStop := make(chan struct{})
	wait.Until(func() {
		inspect, err := cli.ContainerExecInspect(ctx, create.ID)
		if err != nil {
			logrus.Warningf("can not inspect container %s, err: %v", id, err)
			return
		}
		if !inspect.Running {
			close(chanStop)
		}
	}, time.Second, chanStop)
	return target, nil
}
