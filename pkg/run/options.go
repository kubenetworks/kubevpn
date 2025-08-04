package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	typescontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	pkgerr "github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type ConnectMode string

const (
	ConnectModeContainer ConnectMode = "container"
	ConnectModeHost      ConnectMode = "host"
)

type Options struct {
	Headers        map[string]string
	Namespace      string
	Workload       string
	ContainerName  string
	NoProxy        bool
	ExtraRouteInfo handler.ExtraRouteInfo
	ConnectMode    ConnectMode

	// docker options
	DevImage string

	RunOptions       RunOptions
	ContainerOptions *ContainerOptions

	factory    cmdutil.Factory
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config

	// rollback
	rollbackFuncList []func() error
}

func (option *Options) Main(ctx context.Context, sshConfig *pkgssh.SshConfig, config *Config, hostConfig *HostConfig, imagePullSecretName string, managerNamespace string) error {
	mode := typescontainer.NetworkMode(option.ContainerOptions.netMode.NetworkMode())
	if mode.IsContainer() {
		plog.G(ctx).Infof("Network mode container is %s", mode.ConnectedContainer())
	} else if mode.IsDefault() && util.RunningInContainer() {
		hostname, err := os.Hostname()
		if err != nil {
			return err
		}
		plog.G(ctx).Infof("Hostname is %s", hostname)
		err = option.ContainerOptions.netMode.Set(fmt.Sprintf("container:%s", hostname))
		if err != nil {
			return err
		}
	}

	// Connect to cluster, in container or host
	err := option.Connect(ctx, sshConfig, imagePullSecretName, hostConfig.PortBindings, managerNamespace)
	if err != nil {
		plog.G(ctx).Errorf("Connect to cluster failed, err: %v", err)
		return err
	}

	return option.Run(ctx, config, hostConfig)
}

// Connect to cluster network on docker container or host
func (option *Options) Connect(ctx context.Context, sshConfig *pkgssh.SshConfig, imagePullSecretName string, portBindings nat.PortMap, managerNamespace string) error {
	if option.ConnectMode == ConnectModeHost {
		cli, err := daemon.GetClient(false)
		if err != nil {
			return pkgerr.Wrap(err, "get nil daemon client")
		}
		kubeConfigBytes, ns, err := util.ConvertToKubeConfigBytes(option.factory)
		if err != nil {
			return err
		}
		if !sshConfig.IsEmpty() {
			if ip := util.GetAPIServerFromKubeConfigBytes(kubeConfigBytes); ip != nil {
				option.ExtraRouteInfo.ExtraCIDR = append(option.ExtraRouteInfo.ExtraCIDR, ip.String())
			}
		}
		// no need to ssh jump in daemon, because run mode will hang up until user exit,
		// so just ssh jump in client is enough
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
			SshJump:              sshConfig.ToRPC(),
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
				SshJump:         sshConfig.ToRPC(),
			})
			if err != nil {
				return err
			}
			_ = util.PrintGRPCStream[rpc.DisconnectResponse](nil, resp)
			return nil
		})
		var resp rpc.Daemon_ProxyClient
		resp, err = cli.Proxy(context.Background())
		if err != nil {
			plog.G(ctx).Errorf("Connect to cluster error: %s", err.Error())
			return err
		}
		err = resp.Send(req)
		if err != nil {
			plog.G(ctx).Errorf("Connect to cluster error: %s", err.Error())
			return err
		}
		err = util.PrintGRPCStream[rpc.SyncResponse](ctx, resp)
		return err
	}

	if option.ConnectMode == ConnectModeContainer {
		name, err := option.CreateConnectContainer(ctx, portBindings, managerNamespace)
		if err != nil {
			return err
		}
		plog.G(ctx).Infof("Starting connect to cluster in container")
		err = util.WaitDockerContainerRunning(ctx, *name)
		if err != nil {
			return err
		}
		option.AddRollbackFunc(func() error {
			// docker kill --signal
			_, _ = util.ContainerKill(context.Background(), name)
			_ = util.RunLogsSinceNow(*name, true)
			return nil
		})
		err = util.RunLogsWaitRunning(ctx, *name)
		if err != nil {
			// interrupt by signal KILL
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		plog.G(ctx).Infof("Connected to cluster in container")
		err = option.ContainerOptions.netMode.Set(fmt.Sprintf("container:%s", *name))
		return err
	}

	return fmt.Errorf("unsupport connect mode: %s", option.ConnectMode)
}

func (option *Options) Run(ctx context.Context, config *Config, hostConfig *HostConfig) error {
	templateSpec, err := option.GetPodTemplateSpec(ctx)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get unstructured object error: %v", err)
		return err
	}

	label := labels.SelectorFromSet(templateSpec.Labels).String()
	var list []v1.Pod
	list, err = util.GetRunningPodList(ctx, option.clientset, option.Namespace, label)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get first running pod from k8s: %v", err)
		return err
	}

	env, err := util.GetEnv(ctx, option.clientset, option.config, option.Namespace, list[0].Name)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get env from k8s: %v", err)
		return err
	}
	option.AddRollbackFunc(func() error {
		for _, s := range env {
			_ = os.RemoveAll(s)
		}
		return nil
	})
	volume, err := util.GetVolume(ctx, option.clientset, option.factory, option.Namespace, list[0].Name)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get volume from k8s: %v", err)
		return err
	}
	option.AddRollbackFunc(func() error {
		return util.RemoveDir(volume)
	})
	dns, err := util.GetDNS(ctx, option.clientset, option.config, option.Namespace, list[0].Name)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get DNS from k8s: %v", err)
		return err
	}
	configList, err := option.ConvertPodToContainerConfigList(ctx, *templateSpec, config, hostConfig, env, volume, dns)
	if err != nil {
		return err
	}
	option.AddRollbackFunc(func() error {
		if hostConfig.AutoRemove {
			_ = configList.Remove(context.Background(), len(option.ContainerOptions.netMode.Value()) != 0)
		}
		return nil
	})
	return configList.Run(ctx)
}

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

	var entrypoint []string
	if option.NoProxy {
		entrypoint = []string{
			"kubevpn",
			"connect",
			"--foreground",
			"-n", option.Namespace,
			"--kubeconfig", "/root/.kube/config",
			"--image", config.Image,
		}
	} else {
		entrypoint = []string{
			"kubevpn",
			"proxy",
			option.Workload,
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

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:5]
	name := util.Join(option.Namespace, "kubevpn", suffix)
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
		"--cap-add", "SYS_PTRACE",
		"--cap-add", "SYS_ADMIN",
		"--security-opt", "apparmor=unconfined",
		"--security-opt", "seccomp=unconfined",
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=0",
		"--add-host", "host.docker.internal:host-gateway",
		"--add-host", "kubernetes:host-gateway",
		"--network", config.ConfigMapPodTrafficManager,
		"--name", name,
	}
	for port := range portSet {
		args = append(args, "--expose", port.Port())
	}
	for port, bindings := range portMap {
		args = append(args, "--publish", fmt.Sprintf("%s:%s", port.Port(), bindings[0].HostPort))
	}

	var result = []string{"docker"}
	result = append(result, args...)
	result = append(result, config.Image)
	result = append(result, entrypoint...)
	err = util.RunContainer(ctx, result)
	if err != nil {
		return nil, err
	}
	return &name, nil
}

func (option *Options) AddRollbackFunc(f func() error) {
	option.rollbackFuncList = append(option.rollbackFuncList, f)
}

func (option *Options) GetRollbackFuncList() []func() error {
	return option.rollbackFuncList
}

func (option *Options) GetExposePort(ctx context.Context, portBinds nat.PortMap) (nat.PortMap, nat.PortSet, error) {
	templateSpec, err := option.GetPodTemplateSpec(ctx)
	if err != nil {
		plog.G(context.Background()).Errorf("Failed to get unstructured object error: %v", err)
		return nil, nil, err
	}

	var portMap = nat.PortMap{}
	var portSet = nat.PortSet{}
	for _, c := range templateSpec.Spec.Containers {
		for _, port := range c.Ports {
			p := nat.Port(fmt.Sprintf("%d/%s", port.ContainerPort, strings.ToLower(string(port.Protocol))))
			if port.HostPort != 0 {
				binding := []nat.PortBinding{{HostPort: strconv.FormatInt(int64(port.HostPort), 10)}}
				portMap[p] = binding
			} else {
				binding := []nat.PortBinding{{HostPort: strconv.FormatInt(int64(port.ContainerPort), 10)}}
				portMap[p] = binding
			}
			portSet[p] = struct{}{}
		}
	}

	for port, bindings := range portBinds {
		portMap[port] = bindings
		portSet[port] = struct{}{}
	}

	return portMap, portSet, nil
}

func (option *Options) InitClient(f cmdutil.Factory) (err error) {
	option.factory = f
	if option.config, err = option.factory.ToRESTConfig(); err != nil {
		return
	}
	if option.restclient, err = option.factory.RESTClient(); err != nil {
		return
	}
	if option.clientset, err = option.factory.KubernetesClientSet(); err != nil {
		return
	}
	if option.Namespace, _, err = option.factory.ToRawKubeConfigLoader().Namespace(); err != nil {
		return
	}
	return
}

func (option *Options) GetPodTemplateSpec(ctx context.Context) (*v1.PodTemplateSpec, error) {
	_, controller, err := util.GetTopOwnerObject(ctx, option.factory, option.Namespace, option.Workload)
	if err != nil {
		return nil, err
	}

	u := controller.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	templateSpec, _, err = util.GetPodTemplateSpecPath(u)
	return templateSpec, err
}
