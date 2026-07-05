package run

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/calmh/incontainer"
	typescontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"
	"github.com/miekg/dns"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// ConnectMode specifies how to establish the cluster network connection (host daemon or container).
type ConnectMode string

const (
	ConnectModeContainer ConnectMode = "container"
	ConnectModeHost      ConnectMode = "host"
)

// Options holds configuration for running a K8s workload locally via Docker.
type Options struct {
	Headers        map[string]string
	Namespace      string
	Workload       string
	ContainerName  string
	NoProxy        bool
	ExtraRouteInfo handler.ExtraRouteInfo
	ConnectMode    ConnectMode

	DevImage string

	RunOptions       RunOptions
	ContainerOptions *ContainerOptions

	factory    cmdutil.Factory
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config

	rollbackFuncList []func() error
}

// PodContext bundles all data fetched from the cluster for a workload.
// Populated once by fetchPodContext, consumed by ConvertPodToContainerConfigList.
type PodContext struct {
	TemplateSpec *v1.PodTemplateSpec
	EnvMap       map[string]string
	MountVolume  map[string][]mount.Mount
	DNSConfig    *dns.ClientConfig
}

// Main is the entrypoint: connect to cluster, then run the workload locally.
func (option *Options) Main(ctx context.Context, sshConfig *pkgssh.SshConfig, config *Config, hostConfig *HostConfig, imagePullSecretName string, managerNamespace string) error {
	mode := typescontainer.NetworkMode(option.ContainerOptions.netMode.NetworkMode())
	if mode.IsContainer() {
		plog.G(ctx).Infof("Network mode container is %s", mode.ConnectedContainer())
	} else if mode.IsDefault() && incontainer.Detect() {
		hostname, err := os.Hostname()
		if err != nil {
			return err
		}
		plog.G(ctx).Infof("Hostname is %s", hostname)
		if err = option.ContainerOptions.netMode.Set(fmt.Sprintf("container:%s", hostname)); err != nil {
			return err
		}
	}

	if err := option.Connect(ctx, sshConfig, imagePullSecretName, hostConfig.PortBindings, managerNamespace); err != nil {
		plog.G(ctx).Errorf("Connect to cluster failed, err: %v", err)
		return err
	}

	return option.Run(ctx, config, hostConfig)
}

// Run fetches workload context from K8s, converts to Docker configs, and starts containers.
func (option *Options) Run(ctx context.Context, config *Config, hostConfig *HostConfig) error {
	podCtx, err := option.fetchPodContext(ctx)
	if err != nil {
		return err
	}
	option.AddRollbackFunc(func() error {
		for _, s := range podCtx.EnvMap {
			_ = os.RemoveAll(s)
		}
		return RemoveDir(podCtx.MountVolume)
	})

	configList, err := option.ConvertPodToContainerConfigList(ctx, podCtx, config, hostConfig)
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

// fetchPodContext fetches the PodTemplateSpec, env vars, volumes, and DNS from the cluster.
func (option *Options) fetchPodContext(ctx context.Context) (*PodContext, error) {
	templateSpec, err := option.GetPodTemplateSpec(ctx)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get pod template spec: %v", err)
		return nil, err
	}

	label := labels.SelectorFromSet(templateSpec.Labels).String()
	podList, err := util.GetRunningPodList(ctx, option.clientset, option.Namespace, label)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get running pod list: %v", err)
		return nil, err
	}
	podName := podList[0].Name

	env, err := util.GetEnv(ctx, option.clientset, option.config, option.Namespace, podName)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get env from k8s: %v", err)
		return nil, err
	}
	volume, err := GetVolume(ctx, option.clientset, option.factory, option.Namespace, podName)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get volume from k8s: %v", err)
		return nil, err
	}
	dnsConf, err := util.GetDNS(ctx, option.clientset, option.config, option.Namespace, podName)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get DNS from k8s: %v", err)
		return nil, err
	}
	return &PodContext{
		TemplateSpec: templateSpec,
		EnvMap:       env,
		MountVolume:  volume,
		DNSConfig:    dnsConf,
	}, nil
}

// GetExposePort collects all exposed ports from the workload spec and user-specified bindings.
func (option *Options) GetExposePort(ctx context.Context, portBinds nat.PortMap) (nat.PortMap, nat.PortSet, error) {
	templateSpec, err := option.GetPodTemplateSpec(ctx)
	if err != nil {
		return nil, nil, err
	}
	portMap := nat.PortMap{}
	portSet := nat.PortSet{}
	for _, c := range templateSpec.Spec.Containers {
		addContainerPortBindings(c.Ports, portMap, portSet)
	}
	for port, bindings := range portBinds {
		portMap[port] = bindings
		portSet[port] = struct{}{}
	}
	return portMap, portSet, nil
}

// addContainerPortBindings converts K8s container ports into Docker port bindings.
// Shared by GetExposePort and ConvertPodToContainerConfigList.
func addContainerPortBindings(ports []v1.ContainerPort, portMap nat.PortMap, portSet nat.PortSet) {
	for _, port := range ports {
		p := toNatPort(port)
		if portMap[p] != nil {
			continue
		}
		hostPort := port.ContainerPort
		if port.HostPort != 0 {
			hostPort = port.HostPort
		}
		portMap[p] = []nat.PortBinding{{HostPort: strconv.FormatInt(int64(hostPort), 10)}}
		portSet[p] = struct{}{}
	}
}

func toNatPort(port v1.ContainerPort) nat.Port {
	return nat.Port(fmt.Sprintf("%d/%s", port.ContainerPort, strings.ToLower(string(port.Protocol))))
}

// InitClient initializes the Kubernetes client, REST config, and namespace from the factory.
func (option *Options) InitClient(f cmdutil.Factory) error {
	option.factory = f
	var err error
	option.config, option.restclient, option.clientset, option.Namespace, err = util.InitKubeClient(f)
	return err
}

// GetPodTemplateSpec resolves the workload's top-level owner and extracts its PodTemplateSpec.
func (option *Options) GetPodTemplateSpec(ctx context.Context) (*v1.PodTemplateSpec, error) {
	_, controller, err := util.GetTopOwnerObject(ctx, option.factory, option.Namespace, option.Workload)
	if err != nil {
		return nil, err
	}
	u := controller.Object.(*unstructured.Unstructured)
	templateSpec, _, err := util.GetPodTemplateSpecPath(u)
	return templateSpec, err
}

// AddRollbackFunc registers a cleanup function to be called on teardown.
func (option *Options) AddRollbackFunc(f func() error) {
	option.rollbackFuncList = append(option.rollbackFuncList, f)
}

// GetRollbackFuncList returns all registered rollback/cleanup functions.
func (option *Options) GetRollbackFuncList() []func() error {
	return option.rollbackFuncList
}
