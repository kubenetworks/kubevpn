package run

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/docker/go-connections/nat"
	log "github.com/sirupsen/logrus"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Connect connects to the cluster network via either host daemon or a Docker container.
func (option *Options) Connect(ctx context.Context, sshConfig *pkgssh.SshConfig, imagePullSecretName string, portBindings nat.PortMap, managerNamespace string) error {
	switch option.ConnectMode {
	case ConnectModeHost:
		return option.connectViaHost(ctx, sshConfig, imagePullSecretName, managerNamespace)
	case ConnectModeContainer:
		return option.connectViaContainer(ctx, portBindings, managerNamespace)
	default:
		return fmt.Errorf("unsupported connect mode: %s", option.ConnectMode)
	}
}

func (option *Options) connectViaHost(ctx context.Context, sshConfig *pkgssh.SshConfig, imagePullSecretName string, managerNamespace string) error {
	cli, err := daemon.GetClient(false)
	if err != nil {
		return fmt.Errorf("failed to get daemon client: %w", err)
	}
	kubeConfigBytes, ns, err := util.ConvertToKubeConfigBytes(option.factory)
	if err != nil {
		return err
	}
	if !sshConfig.IsEmpty() && !sshConfig.IsLoopback() {
		if ip := util.GetAPIServerFromKubeConfigBytes(kubeConfigBytes); ip != nil {
			option.ExtraRouteInfo.ExtraCIDR = append(option.ExtraRouteInfo.ExtraCIDR, ip.String())
		}
	}
	req := &rpc.ProxyRequest{
		KubeconfigBytes:      string(kubeConfigBytes),
		Namespace:            ns,
		Headers:              option.Headers,
		Workloads:            util.If(option.NoProxy, nil, []string{option.Workload}),
		ExtraRoute:           option.ExtraRouteInfo.ToRPC(),
		OriginKubeconfigPath: util.GetKubeConfigPath(option.factory),
		Image:                config.Image,
		ImagePullSecretName:  imagePullSecretName,
		Level:                int32(util.If(config.Debug, log.DebugLevel, log.InfoLevel)),
		SshJump:              handler.SshConfigToRPC(sshConfig),
		ManagerNamespace:     managerNamespace,
	}
	option.AddRollbackFunc(func() error {
		resp, err := cli.Disconnect(context.Background())
		if err != nil {
			return err
		}
		err = resp.Send(&rpc.DisconnectRequest{
			KubeconfigBytes: ptr.To(string(kubeConfigBytes)),
			Namespace:       ptr.To(ns),
			SshJump:         handler.SshConfigToRPC(sshConfig),
		})
		if err != nil {
			return err
		}
		_ = grpcutil.PrintGRPCStream[rpc.DisconnectResponse](context.Background(), resp)
		return nil
	})
	resp, err := cli.Proxy(context.Background())
	if err != nil {
		plog.G(ctx).Errorf("Connect to cluster error: %v", err)
		return err
	}
	err = resp.Send(req)
	if err != nil {
		plog.G(ctx).Errorf("Connect to cluster error: %v", err)
		return err
	}
	return grpcutil.PrintGRPCStream[rpc.SyncResponse](ctx, resp)
}

func (option *Options) connectViaContainer(ctx context.Context, portBindings nat.PortMap, managerNamespace string) error {
	name, err := option.CreateConnectContainer(ctx, portBindings, managerNamespace)
	if err != nil {
		return err
	}
	plog.G(ctx).Info("Connecting to the cluster (in container) ...")
	err = util.WaitDockerContainerRunning(ctx, *name)
	if err != nil {
		return err
	}
	option.AddRollbackFunc(func() error {
		_, _ = util.ContainerKill(context.Background(), name)
		_ = util.RunLogsSinceNow(*name, true)
		return nil
	})
	err = util.RunLogsWaitRunning(ctx, *name)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	plog.G(ctx).Infof("Connected to cluster in container")
	return option.ContainerOptions.netMode.Set(fmt.Sprintf("container:%s", *name))
}

// CreateConnectContainer creates a Docker container running kubevpn connect/proxy.
func (option *Options) CreateConnectContainer(ctx context.Context, portBindings nat.PortMap, managerNamespace string) (*string, error) {
	portMap, portSet, err := option.GetExposePort(ctx, portBindings)
	if err != nil {
		return nil, err
	}

	var kubeconfigPath = os.Getenv(config.EnvSSHJump)
	if kubeconfigPath != "" {
		kubeconfigPath, err = util.ConvertK8sApiServerToDomain(kubeconfigPath)
	} else {
		kubeconfigPath, err = util.GetKubeconfigPath(option.factory)
	}
	if err != nil {
		return nil, err
	}

	entrypoint := option.buildConnectEntrypoint(managerNamespace)

	name := util.Join(option.Namespace, "kubevpn", randomSuffix())
	_, err = util.CreateNetwork(ctx, config.ConfigMapPodTrafficManager)
	if err != nil {
		return nil, err
	}
	args := []string{
		"run",
		"--detach",
		"--volume", fmt.Sprintf("%s:%s", kubeconfigPath, "/root/.kube/config"),
		"--privileged",
		"--rm",
	}
	args = append(args, containerSecurityOpts()...)
	args = append(args,
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=0",
		"--add-host", "host.docker.internal:host-gateway",
		"--add-host", "kubernetes:host-gateway",
		"--network", config.ConfigMapPodTrafficManager,
		"--name", name,
	)
	for port := range portSet {
		args = append(args, "--expose", port.Port())
	}
	for port, bindings := range portMap {
		// docker --publish expects hostPort:containerPort (see runconfig.go).
		for _, binding := range bindings {
			args = append(args, "--publish", fmt.Sprintf("%s:%s", binding.HostPort, port.Port()))
		}
	}

	result := append([]string{"docker"}, args...)
	result = append(result, config.Image)
	result = append(result, entrypoint...)
	err = util.RunContainer(ctx, result)
	if err != nil {
		return nil, err
	}
	return &name, nil
}

func (option *Options) buildConnectEntrypoint(managerNamespace string) []string {
	var entrypoint []string
	if option.NoProxy {
		entrypoint = []string{
			"kubevpn", "connect",
			"--foreground",
			"-n", option.Namespace,
			"--kubeconfig", "/root/.kube/config",
			"--image", config.Image,
		}
	} else {
		entrypoint = []string{
			"kubevpn", "proxy", option.Workload,
			"--foreground",
			"-n", option.Namespace,
			"--kubeconfig", "/root/.kube/config",
			"--image", config.Image,
			"--manager-namespace", managerNamespace,
		}
		for k, v := range option.Headers {
			entrypoint = append(entrypoint, "--headers", fmt.Sprintf("%s=%s", k, v))
		}
	}
	for _, v := range option.ExtraRouteInfo.ExtraCIDR {
		entrypoint = append(entrypoint, "--extra-cidr", v)
	}
	for _, v := range option.ExtraRouteInfo.ExtraDomain {
		entrypoint = append(entrypoint, "--extra-domain", v)
	}
	if option.ExtraRouteInfo.ExtraNodeIP {
		entrypoint = append(entrypoint, "--extra-node-ip")
	}
	return entrypoint
}
