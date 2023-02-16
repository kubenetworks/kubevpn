package dev

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/container"
	"github.com/docker/cli/cli/streams"
	"github.com/docker/docker/api/types"
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
	img, _, err := cli.ImageInspectWithRaw(ctx, config.Image)
	if err != nil {
		needPull = true
		err = nil
	}
	if platform.Architecture != "" && platform.OS != "" {
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

	create, err := cli.ContainerCreate(ctx, config, hostConfig, networkConfig, platform, name)
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
	wait.Until(func() {
		inspect, err2 := cli.ContainerInspect(ctx, create.ID)
		if err2 != nil {
			return
		}
		if inspect.State != nil && inspect.State.Running {
			close(chanStop)
		}
	}, time.Second, chanStop)
	log.Infof("Container %s is running now", name)
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
