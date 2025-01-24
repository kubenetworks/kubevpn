package dev

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types/container"
	typescontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
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
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
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
	Engine         config.Engine

	// docker options
	DevImage string

	RunOptions       RunOptions
	ContainerOptions *ContainerOptions

	// inner
	cli       *client.Client
	dockerCli *command.DockerCli

	factory    cmdutil.Factory
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config

	// rollback
	rollbackFuncList []func() error
}

func (option *Options) Main(ctx context.Context, sshConfig *pkgssh.SshConfig, flags *pflag.FlagSet, transferImage bool, imagePullSecretName string) error {
	mode := typescontainer.NetworkMode(option.ContainerOptions.netMode.NetworkMode())
	if mode.IsContainer() {
		log.Infof("Network mode container is %s", mode.ConnectedContainer())
		inspect, err := option.cli.ContainerInspect(ctx, mode.ConnectedContainer())
		if err != nil {
			log.Errorf("Failed to inspect container %s, err: %v", mode.ConnectedContainer(), err)
			return err
		}
		if inspect.State == nil {
			return fmt.Errorf("can not get container status, please make container name is valid")
		}
		if !inspect.State.Running {
			return fmt.Errorf("container %s status is %s, expect is running, please make sure your outer docker name is correct", mode.ConnectedContainer(), inspect.State.Status)
		}
		log.Infof("Container %s is running", mode.ConnectedContainer())
	} else if mode.IsDefault() && util.RunningInContainer() {
		hostname, err := os.Hostname()
		if err != nil {
			return err
		}
		log.Infof("Hostname is %s", hostname)
		err = option.ContainerOptions.netMode.Set(fmt.Sprintf("container:%s", hostname))
		if err != nil {
			return err
		}
	}

	config, hostConfig, err := Parse(flags, option.ContainerOptions)
	// just in case the Parse does not exit
	if err != nil {
		return err
	}

	// Connect to cluster, in container or host
	err = option.Connect(ctx, sshConfig, transferImage, imagePullSecretName, hostConfig.PortBindings)
	if err != nil {
		log.Errorf("Connect to cluster failed, err: %v", err)
		return err
	}

	return option.Dev(ctx, config, hostConfig)
}

// Connect to cluster network on docker container or host
func (option *Options) Connect(ctx context.Context, sshConfig *pkgssh.SshConfig, transferImage bool, imagePullSecretName string, portBindings nat.PortMap) error {
	switch option.ConnectMode {
	case ConnectModeHost:
		daemonCli := daemon.GetClient(false)
		if daemonCli == nil {
			return fmt.Errorf("get nil daemon client")
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
		logLevel := log.InfoLevel
		if config.Debug {
			logLevel = log.DebugLevel
		}
		// not needs to ssh jump in daemon, because dev mode will hang up until user exit,
		// so just ssh jump in client is enough
		req := &rpc.ConnectRequest{
			KubeconfigBytes:      string(kubeConfigBytes),
			Namespace:            ns,
			Headers:              option.Headers,
			Workloads:            []string{option.Workload},
			ExtraRoute:           option.ExtraRouteInfo.ToRPC(),
			Engine:               string(option.Engine),
			OriginKubeconfigPath: util.GetKubeConfigPath(option.factory),
			TransferImage:        transferImage,
			Image:                config.Image,
			ImagePullSecretName:  imagePullSecretName,
			Level:                int32(logLevel),
			SshJump:              sshConfig.ToRPC(),
		}
		if option.NoProxy {
			req.Workloads = nil
		}
		option.AddRollbackFunc(func() error {
			resp, err := daemonCli.Disconnect(ctx, &rpc.DisconnectRequest{
				KubeconfigBytes: ptr.To(string(kubeConfigBytes)),
				Namespace:       ptr.To(ns),
				SshJump:         sshConfig.ToRPC(),
			})
			if err != nil {
				return err
			}
			_ = util.PrintGRPCStream[rpc.DisconnectResponse](resp)
			return nil
		})
		var resp rpc.Daemon_ConnectClient
		resp, err = daemonCli.Proxy(ctx, req)
		if err != nil {
			log.Errorf("Connect to cluster error: %s", err.Error())
			return err
		}
		err = util.PrintGRPCStream[rpc.CloneResponse](resp)
		return err

	case ConnectModeContainer:
		runConfig, err := option.CreateConnectContainer(portBindings)
		if err != nil {
			return err
		}
		var id string
		log.Infof("Starting connect to cluster in container")
		id, err = run(ctx, option.cli, option.dockerCli, runConfig)
		if err != nil {
			return err
		}
		option.AddRollbackFunc(func() error {
			_ = option.cli.ContainerKill(context.Background(), id, "SIGTERM")
			_ = runLogsSinceNow(option.dockerCli, id, true)
			return nil
		})
		err = runLogsWaitRunning(ctx, option.dockerCli, id)
		if err != nil {
			// interrupt by signal KILL
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		log.Infof("Connected to cluster in container")
		err = option.ContainerOptions.netMode.Set(fmt.Sprintf("container:%s", id))
		return err
	default:
		return fmt.Errorf("unsupport connect mode: %s", option.ConnectMode)
	}
}

func (option *Options) Dev(ctx context.Context, cConfig *Config, hostConfig *HostConfig) error {
	templateSpec, err := option.GetPodTemplateSpec()
	if err != nil {
		return err
	}

	label := labels.SelectorFromSet(templateSpec.Labels).String()
	var list []v1.Pod
	list, err = util.GetRunningPodList(ctx, option.clientset, option.Namespace, label)
	if err != nil {
		log.Errorf("Failed to get first running pod from k8s: %v", err)
		return err
	}

	env, err := util.GetEnv(ctx, option.clientset, option.config, option.Namespace, list[0].Name)
	if err != nil {
		log.Errorf("Failed to get env from k8s: %v", err)
		return err
	}
	volume, err := util.GetVolume(ctx, option.factory, option.Namespace, list[0].Name)
	if err != nil {
		log.Errorf("Failed to get volume from k8s: %v", err)
		return err
	}
	option.AddRollbackFunc(func() error {
		return util.RemoveDir(volume)
	})
	dns, err := util.GetDNS(ctx, option.clientset, option.config, option.Namespace, list[0].Name)
	if err != nil {
		log.Errorf("Failed to get DNS from k8s: %v", err)
		return err
	}

	inject.RemoveContainers(templateSpec)
	if option.ContainerName != "" {
		var index = -1
		for i, c := range templateSpec.Spec.Containers {
			if option.ContainerName == c.Name {
				index = i
				break
			}
		}
		if index != -1 {
			templateSpec.Spec.Containers[0], templateSpec.Spec.Containers[index] = templateSpec.Spec.Containers[index], templateSpec.Spec.Containers[0]
		}
	}
	configList := ConvertPodToContainer(option.Namespace, *templateSpec, env, volume, dns)
	MergeDockerOptions(configList, option, cConfig, hostConfig)

	mode := container.NetworkMode(option.ContainerOptions.netMode.NetworkMode())
	if len(option.ContainerOptions.netMode.Value()) != 0 {
		log.Infof("Network mode is %s", option.ContainerOptions.netMode.NetworkMode())
		for _, runConfig := range configList[:] {
			// remove expose port
			runConfig.config.ExposedPorts = nil
			runConfig.hostConfig.NetworkMode = mode
			if mode.IsContainer() {
				runConfig.hostConfig.PidMode = typescontainer.PidMode(option.ContainerOptions.netMode.NetworkMode())
			}
			runConfig.hostConfig.PortBindings = nil

			// remove dns
			runConfig.hostConfig.DNS = nil
			runConfig.hostConfig.DNSOptions = nil
			runConfig.hostConfig.DNSSearch = nil
			runConfig.hostConfig.PublishAllPorts = false
			runConfig.config.Hostname = ""
		}
	} else {
		var networkID string
		networkID, err = createNetwork(ctx, option.cli)
		if err != nil {
			log.Errorf("Failed to create network for %s: %v", option.Workload, err)
			return err
		}
		log.Infof("Create docker network %s", networkID)

		configList[len(configList)-1].networkingConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			configList[len(configList)-1].name: {NetworkID: networkID},
		}
		var portMap = nat.PortMap{}
		var portSet = nat.PortSet{}
		for _, runConfig := range configList {
			for k, v := range runConfig.hostConfig.PortBindings {
				if oldValue, ok := portMap[k]; ok {
					portMap[k] = append(oldValue, v...)
				} else {
					portMap[k] = v
				}
			}
			for k, v := range runConfig.config.ExposedPorts {
				portSet[k] = v
			}
		}
		configList[len(configList)-1].hostConfig.PortBindings = portMap
		configList[len(configList)-1].config.ExposedPorts = portSet

		// skip last, use last container network
		for _, runConfig := range configList[:len(configList)-1] {
			// remove expose port
			runConfig.config.ExposedPorts = nil
			runConfig.hostConfig.NetworkMode = typescontainer.NetworkMode("container:" + configList[len(configList)-1].name)
			runConfig.hostConfig.PidMode = typescontainer.PidMode("container:" + configList[len(configList)-1].name)
			runConfig.hostConfig.PortBindings = nil

			// remove dns
			runConfig.hostConfig.DNS = nil
			runConfig.hostConfig.DNSOptions = nil
			runConfig.hostConfig.DNSSearch = nil
			runConfig.hostConfig.PublishAllPorts = false
			runConfig.config.Hostname = ""
		}
	}

	option.AddRollbackFunc(func() error {
		_ = configList.Remove(ctx, option.cli)
		return nil
	})
	return configList.Run(ctx, volume, option.cli, option.dockerCli)
}

func (option *Options) CreateConnectContainer(portBindings nat.PortMap) (*RunConfig, error) {
	portMap, portSet, err := option.GetExposePort(portBindings)
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
		entrypoint = []string{"kubevpn", "connect", "--foreground", "-n", option.Namespace, "--kubeconfig", "/root/.kube/config", "--image", config.Image, "--netstack", string(option.Engine)}
	} else {
		entrypoint = []string{"kubevpn", "proxy", option.Workload, "--foreground", "-n", option.Namespace, "--kubeconfig", "/root/.kube/config", "--image", config.Image, "--netstack", string(option.Engine)}
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

	runConfig := &container.Config{
		User:         "root",
		ExposedPorts: portSet,
		Env:          []string{},
		Cmd:          []string{},
		Healthcheck:  nil,
		Image:        config.Image,
		Entrypoint:   entrypoint,
	}
	hostConfig := &container.HostConfig{
		Binds:         []string{fmt.Sprintf("%s:%s", kubeconfigPath, "/root/.kube/config")},
		LogConfig:     container.LogConfig{},
		PortBindings:  portMap,
		AutoRemove:    true,
		Privileged:    true,
		RestartPolicy: container.RestartPolicy{},
		CapAdd:        strslice.StrSlice{"SYS_PTRACE", "SYS_ADMIN"}, // for dlv
		// https://stackoverflow.com/questions/24319662/from-inside-of-a-docker-container-how-do-i-connect-to-the-localhost-of-the-mach
		// couldn't get current server API group list: Get "https://host.docker.internal:62844/api?timeout=32s": tls: failed to verify certificate: x509: certificate is valid for kubernetes.default.svc.cluster.local, kubernetes.default.svc, kubernetes.default, kubernetes, istio-sidecar-injector.istio-system.svc, proxy-exporter.kube-system.svc, not host.docker.internal
		ExtraHosts:  []string{"host.docker.internal:host-gateway", "kubernetes:host-gateway"},
		SecurityOpt: []string{"apparmor=unconfined", "seccomp=unconfined"},
		Sysctls:     map[string]string{"net.ipv6.conf.all.disable_ipv6": strconv.Itoa(0)},
		Resources:   container.Resources{},
	}
	newUUID, err := uuid.NewUUID()
	if err != nil {
		return nil, err
	}
	suffix := strings.ReplaceAll(newUUID.String(), "-", "")[:5]
	name := util.Join(option.Namespace, "kubevpn", suffix)
	networkID, err := createNetwork(context.Background(), option.cli)
	if err != nil {
		return nil, err
	}
	var platform *specs.Platform
	if option.RunOptions.Platform != "" {
		plat, _ := platforms.Parse(option.RunOptions.Platform)
		platform = &plat
	}
	c := &RunConfig{
		config:           runConfig,
		hostConfig:       hostConfig,
		networkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{name: {NetworkID: networkID}}},
		platform:         platform,
		name:             name,
		Options:          RunOptions{Pull: PullImageMissing},
	}
	return c, nil
}

func (option *Options) AddRollbackFunc(f func() error) {
	option.rollbackFuncList = append(option.rollbackFuncList, f)
}

func (option *Options) GetRollbackFuncList() []func() error {
	return option.rollbackFuncList
}

func AddDockerFlags(options *Options, p *pflag.FlagSet) {
	p.SetInterspersed(false)

	// These are flags not stored in Config/HostConfig
	p.StringVar(&options.RunOptions.Pull, "pull", PullImageMissing, `Pull image before running ("`+PullImageAlways+`"|"`+PullImageMissing+`"|"`+PullImageNever+`")`)
	p.BoolVar(&options.RunOptions.SigProxy, "sig-proxy", true, "Proxy received signals to the process")

	// Add an explicit help that doesn't have a `-h` to prevent the conflict
	// with hostname
	p.Bool("help", false, "Print usage")

	command.AddPlatformFlag(p, &options.RunOptions.Platform)
	options.ContainerOptions = addFlags(p)
}

func (option *Options) GetExposePort(portBinds nat.PortMap) (nat.PortMap, nat.PortSet, error) {
	templateSpec, err := option.GetPodTemplateSpec()
	if err != nil {
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
	if option.cli, option.dockerCli, err = pkgssh.GetClient(); err != nil {
		return err
	}
	return
}

func (option *Options) GetPodTemplateSpec() (*v1.PodTemplateSpec, error) {
	object, err := util.GetUnstructuredObject(option.factory, option.Namespace, option.Workload)
	if err != nil {
		log.Errorf("Failed to get unstructured object error: %v", err)
		return nil, err
	}

	u := object.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	templateSpec, _, err = util.GetPodTemplateSpecPath(u)
	return templateSpec, err
}
