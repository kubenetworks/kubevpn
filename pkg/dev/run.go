package dev

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/container"
	"github.com/docker/cli/cli/streams"
	"github.com/docker/docker/api/types"
	typescommand "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	dockerterm "github.com/moby/term"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"
)

func run(ctx context.Context, runConfig *RunConfig, cli *client.Client) (id string, err error) {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	var config = runConfig.config
	var hostConfig = runConfig.hostConfig
	var platform = runConfig.platform
	var networkConfig = runConfig.networkingConfig
	var name = runConfig.containerName

	var needPull bool
	var img types.ImageInspect
	img, _, err = cli.ImageInspectWithRaw(ctx, config.Image)
	if err != nil {
		needPull = true
		err = nil
	}
	if platform != nil && platform.Architecture != "" && platform.OS != "" {
		if img.Os != platform.OS || img.Architecture != platform.Architecture {
			needPull = true
		}
	}
	if needPull {
		var readCloser io.ReadCloser
		var plat string
		if runConfig.platform != nil && runConfig.platform.Architecture != "" && runConfig.platform.OS != "" {
			plat = fmt.Sprintf("%s/%s", runConfig.platform.OS, runConfig.platform.Architecture)
		}
		readCloser, err = cli.ImagePull(ctx, config.Image, types.ImagePullOptions{Platform: plat})
		if err != nil {
			err = fmt.Errorf("can not pull image %s, err: %s, please make sure image is exist and can be pulled from local", config.Image, err)
			return
		}
		defer readCloser.Close()
		_, stdout, _ := dockerterm.StdStreams()
		out := streams.NewOut(stdout)
		err = jsonmessage.DisplayJSONMessagesToStream(readCloser, out, nil)
		if err != nil {
			err = fmt.Errorf("can not display message, err: %v", err)
			return
		}
	}

	var create typescommand.CreateResponse
	create, err = cli.ContainerCreate(ctx, config, hostConfig, networkConfig, platform, name)
	if err != nil {
		err = fmt.Errorf("failed to create container %s, err: %s", name, err)
		return
	}
	id = create.ID
	log.Infof("Created container: %s", name)

	err = cli.ContainerStart(ctx, create.ID, types.ContainerStartOptions{})
	if err != nil {
		return
	}
	log.Infof("Wait container %s to be running...", name)
	chanStop := make(chan struct{})
	var inspect types.ContainerJSON
	wait.Until(func() {
		inspect, err = cli.ContainerInspect(ctx, create.ID)
		if err != nil {
			return
		}
		if inspect.State != nil && inspect.State.Running {
			close(chanStop)
		}
	}, time.Second, chanStop)

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

func terminal(c string, cli *command.DockerCli) error {
	options := container.NewExecOptions()
	options.Interactive = true
	options.TTY = true
	options.Container = c
	options.Command = []string{"sh", "-c", `command -v bash >/dev/null && exec bash || exec sh`}
	return container.RunExec(cli, options)
}
