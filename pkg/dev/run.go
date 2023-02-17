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

func run(ctx context.Context, runConfig *RunConfig, cli *client.Client) (err error) {

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
		if runConfig.platform.Architecture != "" && runConfig.platform.OS != "" {
			plat = fmt.Sprintf("%s/%s", runConfig.platform.OS, runConfig.platform.Architecture)
		}
		readCloser, err = cli.ImagePull(ctx, config.Image, types.ImagePullOptions{Platform: plat})
		if err != nil {
			return err
		}
		defer readCloser.Close()
		_, stdout, _ := dockerterm.StdStreams()
		out := streams.NewOut(stdout)
		err = jsonmessage.DisplayJSONMessagesToStream(readCloser, out, nil)
		if err != nil {
			return err
		}
	}

	var create typescommand.CreateResponse
	create, err = cli.ContainerCreate(ctx, config, hostConfig, networkConfig, platform, name)
	if err != nil {
		return err
	}

	log.Infof("Created container: %s", name)
	if err != nil {
		return err
	}

	err = cli.ContainerStart(ctx, create.ID, types.ContainerStartOptions{})
	if err != nil {
		return err
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
	return nil
}

func terminal(c string, cli *command.DockerCli) error {
	options := container.NewExecOptions()
	options.Interactive = true
	options.TTY = true
	options.Container = c
	options.Command = []string{"/bin/sh"}
	return container.RunExec(cli, options)
}
