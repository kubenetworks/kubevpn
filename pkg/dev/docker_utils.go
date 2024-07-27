package dev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/distribution/reference"
	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	image2 "github.com/docker/cli/cli/command/image"
	"github.com/docker/cli/cli/streams"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/moby/sys/signal"
	"github.com/moby/term"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

func waitExitOrRemoved(ctx context.Context, apiClient client.APIClient, containerID string, waitRemove bool) <-chan int {
	if len(containerID) == 0 {
		// containerID can never be empty
		panic("Internal Error: waitExitOrRemoved needs a containerID as parameter")
	}

	// Older versions used the Events API, and even older versions did not
	// support server-side removal. This legacyWaitExitOrRemoved method
	// preserves that old behavior and any issues it may have.
	if versions.LessThan(apiClient.ClientVersion(), "1.30") {
		return legacyWaitExitOrRemoved(ctx, apiClient, containerID, waitRemove)
	}

	condition := container.WaitConditionNextExit
	if waitRemove {
		condition = container.WaitConditionRemoved
	}

	resultC, errC := apiClient.ContainerWait(ctx, containerID, condition)

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
			logrus.Errorf("error waiting for container: %v", err)
			statusC <- 125
		}
	}()

	return statusC
}

func legacyWaitExitOrRemoved(ctx context.Context, apiClient client.APIClient, containerID string, waitRemove bool) <-chan int {
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
	eventq, errq := apiClient.Events(eventCtx, options)

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
			} else if versions.LessThan(apiClient.ClientVersion(), "1.25") {
				// If we are talking to an older daemon, `AutoRemove` is not supported.
				// We need to fall back to the old behavior, which is client-side removal
				go func() {
					removeErr = apiClient.ContainerRemove(ctx, containerID, container.RemoveOptions{RemoveVolumes: true})
					if removeErr != nil {
						logrus.Errorf("error removing container: %v", removeErr)
						cancel() // cancel the event Q
					}
				}()
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

func runLogsWaitRunning(ctx context.Context, dockerCli command.Cli, id string) error {
	c, err := dockerCli.Client().ContainerInspect(ctx, id)
	if err != nil {
		return err
	}

	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	}
	logStream, err := dockerCli.Client().ContainerLogs(ctx, c.ID, options)
	if err != nil {
		return err
	}
	defer logStream.Close()

	buf := bytes.NewBuffer(nil)
	w := io.MultiWriter(buf, dockerCli.Out())

	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			// keyword, maybe can find another way more elegant
			if strings.Contains(buf.String(), "Now you can access resources in the kubernetes cluster, enjoy it :)") {
				cancelFunc()
				return
			}
		}
	}()

	var errChan = make(chan error)
	go func() {
		var err error
		if c.Config.Tty {
			_, err = io.Copy(w, logStream)
		} else {
			_, err = stdcopy.StdCopy(w, dockerCli.Err(), logStream)
		}
		if err != nil {
			errChan <- err
		}
	}()

	select {
	case err = <-errChan:
		return err
	case <-cancel.Done():
		return nil
	}
}

func runLogsSinceNow(dockerCli command.Cli, id string, follow bool) error {
	ctx := context.Background()

	c, err := dockerCli.Client().ContainerInspect(ctx, id)
	if err != nil {
		return err
	}

	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      "0m",
		Follow:     follow,
	}
	responseBody, err := dockerCli.Client().ContainerLogs(ctx, c.ID, options)
	if err != nil {
		return err
	}
	defer responseBody.Close()

	if c.Config.Tty {
		_, err = io.Copy(dockerCli.Out(), responseBody)
	} else {
		_, err = stdcopy.StdCopy(dockerCli.Out(), dockerCli.Err(), responseBody)
	}
	return err
}

func createNetwork(ctx context.Context, cli *client.Client) (string, error) {
	by := map[string]string{"owner": config.ConfigMapPodTrafficManager}
	list, _ := cli.NetworkList(ctx, types.NetworkListOptions{})
	for _, resource := range list {
		if reflect.DeepEqual(resource.Labels, by) {
			return resource.ID, nil
		}
	}

	create, err := cli.NetworkCreate(ctx, config.ConfigMapPodTrafficManager, types.NetworkCreate{
		Driver: "bridge",
		Scope:  "local",
		IPAM: &network.IPAM{
			Driver:  "",
			Options: nil,
			Config: []network.IPAMConfig{
				{
					Subnet:  config.DockerCIDR.String(),
					Gateway: config.DockerRouterIP.String(),
				},
			},
		},
		//Options: map[string]string{"--icc": "", "--ip-masq": ""},
		Labels: by,
	})
	if err != nil {
		if errdefs.IsForbidden(err) {
			list, _ = cli.NetworkList(ctx, types.NetworkListOptions{})
			for _, resource := range list {
				if reflect.DeepEqual(resource.Labels, by) {
					return resource.ID, nil
				}
			}
		}
		return "", err
	}
	return create.ID, nil
}

// Pull constants
const (
	PullImageAlways  = "always"
	PullImageMissing = "missing" // Default (matches previous behavior)
	PullImageNever   = "never"
)

func pullImage(ctx context.Context, dockerCli command.Cli, img string, options RunOptions) error {
	encodedAuth, err := command.RetrieveAuthTokenFromImage(dockerCli.ConfigFile(), img)
	if err != nil {
		return err
	}

	responseBody, err := dockerCli.Client().ImageCreate(ctx, img, image.CreateOptions{
		RegistryAuth: encodedAuth,
		Platform:     options.Platform,
	})
	if err != nil {
		return err
	}
	defer responseBody.Close()

	out := dockerCli.Err()
	return jsonmessage.DisplayJSONMessagesToStream(responseBody, streams.NewOut(out), nil)
}

//nolint:gocyclo
func createContainer(ctx context.Context, dockerCli command.Cli, runConfig *RunConfig) (string, error) {
	config := runConfig.config
	hostConfig := runConfig.hostConfig
	networkingConfig := runConfig.networkingConfig
	var (
		trustedRef reference.Canonical
		namedRef   reference.Named
	)

	ref, err := reference.ParseAnyReference(config.Image)
	if err != nil {
		return "", err
	}
	if named, ok := ref.(reference.Named); ok {
		namedRef = reference.TagNameOnly(named)

		if taggedRef, ok := namedRef.(reference.NamedTagged); ok && dockerCli.ContentTrustEnabled() {
			var err error
			trustedRef, err = image2.TrustedReference(ctx, dockerCli, taggedRef)
			if err != nil {
				return "", err
			}
			config.Image = reference.FamiliarString(trustedRef)
		}
	}

	pullAndTagImage := func() error {
		if err = pullImage(ctx, dockerCli, config.Image, runConfig.Options); err != nil {
			return err
		}
		if taggedRef, ok := namedRef.(reference.NamedTagged); ok && trustedRef != nil {
			return image2.TagTrusted(ctx, dockerCli, trustedRef, taggedRef)
		}
		return nil
	}

	if runConfig.Options.Pull == PullImageAlways {
		if err = pullAndTagImage(); err != nil {
			return "", err
		}
	}

	hostConfig.ConsoleSize[0], hostConfig.ConsoleSize[1] = dockerCli.Out().GetTtySize()

	response, err := dockerCli.Client().ContainerCreate(ctx, config, hostConfig, networkingConfig, runConfig.platform, runConfig.name)
	if err != nil {
		// Pull image if it does not exist locally and we have the PullImageMissing option. Default behavior.
		if errdefs.IsNotFound(err) && namedRef != nil && runConfig.Options.Pull == PullImageMissing {
			// we don't want to write to stdout anything apart from container.ID
			_, _ = fmt.Fprintf(dockerCli.Err(), "Unable to find image '%s' locally\n", reference.FamiliarString(namedRef))

			if err = pullAndTagImage(); err != nil {
				return "", err
			}

			var retryErr error
			response, retryErr = dockerCli.Client().ContainerCreate(ctx, config, hostConfig, networkingConfig, runConfig.platform, runConfig.name)
			if retryErr != nil {
				return "", retryErr
			}
		} else {
			return "", err
		}
	}

	for _, w := range response.Warnings {
		_, _ = fmt.Fprintf(dockerCli.Err(), "WARNING: %s\n", w)
	}
	return response.ID, err
}

func runContainer(ctx context.Context, dockerCli command.Cli, runConfig *RunConfig) error {
	config := runConfig.config
	stdout, stderr := dockerCli.Out(), dockerCli.Err()
	apiClient := dockerCli.Client()

	config.ArgsEscaped = false

	if err := dockerCli.In().CheckTty(config.AttachStdin, config.Tty); err != nil {
		return err
	}

	ctx, cancelFun := context.WithCancel(ctx)
	defer cancelFun()

	containerID, err := createContainer(ctx, dockerCli, runConfig)
	if err != nil {
		reportError(stderr, err.Error())
		return runStartContainerErr(err)
	}
	if runConfig.Options.SigProxy {
		sigc := notifyAllSignals()
		go ForwardAllSignals(ctx, apiClient, containerID, sigc)
		defer signal.StopCatch(sigc)
	}

	var (
		waitDisplayID chan struct{}
		errCh         chan error
	)
	if !config.AttachStdout && !config.AttachStderr {
		// Make this asynchronous to allow the client to write to stdin before having to read the ID
		waitDisplayID = make(chan struct{})
		go func() {
			defer close(waitDisplayID)
			_, _ = fmt.Fprintln(stdout, containerID)
		}()
	}
	attach := config.AttachStdin || config.AttachStdout || config.AttachStderr
	if attach {
		closeFn, err := attachContainer(ctx, dockerCli, containerID, &errCh, config, container.AttachOptions{
			Stream:     true,
			Stdin:      config.AttachStdin,
			Stdout:     config.AttachStdout,
			Stderr:     config.AttachStderr,
			DetachKeys: dockerCli.ConfigFile().DetachKeys,
		})
		if err != nil {
			return err
		}
		defer closeFn()
	}

	statusChan := waitExitOrRemoved(ctx, apiClient, containerID, runConfig.hostConfig.AutoRemove)

	// start the container
	if err := apiClient.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		// If we have hijackedIOStreamer, we should notify
		// hijackedIOStreamer we are going to exit and wait
		// to avoid the terminal are not restored.
		if attach {
			cancelFun()
			<-errCh
		}

		reportError(stderr, err.Error())
		if runConfig.hostConfig.AutoRemove {
			// wait container to be removed
			<-statusChan
		}
		return runStartContainerErr(err)
	}

	if (config.AttachStdin || config.AttachStdout || config.AttachStderr) && config.Tty && dockerCli.Out().IsTerminal() {
		if err := MonitorTtySize(ctx, dockerCli, containerID, false); err != nil {
			_, _ = fmt.Fprintln(stderr, "Error monitoring TTY size:", err)
		}
	}

	if errCh != nil {
		if err := <-errCh; err != nil {
			if _, ok := err.(term.EscapeError); ok {
				// The user entered the detach escape sequence.
				return nil
			}

			logrus.Debugf("Error hijack: %s", err)
			return err
		}
	}

	// Detached mode: wait for the id to be displayed and return.
	if !config.AttachStdout && !config.AttachStderr {
		// Detached mode
		<-waitDisplayID
		return nil
	}

	status := <-statusChan
	if status != 0 {
		return cli.StatusError{StatusCode: status}
	}
	return nil
}

func attachContainer(ctx context.Context, dockerCli command.Cli, containerID string, errCh *chan error, config *container.Config, options container.AttachOptions) (func(), error) {
	resp, errAttach := dockerCli.Client().ContainerAttach(ctx, containerID, options)
	if errAttach != nil {
		return nil, errAttach
	}

	var (
		out, cerr io.Writer
		in        io.ReadCloser
	)
	if options.Stdin {
		in = dockerCli.In()
	}
	if options.Stdout {
		out = dockerCli.Out()
	}
	if options.Stderr {
		if config.Tty {
			cerr = dockerCli.Out()
		} else {
			cerr = dockerCli.Err()
		}
	}

	ch := make(chan error, 1)
	*errCh = ch

	go func() {
		ch <- func() error {
			streamer := hijackedIOStreamer{
				streams:      dockerCli,
				inputStream:  in,
				outputStream: out,
				errorStream:  cerr,
				resp:         resp,
				tty:          config.Tty,
				detachKeys:   options.DetachKeys,
			}

			if errHijack := streamer.stream(ctx); errHijack != nil {
				return errHijack
			}
			return errAttach
		}()
	}()
	return resp.Close, nil
}

// reportError is a utility method that prints a user-friendly message
// containing the error that occurred during parsing and a suggestion to get help
func reportError(stderr io.Writer, str string) {
	str = strings.TrimSuffix(str, ".") + "."
	_, _ = fmt.Fprintln(stderr, "docker:", str)
}

// if container start fails with 'not found'/'no such' error, return 127
// if container start fails with 'permission denied' error, return 126
// return 125 for generic docker daemon failures
func runStartContainerErr(err error) error {
	trimmedErr := strings.TrimPrefix(err.Error(), "Error response from daemon: ")
	statusError := cli.StatusError{StatusCode: 125, Status: trimmedErr}
	if strings.Contains(trimmedErr, "executable file not found") ||
		strings.Contains(trimmedErr, "no such file or directory") ||
		strings.Contains(trimmedErr, "system cannot find the file specified") {
		statusError = cli.StatusError{StatusCode: 127, Status: trimmedErr}
	} else if strings.Contains(trimmedErr, syscall.EACCES.Error()) ||
		strings.Contains(trimmedErr, syscall.EISDIR.Error()) {
		statusError = cli.StatusError{StatusCode: 126, Status: trimmedErr}
	}

	return statusError
}

func run(ctx context.Context, cli *client.Client, dockerCli *command.DockerCli, runConfig *RunConfig) (id string, err error) {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	var config = runConfig.config
	var hostConfig = runConfig.hostConfig
	var platform = runConfig.platform
	var networkConfig = runConfig.networkingConfig
	var name = runConfig.name

	var needPull bool
	var img types.ImageInspect
	img, _, err = cli.ImageInspectWithRaw(ctx, config.Image)
	if errdefs.IsNotFound(err) {
		logrus.Infof("needs to pull image %s", config.Image)
		needPull = true
		err = nil
	} else if err != nil {
		logrus.Errorf("image inspect failed: %v", err)
		return
	}
	if platform != nil && platform.Architecture != "" && platform.OS != "" {
		if img.Os != platform.OS || img.Architecture != platform.Architecture {
			needPull = true
		}
	}
	if needPull {
		err = pkgssh.PullImage(ctx, runConfig.platform, cli, dockerCli, config.Image, nil)
		if err != nil {
			logrus.Errorf("Failed to pull image: %s, err: %s", config.Image, err)
			return
		}
	}

	var create container.CreateResponse
	create, err = cli.ContainerCreate(ctx, config, hostConfig, networkConfig, platform, name)
	if err != nil {
		logrus.Errorf("Failed to create container: %s, err: %s", name, err)
		return
	}
	id = create.ID
	logrus.Infof("Created container: %s", name)
	err = cli.ContainerStart(ctx, create.ID, container.StartOptions{})
	if err != nil {
		logrus.Errorf("failed to startup container %s: %v", name, err)
		return
	}
	logrus.Infof("Wait container %s to be running...", name)
	var inspect types.ContainerJSON
	ctx2, cancelFunc := context.WithCancel(ctx)
	wait.UntilWithContext(ctx2, func(ctx context.Context) {
		inspect, err = cli.ContainerInspect(ctx, create.ID)
		if errdefs.IsNotFound(err) {
			cancelFunc()
			return
		} else if err != nil {
			cancelFunc()
			return
		}
		if inspect.State != nil && (inspect.State.Status == "exited" || inspect.State.Status == "dead" || inspect.State.Dead) {
			cancelFunc()
			err = errors.New(fmt.Sprintf("container status: %s", inspect.State.Status))
			return
		}
		if inspect.State != nil && inspect.State.Running {
			cancelFunc()
			return
		}
	}, time.Second)
	if err != nil {
		logrus.Errorf("failed to wait container to be ready: %v", err)
		_ = runLogsSinceNow(dockerCli, id, false)
		return
	}

	logrus.Infof("Container %s is running now", name)
	return
}
