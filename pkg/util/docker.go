package util

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func RunLogsWaitRunning(ctx context.Context, name string) error {
	buf := bytes.NewBuffer(nil)
	w := io.MultiWriter(buf, os.Stdout)

	args := []string{"logs", name, "--since", "0m", "--details", "--follow"}
	cmd := exec.Command("docker", args...)
	cmd.Stdout = w
	go cmd.Start()

	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			// keyword, maybe can find another way more elegant
			if strings.Contains(buf.String(), config.Slogan) {
				cancelFunc()
				return
			}
		}
	}()

	var errChan = make(chan error)
	go func() {
		var err error
		_, err = stdcopy.StdCopy(w, os.Stdout, buf)
		if err != nil {
			errChan <- err
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-cancel.Done():
		return nil
	}
}

func RunLogsSinceNow(name string, follow bool) error {
	args := []string{"logs", name, "--since", "0m", "--details"}
	if follow {
		args = append(args, "--follow")
	}
	output, err := exec.Command("docker", args...).CombinedOutput()
	_, err = stdcopy.StdCopy(os.Stdout, os.Stderr, bytes.NewReader(output))
	return err
}

// CreateNetwork
// docker create kubevpn-traffic-manager --labels owner=config.ConfigMapPodTrafficManager --subnet 198.18.0.0/16 --gateway 198.18.0.100
func CreateNetwork(ctx context.Context, name string) (string, error) {
	args := []string{
		"network",
		"inspect",
		name,
	}
	_, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err == nil {
		return name, nil
	}

	args = []string{
		"network",
		"create",
		name,
		"--label", "owner=" + name,
		"--subnet", config.DockerCIDR.String(),
		"--gateway", config.DockerRouterIP.String(),
		"--driver", "bridge",
		"--scope", "local",
	}

	id, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", err
	}

	return string(id), nil
}

func RunContainer(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.G(ctx).Debugf("Run container with cmd: %v", cmd.Args)
	err := cmd.Start()
	if err != nil {
		log.G(ctx).Errorf("Failed to run container with cmd: %v: %v", cmd.Args, err)
		return err
	}
	return cmd.Wait()
}

func WaitDockerContainerRunning(ctx context.Context, name string) error {
	log.G(ctx).Infof("Wait container %s to be running...", name)

	for ctx.Err() == nil {
		time.Sleep(time.Second * 1)
		inspect, err := ContainerInspect(ctx, name)
		if err != nil {
			return err
		}
		if inspect.State != nil && (inspect.State.Status == "exited" || inspect.State.Status == "dead" || inspect.State.Dead) {
			err = errors.New(fmt.Sprintf("container status: %s", inspect.State.Status))
			break
		}
		if inspect.State != nil && inspect.State.Running {
			break
		}
	}

	log.G(ctx).Infof("Container %s is running now", name)
	return nil
}

func ContainerInspect(ctx context.Context, name string) (types.ContainerJSON, error) {
	output, err := exec.CommandContext(ctx, "docker", "inspect", name).CombinedOutput()
	if err != nil {
		_ = RunLogsSinceNow(name, false)
		return types.ContainerJSON{}, err
	}
	var inspect []types.ContainerJSON
	rdr := bytes.NewReader(output)
	err = json.NewDecoder(rdr).Decode(&inspect)
	if err != nil {
		return types.ContainerJSON{}, err
	}
	if len(inspect) == 0 {
		return types.ContainerJSON{}, err
	}
	return inspect[0], nil
}

func NetworkInspect(ctx context.Context, name string) (network.Inspect, error) {
	output, err := exec.CommandContext(ctx, "docker", "network", "inspect", name).CombinedOutput()
	if err != nil {
		_ = RunLogsSinceNow(name, false)
		return network.Inspect{}, err
	}
	var inspect []network.Inspect
	rdr := bytes.NewReader(output)
	err = json.NewDecoder(rdr).Decode(&inspect)
	if err != nil {
		return network.Inspect{}, err
	}
	if len(inspect) == 0 {
		return network.Inspect{}, err
	}
	return inspect[0], nil
}

func NetworkRemove(ctx context.Context, name string) error {
	output, err := exec.CommandContext(ctx, "docker", "network", "remove", name).CombinedOutput()
	if err != nil && strings.Contains(string(output), "not found") {
		return nil
	}
	return err
}

// NetworkDisconnect
// docker network disconnect --force
func NetworkDisconnect(ctx context.Context, containerName string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "docker", "network", "disconnect", "--force", config.ConfigMapPodTrafficManager, containerName).CombinedOutput()
	if err != nil && strings.Contains(string(output), "not found") {
		return output, nil
	}
	return output, err
}

// ContainerRemove
// docker remove --force
func ContainerRemove(ctx context.Context, containerName string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "docker", "remove", "--force", containerName).CombinedOutput()
	if err != nil && strings.Contains(string(output), "not found") {
		return output, nil
	}
	return output, err
}

func ContainerKill(ctx context.Context, name *string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "docker", "kill", *name, "--signal", "SIGTERM").CombinedOutput()
	if err != nil && strings.Contains(string(output), "not found") {
		return output, nil
	}
	return output, err
}
