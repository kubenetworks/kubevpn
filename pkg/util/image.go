package util

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/image"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/cli/cli/streams"
	"github.com/docker/cli/cli/trust"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/moby/term"
	"github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

func GetClient() (*client.Client, *command.DockerCli, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("can not create docker client from env, err: %v", err)
	}
	var dockerCli *command.DockerCli
	dockerCli, err = command.NewDockerCli(command.WithAPIClient(cli))
	if err != nil {
		return nil, nil, fmt.Errorf("can not create docker client from env, err: %v", err)
	}
	err = dockerCli.Initialize(flags.NewClientOptions())
	if err != nil {
		return nil, nil, fmt.Errorf("can not init docker client, err: %v", err)
	}
	return cli, dockerCli, nil
}

// TransferImage
// 1) if not special ssh config, just pull image and tag and push
// 2) if special ssh config, pull image, tag image, save image and scp image to remote, load image and push
func TransferImage(ctx context.Context, conf *SshConfig, from, to string) error {
	cli, c, err := GetClient()
	if err != nil {
		return fmt.Errorf("failed to get docker client: %v", err)
	}
	// todo add flags? or detect k8s node runtime ?
	err = PullImage(ctx, &v1.Platform{
		Architecture: "amd64",
		OS:           "linux",
	}, cli, c, from)
	if err != nil {
		return fmt.Errorf("failed to pull image: %v", err)
	}

	err = cli.ImageTag(ctx, from, to)
	if err != nil {
		return fmt.Errorf("failed to tag image %s to %s: %v", from, to, err)
	}

	// use it if sshConfig is not empty
	if conf.ConfigAlias == "" && conf.Addr == "" {
		var distributionRef reference.Named
		distributionRef, err = reference.ParseNormalizedNamed(to)
		if err != nil {
			return fmt.Errorf("can not parse image name %s: %v", to, err)
		}
		var imgRefAndAuth trust.ImageRefAndAuth
		imgRefAndAuth, err = trust.GetImageReferencesAndAuth(ctx, nil, image.AuthResolver(c), distributionRef.String())
		if err != nil {
			return fmt.Errorf("can not get image auth: %v", err)
		}
		var encodedAuth string
		encodedAuth, err = command.EncodeAuthToBase64(*imgRefAndAuth.AuthConfig())
		if err != nil {
			return fmt.Errorf("can not encode auth config to base64: %v", err)
		}
		requestPrivilege := command.RegistryAuthenticationPrivilegedFunc(c, imgRefAndAuth.RepoInfo().Index, "push")
		var readCloser io.ReadCloser
		readCloser, err = cli.ImagePush(ctx, to, types.ImagePushOptions{
			RegistryAuth:  encodedAuth,
			PrivilegeFunc: requestPrivilege,
		})
		if err != nil {
			err = fmt.Errorf("can not push image %s, err: %v", to, err)
			return err
		}
		defer readCloser.Close()
		_, stdout, _ := term.StdStreams()
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
	responseReader, err = cli.ImageSave(ctx, []string{to})
	if err != nil {
		return err
	}
	defer responseReader.Close()
	file, err := os.CreateTemp("", "*.tar")
	if err != nil {
		return err
	}
	logrus.Infof("saving image %s to temp file %s", to, file.Name())
	if _, err = io.Copy(file, responseReader); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	defer os.Remove(file.Name())

	logrus.Infof("Transfering image %s", to)
	err = SCP(conf, file.Name(), []string{
		fmt.Sprintf(
			"(docker load image -i kubevpndir/%s && docker push %s) || (nerdctl image load -i kubevpndir/%s && nerdctl image push %s)",
			filepath.Base(file.Name()), to,
			filepath.Base(file.Name()), to,
		),
	}...)
	if err != nil {
		return err
	}
	logrus.Infof("Loaded image: %s", to)
	return nil
}

func PullImage(ctx context.Context, platform *v1.Platform, cli *client.Client, c *command.DockerCli, img string) error {
	var readCloser io.ReadCloser
	var plat string
	if platform != nil && platform.Architecture != "" && platform.OS != "" {
		plat = fmt.Sprintf("%s/%s", platform.OS, platform.Architecture)
	}
	distributionRef, err := reference.ParseNormalizedNamed(img)
	if err != nil {
		return fmt.Errorf("can not parse image name %s: %v", img, err)
	}
	var imgRefAndAuth trust.ImageRefAndAuth
	imgRefAndAuth, err = trust.GetImageReferencesAndAuth(ctx, nil, image.AuthResolver(c), distributionRef.String())
	if err != nil {
		return fmt.Errorf("can not get image auth: %v", err)
	}
	var encodedAuth string
	encodedAuth, err = command.EncodeAuthToBase64(*imgRefAndAuth.AuthConfig())
	if err != nil {
		return fmt.Errorf("can not encode auth config to base64:%v", err)
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
	_, stdout, _ := term.StdStreams()
	out := streams.NewOut(stdout)
	err = jsonmessage.DisplayJSONMessagesToStream(readCloser, out, nil)
	if err != nil {
		err = fmt.Errorf("can not display message, err: %v", err)
		return err
	}
	return nil
}
