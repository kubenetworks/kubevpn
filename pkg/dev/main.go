package dev

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	util2 "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func DoDev(ctx context.Context, option *Options, conf *util.SshConfig, flags *pflag.FlagSet, f util2.Factory, transferImage bool) error {
	if p := option.Options.platform; p != "" {
		_, err := platforms.Parse(p)
		if err != nil {
			return fmt.Errorf("error parsing specified platform: %v", err)
		}
	}
	client, cli, err := util.GetClient()
	if err != nil {
		return err
	}
	mode := container.NetworkMode(option.Copts.netMode.NetworkMode())
	if mode.IsContainer() {
		logrus.Infof("network mode container is %s", mode.ConnectedContainer())
		var inspect types.ContainerJSON
		inspect, err = client.ContainerInspect(ctx, mode.ConnectedContainer())
		if err != nil {
			logrus.Errorf("can not inspect container %s, err: %v", mode.ConnectedContainer(), err)
			return err
		}
		if inspect.State == nil {
			return fmt.Errorf("can not get container status, please make container name is valid")
		}
		if !inspect.State.Running {
			return fmt.Errorf("container %s status is %s, expect is running, please make sure your outer docker name is correct", mode.ConnectedContainer(), inspect.State.Status)
		}
		logrus.Infof("container %s is running", mode.ConnectedContainer())
	} else if mode.IsDefault() && util.RunningInContainer() {
		var hostname string
		if hostname, err = os.Hostname(); err != nil {
			return err
		}
		logrus.Infof("hostname is %s", hostname)
		err = option.Copts.netMode.Set(fmt.Sprintf("container:%s", hostname))
		if err != nil {
			return err
		}
	}

	err = validatePullOpt(option.Options.pull)
	if err != nil {
		return err
	}
	proxyConfig := cli.ConfigFile().ParseProxyConfig(cli.Client().DaemonHost(), opts.ConvertKVStringsToMapWithNil(option.Copts.env.GetAll()))
	var newEnv []string
	for k, v := range proxyConfig {
		if v == nil {
			newEnv = append(newEnv, k)
		} else {
			newEnv = append(newEnv, fmt.Sprintf("%s=%s", k, *v))
		}
	}
	option.Copts.env = *opts.NewListOptsRef(&newEnv, nil)
	var c *containerConfig
	c, err = parse(flags, option.Copts, cli.ServerInfo().OSType)
	// just in case the parse does not exit
	if err != nil {
		return err
	}
	err = validateAPIVersion(c, cli.Client().ClientVersion())
	if err != nil {
		return err
	}

	// connect to cluster, in container or host
	cancel, err := option.connect(ctx, f, conf, transferImage, c)
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()
	if err != nil {
		logrus.Errorf("connect to cluster failed, err: %v", err)
		return err
	}

	return option.Main(ctx, c)
}
