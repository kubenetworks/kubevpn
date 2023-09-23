package dev

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/container"
	"github.com/docker/docker/api/types"
	typescommand "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	apiclient "github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/moby/term"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func run(ctx context.Context, runConfig *RunConfig, cli *client.Client, c *command.DockerCli) (id string, err error) {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	var config = runConfig.config
	var hostConfig = runConfig.hostConfig
	var platform = runConfig.platform
	var networkConfig = runConfig.networkingConfig
	var name = runConfig.containerName

	var needPull bool
	var img types.ImageInspect
	img, _, err = cli.ImageInspectWithRaw(ctx, config.Image)
	if errdefs.IsNotFound(err) {
		needPull = true
		err = nil
	} else if err != nil {
		log.Errorf("image inspect failed: %v", err)
		return
	}
	if platform != nil && platform.Architecture != "" && platform.OS != "" {
		if img.Os != platform.OS || img.Architecture != platform.Architecture {
			needPull = true
		}
	}
	if needPull {
		err = util.PullImage(ctx, runConfig.platform, cli, c, config.Image, nil)
		if err != nil {
			log.Errorf("Failed to pull image: %s, err: %s", config.Image, err)
			return
		}
	}

	var create typescommand.CreateResponse
	create, err = cli.ContainerCreate(ctx, config, hostConfig, networkConfig, platform, name)
	if err != nil {
		log.Errorf("Failed to create container: %s, err: %s", name, err)
		return
	}
	id = create.ID
	log.Infof("Created container: %s", name)
	defer func() {
		if err != nil && runConfig.hostConfig.AutoRemove {
			_ = cli.ContainerRemove(ctx, id, types.ContainerRemoveOptions{Force: true})
		}
	}()

	err = cli.ContainerStart(ctx, create.ID, types.ContainerStartOptions{})
	if err != nil {
		log.Errorf("failed to startup container %s: %v", name, err)
		return
	}
	log.Infof("Wait container %s to be running...", name)
	var inspect types.ContainerJSON
	cancel, cancelFunc := context.WithCancel(ctx)
	wait.UntilWithContext(cancel, func(ctx2 context.Context) {
		inspect, err = cli.ContainerInspect(ctx2, create.ID)
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
		log.Errorf("failed to wait container to be ready: %v", err)
		_ = runLogsSinceNow(c, id, false)
		return
	}

	// print port mapping to host
	var empty = true
	var str string
	if inspect.NetworkSettings != nil && inspect.NetworkSettings.Ports != nil {
		var list []string
		for port, bindings := range inspect.NetworkSettings.Ports {
			var p []string
			for _, binding := range bindings {
				if binding.HostPort != "" {
					p = append(p, binding.HostPort)
					empty = false
				}
			}
			list = append(list, fmt.Sprintf("%s:%s", port, strings.Join(p, ",")))
		}
		str = fmt.Sprintf("Container %s is running on port %s now", name, strings.Join(list, " "))
	}
	if !empty {
		log.Infoln(str)
	} else {
		log.Infof("Container %s is running now", name)
	}
	return
}

func runFirst(ctx context.Context, runConfig *RunConfig, cli *apiclient.Client, dockerCli *command.DockerCli) (id string, err error) {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	defer func() {
		if err != nil && runConfig.hostConfig.AutoRemove {
			_ = cli.ContainerRemove(ctx, id, types.ContainerRemoveOptions{Force: true})
		}
	}()

	stdout, stderr := dockerCli.Out(), dockerCli.Err()
	client := dockerCli.Client()

	runConfig.config.ArgsEscaped = false

	if err := dockerCli.In().CheckTty(runConfig.config.AttachStdin, runConfig.config.Tty); err != nil {
		return id, err
	}

	if !runConfig.Options.Detach {
		if err := dockerCli.In().CheckTty(runConfig.config.AttachStdin, runConfig.config.Tty); err != nil {
			return id, err
		}
	} else {
		if runConfig.Copts.attach.Len() != 0 {
			return id, errors.New("Conflicting options: -a and -d")
		}

		runConfig.config.AttachStdin = false
		runConfig.config.AttachStdout = false
		runConfig.config.AttachStderr = false
		runConfig.config.StdinOnce = false
	}

	ctx, cancelFun := context.WithCancel(context.Background())
	defer cancelFun()

	createResponse, err := createContainer(ctx, dockerCli, &containerConfig{
		Config:           runConfig.config,
		HostConfig:       runConfig.hostConfig,
		NetworkingConfig: runConfig.networkingConfig,
	}, &runConfig.Options.createOptions)
	if err != nil {
		log.Errorf("Failed to create main container: %s", err)
		return "", err
	}
	log.Infof("Created main container: %s", runConfig.containerName)

	var (
		waitDisplayID chan struct{}
		errCh         chan error
	)
	if !runConfig.config.AttachStdout && !runConfig.config.AttachStderr {
		// Make this asynchronous to allow the client to write to stdin before having to read the ID
		waitDisplayID = make(chan struct{})
		go func() {
			defer close(waitDisplayID)
			fmt.Fprintln(stdout, createResponse.ID)
		}()
	}
	attach := runConfig.config.AttachStdin || runConfig.config.AttachStdout || runConfig.config.AttachStderr
	if attach {
		close, err := attachContainer(ctx, dockerCli, &errCh, runConfig.config, createResponse.ID)

		if err != nil {
			return id, err
		}
		defer close()
	}
	statusChan := waitExitOrRemoved(ctx, dockerCli, createResponse.ID, runConfig.Copts.autoRemove)
	// start the container
	if err := client.ContainerStart(ctx, createResponse.ID, types.ContainerStartOptions{}); err != nil {
		// If we have hijackedIOStreamer, we should notify
		// hijackedIOStreamer we are going to exit and wait
		// to avoid the terminal are not restored.
		if attach {
			cancelFun()
			<-errCh
		}

		reportError(stderr, "run", err.Error(), false)
		if runConfig.Copts.autoRemove {
			// wait container to be removed
			<-statusChan
		}
		return id, runStartContainerErr(err)
	}

	if (runConfig.config.AttachStdin || runConfig.config.AttachStdout || runConfig.config.AttachStderr) && runConfig.config.Tty && dockerCli.Out().IsTerminal() {
		if err := container.MonitorTtySize(ctx, dockerCli, createResponse.ID, false); err != nil {
			fmt.Fprintln(stderr, "Error monitoring TTY size:", err)
		}
	}

	if errCh != nil {
		if err := <-errCh; err != nil {
			if _, ok := err.(term.EscapeError); ok {
				// The user entered the detach escape sequence.
				return id, nil
			}

			logrus.Debugf("Error hijack: %s", err)
			return id, err
		}
	}

	// Detached mode: wait for the id to be displayed and return.
	if !runConfig.config.AttachStdout && !runConfig.config.AttachStderr {
		// Detached mode
		<-waitDisplayID
		return id, nil
	}

	status := <-statusChan
	if status != 0 {
		return id, errors.New(strconv.Itoa(status))
	}
	log.Infof("Wait container %s to be running...", runConfig.containerName)
	chanStop := make(chan struct{})
	var inspect types.ContainerJSON
	var once = &sync.Once{}
	wait.Until(func() {
		inspect, err = cli.ContainerInspect(ctx, createResponse.ID)
		if err != nil && errdefs.IsNotFound(err) {
			once.Do(func() { close(chanStop) })
			return
		}
		if err != nil {
			log.Errorf("Error inspect container: %s", err)
			return
		}
		if inspect.State != nil && (inspect.State.Status == "exited" || inspect.State.Status == "dead" || inspect.State.Dead) {
			once.Do(func() { close(chanStop) })
			err = errors.New(fmt.Sprintf("container status: %s", inspect.State.Status))
			return
		}
		if inspect.State != nil && inspect.State.Running {
			once.Do(func() { close(chanStop) })
			return
		}
	}, time.Second, chanStop)
	if err != nil {
		log.Errorf("wait container to be ready: %v", err)
		err = fmt.Errorf("failed to wait container to be ready: %v", err)
		return
	}

	// print port mapping to host
	var empty = true
	var str string
	if inspect.NetworkSettings != nil && inspect.NetworkSettings.Ports != nil {
		var list []string
		for port, bindings := range inspect.NetworkSettings.Ports {
			var p []string
			for _, binding := range bindings {
				if binding.HostPort != "" {
					p = append(p, binding.HostPort)
					empty = false
				}
			}
			list = append(list, fmt.Sprintf("%s:%s", port, strings.Join(p, ",")))
		}
		str = fmt.Sprintf("Container %s is running on port %s now", runConfig.containerName, strings.Join(list, " "))
	}
	if !empty {
		log.Infoln(str)
	} else {
		log.Infof("Container %s is running now", runConfig.containerName)
	}

	return
}

func terminal(c string, cli *command.DockerCli) error {
	options := container.NewExecOptions()
	options.Interactive = true
	options.TTY = true
	options.Container = c
	options.Command = []string{"sh", "-c", `command -v bash >/dev/null && exec bash || exec sh`}
	return container.RunExec(cli, options)
}
