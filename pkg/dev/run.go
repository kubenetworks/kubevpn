package dev

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/container"
	"github.com/docker/cli/cli/command/image"
	"github.com/docker/cli/cli/streams"
	"github.com/docker/cli/cli/trust"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	typescommand "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	dockerterm "github.com/moby/term"
	v12 "github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/wencaiwulue/kubevpn/pkg/config"
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
		if err = PullImage(ctx, runConfig.platform, cli, c, config.Image); err != nil {
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
	defer func() {
		if err != nil {
			_ = cli.ContainerRemove(ctx, id, types.ContainerRemoveOptions{Force: true})
		}
	}()

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

func PullImage(ctx context.Context, platform *v12.Platform, cli *client.Client, c *command.DockerCli, img string) error {
	var readCloser io.ReadCloser
	var plat string
	if platform != nil && platform.Architecture != "" && platform.OS != "" {
		plat = fmt.Sprintf("%s/%s", platform.OS, platform.Architecture)
	}
	distributionRef, err := reference.ParseNormalizedNamed(img)
	if err != nil {
		return err
	}
	var imgRefAndAuth trust.ImageRefAndAuth
	imgRefAndAuth, err = trust.GetImageReferencesAndAuth(ctx, nil, image.AuthResolver(c), distributionRef.String())
	if err != nil {
		return err
	}
	var encodedAuth string
	encodedAuth, err = command.EncodeAuthToBase64(*imgRefAndAuth.AuthConfig())
	if err != nil {
		return err
	}
	requestPrivilege := command.RegistryAuthenticationPrivilegedFunc(c, imgRefAndAuth.RepoInfo().Index, "pull")
	readCloser, err = cli.ImagePull(ctx, img, types.ImagePullOptions{
		All:           false,
		RegistryAuth:  encodedAuth,
		PrivilegeFunc: requestPrivilege,
		Platform:      plat,
	})
	if err != nil {
		err = fmt.Errorf("can not pull image %s, err: %s, please make sure image is exist and can be pulled from local", img, err)
		return err
	}
	defer readCloser.Close()
	_, stdout, _ := dockerterm.StdStreams()
	out := streams.NewOut(stdout)
	err = jsonmessage.DisplayJSONMessagesToStream(readCloser, out, nil)
	if err != nil {
		err = fmt.Errorf("can not display message, err: %v", err)
		return err
	}
	return nil
}

func terminal(c string, cli *command.DockerCli) error {
	options := container.NewExecOptions()
	options.Interactive = true
	options.TTY = true
	options.Container = c
	options.Command = []string{"sh", "-c", `command -v bash >/dev/null && exec bash || exec sh`}
	return container.RunExec(cli, options)
}

// TransferImage
// 1) if not special ssh config, just pull image and tag and push
// 2) if special ssh config, pull image, tag image, save image and scp image to remote, load image and push
func TransferImage(ctx context.Context, conf *util.SshConfig) error {
	cli, c, err := GetClient()
	if err != nil {
		return err
	}
	// todo add flags? or detect k8s node runtime ?
	err = PullImage(ctx, &v12.Platform{
		Architecture: "amd64",
		OS:           "linux",
	}, cli, c, config.OriginImage)
	if err != nil {
		return err
	}

	err = cli.ImageTag(ctx, config.OriginImage, config.Image)
	if err != nil {
		return err
	}

	// use it if sshConfig is not empty
	if conf.ConfigAlias == "" && conf.Addr == "" {
		distributionRef, err := reference.ParseNormalizedNamed(config.Image)
		if err != nil {
			return err
		}
		var imgRefAndAuth trust.ImageRefAndAuth
		imgRefAndAuth, err = trust.GetImageReferencesAndAuth(ctx, nil, image.AuthResolver(c), distributionRef.String())
		if err != nil {
			return err
		}
		var encodedAuth string
		encodedAuth, err = command.EncodeAuthToBase64(*imgRefAndAuth.AuthConfig())
		if err != nil {
			return err
		}
		requestPrivilege := command.RegistryAuthenticationPrivilegedFunc(c, imgRefAndAuth.RepoInfo().Index, "push")
		var readCloser io.ReadCloser
		readCloser, err = cli.ImagePush(ctx, config.Image, types.ImagePushOptions{
			RegistryAuth:  encodedAuth,
			PrivilegeFunc: requestPrivilege,
		})
		if err != nil {
			err = fmt.Errorf("can not push image %s, err: %v", config.Image, err)
			return err
		}
		defer readCloser.Close()
		_, stdout, _ := dockerterm.StdStreams()
		out := streams.NewOut(stdout)
		err = jsonmessage.DisplayJSONMessagesToStream(readCloser, out, nil)
		if err != nil {
			err = fmt.Errorf("can not display message, err: %v", err)
			return err
		}
		return nil
	}

	// transfer image to remote
	var responseReader io.ReadCloser
	responseReader, err = cli.ImageSave(ctx, []string{config.Image})
	if err != nil {
		return err
	}
	defer responseReader.Close()
	file, err := os.CreateTemp("", "*.tar")
	if err != nil {
		return err
	}
	log.Infof("saving image %s to temp file %s", config.Image, file.Name())
	if _, err = io.Copy(file, responseReader); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	defer os.Remove(file.Name())

	log.Infof("Transfering image %s", config.Image)
	err = util.SCP(conf, file.Name(), []string{
		fmt.Sprintf(
			"(docker load image -i kubevpndir/%s && docker push %s) || (nerdctl image load -i kubevpndir/%s && nerdctl image push %s)",
			filepath.Base(file.Name()), config.Image,
			filepath.Base(file.Name()), config.Image,
		),
	}...)
	if err != nil {
		return err
	}
	log.Infof("Loaded image: %s", config.Image)
	return nil
}
