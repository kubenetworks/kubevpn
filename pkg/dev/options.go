package dev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	typescontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	pkgerr "github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/interrupt"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/mesh"
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
	Factory        cmdutil.Factory
	ContainerName  string
	NoProxy        bool
	ExtraRouteInfo handler.ExtraRouteInfo
	ConnectMode    ConnectMode
	Engine         config.Engine

	// docker options
	DevImage string
	Options  runOptions
	Copts    *containerOptions

	// inner
	Cli       *client.Client
	DockerCli *command.DockerCli

	// rollback
	rollbackFuncList []func() error
}

func (option *Options) Main(ctx context.Context, c *containerConfig) error {
	rand.NewSource(time.Now().UnixNano())
	object, err := util.GetUnstructuredObject(option.Factory, option.Namespace, option.Workload)
	if err != nil {
		log.Errorf("get unstructured object error: %v", err)
		return err
	}

	u := object.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	//var path []string
	templateSpec, _, err = util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	clientSet, err := option.Factory.KubernetesClientSet()
	if err != nil {
		return err
	}

	sortBy := func(pods []*v1.Pod) sort.Interface {
		for i := 0; i < len(pods); i++ {
			if pods[i].DeletionTimestamp != nil {
				pods = append(pods[:i], pods[i+1:]...)
				i--
			}
		}
		return sort.Reverse(podutils.ActivePods(pods))
	}
	label := labels.SelectorFromSet(templateSpec.Labels).String()
	firstPod, _, err := polymorphichelpers.GetFirstPod(clientSet.CoreV1(), option.Namespace, label, time.Second*5, sortBy)
	if err != nil {
		log.Errorf("get first running pod from k8s: %v", err)
		return err
	}

	pod := firstPod.Name

	env, err := util.GetEnv(ctx, option.Factory, option.Namespace, pod)
	if err != nil {
		log.Errorf("get env from k8s: %v", err)
		return err
	}
	volume, err := util.GetVolume(ctx, option.Factory, option.Namespace, pod)
	if err != nil {
		log.Errorf("get volume from k8s: %v", err)
		return err
	}
	option.AddRollbackFunc(func() error {
		return util.RemoveDir(volume)
	})
	dns, err := util.GetDNS(ctx, option.Factory, option.Namespace, pod)
	if err != nil {
		log.Errorf("get dns from k8s: %v", err)
		return err
	}

	mesh.RemoveContainers(templateSpec)
	list := convertKubeResourceToContainer(option.Namespace, *templateSpec, env, volume, dns)
	mergeDockerOptions(list, option, c)
	mode := container.NetworkMode(option.Copts.netMode.NetworkMode())
	if len(option.Copts.netMode.Value()) != 0 {
		log.Infof("network mode is %s", option.Copts.netMode.NetworkMode())
		for _, runConfig := range list[:] {
			// remove expose port
			runConfig.config.ExposedPorts = nil
			runConfig.hostConfig.NetworkMode = mode
			if mode.IsContainer() {
				runConfig.hostConfig.PidMode = typescontainer.PidMode(option.Copts.netMode.NetworkMode())
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
		networkID, err = createKubevpnNetwork(ctx, option.Cli)
		if err != nil {
			log.Errorf("create network for %s: %v", option.Workload, err)
			return err
		}
		log.Infof("create docker network %s", networkID)

		list[len(list)-1].networkingConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			list[len(list)-1].containerName: {NetworkID: networkID},
		}
		var portMap = nat.PortMap{}
		var portSet = nat.PortSet{}
		for _, runConfig := range list {
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
		list[len(list)-1].hostConfig.PortBindings = portMap
		list[len(list)-1].config.ExposedPorts = portSet

		// skip last, use last container network
		for _, runConfig := range list[:len(list)-1] {
			// remove expose port
			runConfig.config.ExposedPorts = nil
			runConfig.hostConfig.NetworkMode = typescontainer.NetworkMode("container:" + list[len(list)-1].containerName)
			runConfig.hostConfig.PidMode = typescontainer.PidMode("container:" + list[len(list)-1].containerName)
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
		return list.Remove(ctx, option.Cli)
	})
	return list.Run(ctx, volume, option.Cli, option.DockerCli)
}

// connect to cluster network on docker container or host
func (option *Options) connect(ctx context.Context, f cmdutil.Factory, conf *util.SshConfig, transferImage bool, c *containerConfig) (func(), error) {
	connect := &handler.ConnectOptions{
		Headers:              option.Headers,
		Workloads:            []string{option.Workload},
		ExtraRouteInfo:       option.ExtraRouteInfo,
		Engine:               option.Engine,
		OriginKubeconfigPath: util.GetKubeConfigPath(f),
	}
	if err := connect.InitClient(f); err != nil {
		return nil, err
	}
	option.Namespace = connect.Namespace

	if err := connect.PreCheckResource(); err != nil {
		return nil, err
	}
	if len(connect.Workloads) > 1 {
		return nil, fmt.Errorf("can only dev one workloads at same time, workloads: %v", connect.Workloads)
	}
	if len(connect.Workloads) < 1 {
		return nil, fmt.Errorf("you must provide resource to dev, workloads : %v is invaild", connect.Workloads)
	}
	option.Workload = connect.Workloads[0]

	// if no-proxy is true, not needs to intercept traffic
	if option.NoProxy {
		if len(connect.Headers) != 0 {
			return nil, fmt.Errorf("not needs to provide headers if is no-proxy mode")
		}
		connect.Workloads = []string{}
	}

	switch option.ConnectMode {
	case ConnectModeHost:
		daemonCli := daemon.GetClient(false)
		if daemonCli == nil {
			return nil, fmt.Errorf("get nil daemon client")
		}
		kubeConfigBytes, ns, err := util.ConvertToKubeConfigBytes(f)
		if err != nil {
			return nil, err
		}
		logLevel := log.ErrorLevel
		if config.Debug {
			logLevel = log.DebugLevel
		}
		// not needs to ssh jump in daemon, because dev mode will hang up until user exit,
		// so just ssh jump in client is enough
		req := &rpc.ConnectRequest{
			KubeconfigBytes:      string(kubeConfigBytes),
			Namespace:            ns,
			Headers:              connect.Headers,
			Workloads:            connect.Workloads,
			ExtraRoute:           connect.ExtraRouteInfo.ToRPC(),
			Engine:               string(connect.Engine),
			OriginKubeconfigPath: util.GetKubeConfigPath(f),
			TransferImage:        transferImage,
			Image:                config.Image,
			Level:                int32(logLevel),
			SshJump:              conf.ToRPC(),
		}
		cancel := disconnect(ctx, daemonCli, &rpc.LeaveRequest{Workloads: connect.Workloads}, &rpc.DisconnectRequest{
			KubeconfigBytes: ptr.To(string(kubeConfigBytes)),
			Namespace:       ptr.To(ns),
			SshJump:         conf.ToRPC(),
		})
		var resp rpc.Daemon_ConnectClient
		resp, err = daemonCli.Proxy(ctx, req)
		if err != nil {
			log.Errorf("connect to cluster error: %s", err.Error())
			return cancel, err
		}
		for {
			response, err := resp.Recv()
			if err == io.EOF {
				return cancel, nil
			} else if err != nil {
				return cancel, err
			}
			fmt.Fprint(os.Stdout, response.Message)
		}

	case ConnectModeContainer:
		port, set, err := option.GetExposePort(c)
		if err != nil {
			return nil, err
		}
		var path = os.Getenv(config.EnvSSHJump)
		if path != "" {
			path, err = util.ConvertK8sApiServerToDomain(path)
		} else {
			path, err = util.GetKubeconfigPath(connect.GetFactory())
		}
		if err != nil {
			return nil, err
		}
		var platform specs.Platform
		if option.Options.platform != "" {
			platform, err = platforms.Parse(option.Options.platform)
			if err != nil {
				return nil, pkgerr.Wrap(err, "error parsing specified platform")
			}
		}

		var connectContainer *RunConfig
		connectContainer, err = createConnectContainer(option.NoProxy, *connect, path, option.Cli, &platform, port, set)
		if err != nil {
			return nil, err
		}
		cancelCtx, cancelFunc := context.WithCancel(ctx)
		defer cancelFunc()
		var id string
		log.Infof("starting container connect to cluster")
		id, err = run(cancelCtx, connectContainer, option.Cli, option.DockerCli)
		if err != nil {
			return nil, err
		}
		h := interrupt.New(
			func(signal os.Signal) { return },
			func() {
				cancelFunc()
				_ = option.Cli.ContainerKill(context.Background(), id, "SIGTERM")
				_ = runLogsSinceNow(option.DockerCli, id, true)
			},
		)
		go h.Run(func() error { select {} })
		option.AddRollbackFunc(func() error {
			h.Close()
			return nil
		})
		err = runLogsWaitRunning(cancelCtx, option.DockerCli, id)
		if err != nil {
			// interrupt by signal KILL
			if errors.Is(err, context.Canceled) {
				return nil, nil
			}
			return nil, err
		}
		log.Infof("container connect to cluster successfully")
		err = option.Copts.netMode.Set(fmt.Sprintf("container:%s", id))
		return nil, err
	default:
		return nil, fmt.Errorf("unsupport connect mode: %s", option.ConnectMode)
	}
}

func disconnect(ctx context.Context, daemonClient rpc.DaemonClient, leaveReq *rpc.LeaveRequest, req *rpc.DisconnectRequest) func() {
	return func() {
		resp, err := daemonClient.Leave(ctx, leaveReq)
		if err == nil {
			for {
				msg, err := resp.Recv()
				if err == io.EOF {
					break
				} else if err != nil {
					log.Errorf("leave resource %s error: %v", strings.Join(leaveReq.Workloads, " "), err)
					break
				}
				fmt.Fprint(os.Stdout, msg.Message)
			}
		}
		resp1, err := daemonClient.Disconnect(ctx, req)
		if err != nil {
			log.Errorf("disconnect error: %v", err)
			return
		}
		for {
			msg, err := resp1.Recv()
			if err == io.EOF {
				return
			} else if err != nil {
				log.Errorf("disconnect error: %v", err)
				return
			}
			fmt.Fprint(os.Stdout, msg.Message)
		}
	}
}

func createConnectContainer(noProxy bool, connect handler.ConnectOptions, path string, cli *client.Client, platform *specs.Platform, port nat.PortMap, set nat.PortSet) (*RunConfig, error) {
	var entrypoint []string
	if noProxy {
		entrypoint = []string{"kubevpn", "connect", "--foreground", "-n", connect.Namespace, "--kubeconfig", "/root/.kube/config", "--image", config.Image, "--engine", string(connect.Engine)}
	} else {
		entrypoint = []string{"kubevpn", "proxy", connect.Workloads[0], "--foreground", "-n", connect.Namespace, "--kubeconfig", "/root/.kube/config", "--image", config.Image, "--engine", string(connect.Engine)}
		for k, v := range connect.Headers {
			entrypoint = append(entrypoint, "--headers", fmt.Sprintf("%s=%s", k, v))
		}
	}
	for _, v := range connect.ExtraRouteInfo.ExtraCIDR {
		entrypoint = append(entrypoint, "--extra-cidr", v)
	}
	for _, v := range connect.ExtraRouteInfo.ExtraDomain {
		entrypoint = append(entrypoint, "--extra-domain", v)
	}
	if connect.ExtraRouteInfo.ExtraNodeIP {
		entrypoint = append(entrypoint, "--extra-node-ip")
	}

	runConfig := &container.Config{
		User:            "root",
		AttachStdin:     false,
		AttachStdout:    false,
		AttachStderr:    false,
		ExposedPorts:    set,
		StdinOnce:       false,
		Env:             []string{},
		Cmd:             []string{},
		Healthcheck:     nil,
		ArgsEscaped:     false,
		Image:           config.Image,
		Volumes:         nil,
		Entrypoint:      entrypoint,
		NetworkDisabled: false,
		MacAddress:      "",
		OnBuild:         nil,
		StopSignal:      "",
		StopTimeout:     nil,
		Shell:           nil,
	}
	hostConfig := &container.HostConfig{
		Binds:         []string{fmt.Sprintf("%s:%s", path, "/root/.kube/config")},
		LogConfig:     container.LogConfig{},
		PortBindings:  port,
		RestartPolicy: container.RestartPolicy{},
		AutoRemove:    false,
		VolumeDriver:  "",
		VolumesFrom:   nil,
		ConsoleSize:   [2]uint{},
		CapAdd:        strslice.StrSlice{"SYS_PTRACE", "SYS_ADMIN"}, // for dlv
		CgroupnsMode:  "",
		// https://stackoverflow.com/questions/24319662/from-inside-of-a-docker-container-how-do-i-connect-to-the-localhost-of-the-mach
		// couldn't get current server API group list: Get "https://host.docker.internal:62844/api?timeout=32s": tls: failed to verify certificate: x509: certificate is valid for kubernetes.default.svc.cluster.local, kubernetes.default.svc, kubernetes.default, kubernetes, istio-sidecar-injector.istio-system.svc, proxy-exporter.kube-system.svc, not host.docker.internal
		ExtraHosts:      []string{"host.docker.internal:host-gateway", "kubernetes:host-gateway"},
		GroupAdd:        nil,
		IpcMode:         "",
		Cgroup:          "",
		Links:           nil,
		OomScoreAdj:     0,
		PidMode:         "",
		Privileged:      true,
		PublishAllPorts: false,
		ReadonlyRootfs:  false,
		SecurityOpt:     []string{"apparmor=unconfined", "seccomp=unconfined"},
		StorageOpt:      nil,
		Tmpfs:           nil,
		UTSMode:         "",
		UsernsMode:      "",
		ShmSize:         0,
		Sysctls:         map[string]string{"net.ipv6.conf.all.disable_ipv6": strconv.Itoa(0)},
		Runtime:         "",
		Isolation:       "",
		Resources:       container.Resources{},
		MaskedPaths:     nil,
		ReadonlyPaths:   nil,
		Init:            nil,
	}
	var suffix string
	if newUUID, err := uuid.NewUUID(); err == nil {
		suffix = strings.ReplaceAll(newUUID.String(), "-", "")[:5]
	}
	networkID, err := createKubevpnNetwork(context.Background(), cli)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s_%s_%s", "kubevpn", "local", suffix)
	c := &RunConfig{
		config:     runConfig,
		hostConfig: hostConfig,
		networkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{name: {
				NetworkID: networkID,
			}},
		},
		platform:         platform,
		containerName:    name,
		k8sContainerName: name,
	}
	return c, nil
}

func runLogsWaitRunning(ctx context.Context, dockerCli command.Cli, container string) error {
	c, err := dockerCli.Client().ContainerInspect(ctx, container)
	if err != nil {
		return err
	}

	options := typescontainer.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	}
	logStream, err := dockerCli.Client().ContainerLogs(ctx, c.ID, options)
	if err != nil {
		return err
	}
	defer logStream.Close()

	buf := bytes.NewBuffer(nil)
	w := io.MultiWriter(buf, dockerCli.Out())

	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			// keyword, maybe can find another way more elegant
			if strings.Contains(buf.String(), "dns service ok") {
				cancelFunc()
				return
			}
		}
	}()

	var errChan = make(chan error)
	go func() {
		var err error
		if c.Config.Tty {
			_, err = io.Copy(w, logStream)
		} else {
			_, err = stdcopy.StdCopy(w, dockerCli.Err(), logStream)
		}
		if err != nil {
			errChan <- err
		}
	}()

	select {
	case err = <-errChan:
		return err
	case <-cancel.Done():
		return nil
	}
}

func runLogsSinceNow(dockerCli command.Cli, container string, follow bool) error {
	ctx := context.Background()

	c, err := dockerCli.Client().ContainerInspect(ctx, container)
	if err != nil {
		return err
	}

	options := typescontainer.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      "0m",
		Follow:     follow,
	}
	responseBody, err := dockerCli.Client().ContainerLogs(ctx, c.ID, options)
	if err != nil {
		return err
	}
	defer responseBody.Close()

	if c.Config.Tty {
		_, err = io.Copy(dockerCli.Out(), responseBody)
	} else {
		_, err = stdcopy.StdCopy(dockerCli.Out(), dockerCli.Err(), responseBody)
	}
	return err
}

func runKill(dockerCli command.Cli, containers ...string) error {
	var errs []string
	ctx := context.Background()
	errChan := parallelOperation(ctx, append([]string{}, containers...), func(ctx context.Context, container string) error {
		return dockerCli.Client().ContainerKill(ctx, container, "SIGTERM")
	})
	for _, name := range containers {
		if err := <-errChan; err != nil {
			errs = append(errs, err.Error())
		} else {
			fmt.Fprintln(dockerCli.Out(), name)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

func createKubevpnNetwork(ctx context.Context, cli *client.Client) (string, error) {
	by := map[string]string{"owner": config.ConfigMapPodTrafficManager}
	list, _ := cli.NetworkList(ctx, types.NetworkListOptions{})
	for _, resource := range list {
		if reflect.DeepEqual(resource.Labels, by) {
			return resource.ID, nil
		}
	}

	create, err := cli.NetworkCreate(ctx, config.ConfigMapPodTrafficManager, types.NetworkCreate{
		Driver: "bridge",
		Scope:  "local",
		IPAM: &network.IPAM{
			Driver:  "",
			Options: nil,
			Config: []network.IPAMConfig{
				{
					Subnet:  config.DockerCIDR.String(),
					Gateway: config.DockerRouterIP.String(),
				},
			},
		},
		//Options: map[string]string{"--icc": "", "--ip-masq": ""},
		Labels: by,
	})
	if err != nil {
		if errdefs.IsForbidden(err) {
			list, _ = cli.NetworkList(ctx, types.NetworkListOptions{})
			for _, resource := range list {
				if reflect.DeepEqual(resource.Labels, by) {
					return resource.ID, nil
				}
			}
		}
		return "", err
	}
	return create.ID, nil
}

func (option *Options) AddRollbackFunc(f func() error) {
	option.rollbackFuncList = append(option.rollbackFuncList, f)
}

func (option *Options) GetRollbackFuncList() []func() error {
	return option.rollbackFuncList
}

func AddDockerFlags(options *Options, p *pflag.FlagSet, cli *command.DockerCli) {
	p.SetInterspersed(false)

	// These are flags not stored in Config/HostConfig
	p.BoolVarP(&options.Options.detach, "detach", "d", false, "Run container in background and print container ID")
	p.StringVar(&options.Options.name, "name", "", "Assign a name to the container")
	p.StringVar(&options.Options.pull, "pull", PullImageMissing, `Pull image before running ("`+PullImageAlways+`"|"`+PullImageMissing+`"|"`+PullImageNever+`")`)
	p.BoolVarP(&options.Options.quiet, "quiet", "q", false, "Suppress the pull output")

	// Add an explicit help that doesn't have a `-h` to prevent the conflict
	// with hostname
	p.Bool("help", false, "Print usage")

	command.AddPlatformFlag(p, &options.Options.platform)
	command.AddTrustVerificationFlags(p, &options.Options.untrusted, cli.ContentTrustEnabled())

	options.Copts = addFlags(p)
}

func (option *Options) GetExposePort(containerCfg *containerConfig) (nat.PortMap, nat.PortSet, error) {
	object, err := util.GetUnstructuredObject(option.Factory, option.Namespace, option.Workload)
	if err != nil {
		log.Errorf("get unstructured object error: %v", err)
		return nil, nil, err
	}

	u := object.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	templateSpec, _, err = util.GetPodTemplateSpecPath(u)
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

	for port, bindings := range containerCfg.HostConfig.PortBindings {
		portMap[port] = bindings
		portSet[port] = struct{}{}
	}

	return portMap, portSet, nil
}
