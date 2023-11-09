package dev

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/versions"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

func waitExitOrRemoved(ctx context.Context, dockerCli command.Cli, containerID string, waitRemove bool) <-chan int {
	if len(containerID) == 0 {
		// containerID can never be empty
		panic("Internal Error: waitExitOrRemoved needs a containerID as parameter")
	}

	// Older versions used the Events API, and even older versions did not
	// support server-side removal. This legacyWaitExitOrRemoved method
	// preserves that old behavior and any issues it may have.
	if versions.LessThan(dockerCli.Client().ClientVersion(), "1.30") {
		return legacyWaitExitOrRemoved(ctx, dockerCli, containerID, waitRemove)
	}

	condition := container.WaitConditionNextExit
	if waitRemove {
		condition = container.WaitConditionRemoved
	}

	resultC, errC := dockerCli.Client().ContainerWait(ctx, containerID, condition)

	statusC := make(chan int)
	go func() {
		select {
		case result := <-resultC:
			if result.Error != nil {
				logrus.Errorf("Error waiting for container: %v", result.Error.Message)
				statusC <- 125
			} else {
				statusC <- int(result.StatusCode)
			}
		case err := <-errC:
			if err != nil && err.Error() != "" {
				logrus.Errorf("error waiting for container: %v", err)
			}
			statusC <- 125
		}
	}()

	return statusC
}

func legacyWaitExitOrRemoved(ctx context.Context, dockerCli command.Cli, containerID string, waitRemove bool) <-chan int {
	var removeErr error
	statusChan := make(chan int)
	exitCode := 125

	// Get events via Events API
	f := filters.NewArgs()
	f.Add("type", "container")
	f.Add("container", containerID)
	options := types.EventsOptions{
		Filters: f,
	}
	eventCtx, cancel := context.WithCancel(ctx)
	eventq, errq := dockerCli.Client().Events(eventCtx, options)

	eventProcessor := func(e events.Message) bool {
		stopProcessing := false
		switch e.Status {
		case "die":
			if v, ok := e.Actor.Attributes["exitCode"]; ok {
				code, cerr := strconv.Atoi(v)
				if cerr != nil {
					logrus.Errorf("failed to convert exitcode '%q' to int: %v", v, cerr)
				} else {
					exitCode = code
				}
			}
			if !waitRemove {
				stopProcessing = true
			} else {
				// If we are talking to an older daemon, `AutoRemove` is not supported.
				// We need to fall back to the old behavior, which is client-side removal
				if versions.LessThan(dockerCli.Client().ClientVersion(), "1.25") {
					go func() {
						removeErr = dockerCli.Client().ContainerRemove(ctx, containerID, types.ContainerRemoveOptions{RemoveVolumes: true})
						if removeErr != nil {
							logrus.Errorf("error removing container: %v", removeErr)
							cancel() // cancel the event Q
						}
					}()
				}
			}
		case "detach":
			exitCode = 0
			stopProcessing = true
		case "destroy":
			stopProcessing = true
		}
		return stopProcessing
	}

	go func() {
		defer func() {
			statusChan <- exitCode // must always send an exit code or the caller will block
			cancel()
		}()

		for {
			select {
			case <-eventCtx.Done():
				if removeErr != nil {
					return
				}
			case evt := <-eventq:
				if eventProcessor(evt) {
					return
				}
			case err := <-errq:
				logrus.Errorf("error getting events from daemon: %v", err)
				return
			}
		}
	}()

	return statusChan
}

func parallelOperation(ctx context.Context, containers []string, op func(ctx context.Context, container string) error) chan error {
	if len(containers) == 0 {
		return nil
	}
	const defaultParallel int = 50
	sem := make(chan struct{}, defaultParallel)
	errChan := make(chan error)

	// make sure result is printed in correct order
	output := map[string]chan error{}
	for _, c := range containers {
		output[c] = make(chan error, 1)
	}
	go func() {
		for _, c := range containers {
			err := <-output[c]
			errChan <- err
		}
	}()

	go func() {
		for _, c := range containers {
			sem <- struct{}{} // Wait for active queue sem to drain.
			go func(container string) {
				output[container] <- op(ctx, container)
				<-sem
			}(c)
		}
	}()
	return errChan
}

func ConvertHost(kubeconfigPath string) (newPath string, err error) {
	var kubeconfigBytes []byte
	kubeconfigBytes, err = os.ReadFile(kubeconfigPath)
	if err != nil {
		err = errors.Wrap(err, "Failed to read kubeconfig file.")
		return
	}
	var conf clientcmd.ClientConfig
	conf, err = clientcmd.NewClientConfigFromBytes(kubeconfigBytes)
	if err != nil {
		err = errors.Wrap(err, "Failed to create new client configuration.")
		return
	}
	var rawConfig api.Config
	rawConfig, err = conf.RawConfig()
	if err != nil {
		err = errors.Wrap(err, "Failed to get raw configuration.")
		return
	}
	if err = api.FlattenConfig(&rawConfig); err != nil {
		return
	}
	if rawConfig.Contexts == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	cluster := rawConfig.Clusters[kubeContext.Cluster]
	if cluster == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	var u *url.URL
	u, err = url.Parse(cluster.Server)
	if err != nil {
		err = errors.Wrap(err, "Failed to parse URL.")
		return
	}
	var remote netip.AddrPort
	remote, err = netip.ParseAddrPort(u.Host)
	if err != nil {
		err = errors.Wrap(err, "Failed to parse address port.")
		return
	}
	host := fmt.Sprintf("%s://%s", u.Scheme, net.JoinHostPort("kubernetes", strconv.Itoa(int(remote.Port()))))
	rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].Server = host
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: clientcmdlatest.Version, Kind: "Config"})

	var convertedObj runtime.Object
	convertedObj, err = clientcmdlatest.Scheme.ConvertToVersion(&rawConfig, clientcmdlatest.ExternalVersion)
	if err != nil {
		err = errors.Wrap(err, "Failed to convert to version.")
		return
	}
	var marshal []byte
	marshal, err = json.Marshal(convertedObj)
	if err != nil {
		err = errors.Wrap(err, "Failed to marshal JSON.")
		return
	}
	var temp *os.File
	temp, err = os.CreateTemp("", "*.kubeconfig")
	if err != nil {
		err = errors.Wrap(err, "Failed to create temporary file.")
		return
	}
	if err = temp.Close(); err != nil {
		return
	}
	err = os.WriteFile(temp.Name(), marshal, 0644)
	if err != nil {
		err = errors.Wrap(err, "Failed to create temporary file.")
		return
	}
	newPath = temp.Name()
	return
}
