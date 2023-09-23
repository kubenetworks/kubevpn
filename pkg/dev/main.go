package dev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/archive"
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
	"k8s.io/apimachinery/pkg/util/wait"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/interrupt"
	"k8s.io/kubectl/pkg/util/podutils"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/mesh"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type ConnectMode string

const (
	ConnectModeContainer ConnectMode = "container"
	ConnectModeHost      ConnectMode = "host"
)

type Options struct {
	Headers       map[string]string
	Namespace     string
	Workload      string
	Factory       cmdutil.Factory
	ContainerName string
	NoProxy       bool
	ExtraCIDR     []string
	ExtraDomain   []string
	ConnectMode   ConnectMode
	Engine        config.Engine

	// docker options
	DockerImage string
	Options     RunOptions
	Copts       *ContainerOptions

	// inner
	Cli       *client.Client
	DockerCli *command.DockerCli
}

func (d *Options) Main(ctx context.Context, tempContainerConfig *containerConfig) error {
	rand.Seed(time.Now().UnixNano())
	object, err := util.GetUnstructuredObject(d.Factory, d.Namespace, d.Workload)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	//var path []string
	templateSpec, _, err = util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	set, err := d.Factory.KubernetesClientSet()
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
	lab := labels.SelectorFromSet(templateSpec.Labels).String()
	firstPod, _, err := polymorphichelpers.GetFirstPod(set.CoreV1(), d.Namespace, lab, time.Second*60, sortBy)
	if err != nil {
		return err
	}

	pod := firstPod.Name

	env, err := util.GetEnv(ctx, d.Factory, d.Namespace, pod)
	if err != nil {
		log.Errorf("get env from k8s: %v", err)
		return err
	}
	volume, err := GetVolume(ctx, d.Factory, d.Namespace, pod)
	if err != nil {
		log.Errorf("get volume from k8s: %v", err)
		return err
	}
	dns, err := GetDNS(ctx, d.Factory, d.Namespace, pod)
	if err != nil {
		log.Errorf("get dns from k8s: %v", err)
		return err
	}

	mesh.RemoveContainers(templateSpec)
	runConfigList := ConvertKubeResourceToContainer(d.Namespace, *templateSpec, env, volume, dns)
	err = mergeDockerOptions(runConfigList, d, tempContainerConfig)
	if err != nil {
		log.Errorf("can not fill docker options, err: %v", err)
		return err
	}
	// check resource
	var outOfMemory bool
	outOfMemory, _ = checkOutOfMemory(templateSpec, d.Cli)
	if outOfMemory {
		return fmt.Errorf("your pod resource request is bigger than docker-desktop resource, please adjust your docker-desktop resource")
	}
	mode := container.NetworkMode(d.Copts.netMode.NetworkMode())
	if len(d.Copts.netMode.Value()) != 0 {
		for _, runConfig := range runConfigList[:] {
			// remove expose port
			runConfig.config.ExposedPorts = nil
			runConfig.hostConfig.NetworkMode = mode
			if mode.IsContainer() {
				runConfig.hostConfig.PidMode = containertypes.PidMode(d.Copts.netMode.NetworkMode())
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
		networkID, err = createKubevpnNetwork(ctx, d.Cli)
		if err != nil {
			log.Errorf("create network for %s: %v", d.Workload, err)
			return err
		}

		runConfigList[len(runConfigList)-1].networkingConfig.EndpointsConfig[runConfigList[len(runConfigList)-1].containerName] = &network.EndpointSettings{
			NetworkID: networkID,
		}
		var portmap = nat.PortMap{}
		var portset = nat.PortSet{}
		for _, runConfig := range runConfigList {
			for k, v := range runConfig.hostConfig.PortBindings {
				if oldValue, ok := portmap[k]; ok {
					portmap[k] = append(oldValue, v...)
				} else {
					portmap[k] = v
				}
			}
			for k, v := range runConfig.config.ExposedPorts {
				portset[k] = v
			}
		}
		runConfigList[len(runConfigList)-1].hostConfig.PortBindings = portmap
		runConfigList[len(runConfigList)-1].config.ExposedPorts = portset

		// skip last, use last container network
		for _, runConfig := range runConfigList[:len(runConfigList)-1] {
			// remove expose port
			runConfig.config.ExposedPorts = nil
			runConfig.hostConfig.NetworkMode = containertypes.NetworkMode("container:" + runConfigList[len(runConfigList)-1].containerName)
			runConfig.hostConfig.PidMode = containertypes.PidMode("container:" + runConfigList[len(runConfigList)-1].containerName)
			runConfig.hostConfig.PortBindings = nil

			// remove dns
			runConfig.hostConfig.DNS = nil
			runConfig.hostConfig.DNSOptions = nil
			runConfig.hostConfig.DNSSearch = nil
			runConfig.hostConfig.PublishAllPorts = false
			runConfig.config.Hostname = ""
		}
	}

	handler.RollbackFuncList = append(handler.RollbackFuncList, func() {
		_ = runConfigList.Remove(ctx, d.Cli)
	})
	err = runConfigList.Run(ctx, volume, d.Cli, d.DockerCli)
	if err != nil {
		return err
	}
	return terminal(runConfigList[0].containerName, d.DockerCli)
}

type ConfigList []*RunConfig

func (l ConfigList) Remove(ctx context.Context, cli *client.Client) error {
	for _, runConfig := range l {
		err := cli.NetworkDisconnect(ctx, runConfig.containerName, runConfig.containerName, true)
		if err != nil {
			log.Debug(err)
		}
		if runConfig.hostConfig.AutoRemove {
			err = cli.ContainerRemove(ctx, runConfig.containerName, types.ContainerRemoveOptions{Force: true})
			if err != nil {
				log.Debug(err)
			}
		}
	}
	i, err := cli.NetworkInspect(ctx, config.ConfigMapPodTrafficManager, types.NetworkInspectOptions{})
	if err != nil {
		return err
	}
	if len(i.Containers) == 0 {
		return cli.NetworkRemove(ctx, config.ConfigMapPodTrafficManager)
	}
	return nil
}

func (l ConfigList) Run(ctx context.Context, volume map[string][]mount.Mount, cli *client.Client, dockerCli *command.DockerCli) error {
	for index := len(l) - 1; index >= 0; index-- {
		runConfig := l[index]
		if index == 0 {
			_, err := runFirst(ctx, runConfig, cli, dockerCli)
			if err != nil {
				log.Errorf("run main container container failed: %v", err)
				return err
			}
		} else {
			id, err := run(ctx, runConfig, cli, dockerCli)
			if err != nil {
				// try another way to startup container
				log.Infof("occur err: %v, try another way to startup container...", err)
				runConfig.hostConfig.Mounts = nil
				id, err = run(ctx, runConfig, cli, dockerCli)
				if err != nil {
					return err
				}
				err = l.copyToContainer(ctx, volume[runConfig.k8sContainerName], cli, id)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (l ConfigList) copyToContainer(ctx context.Context, volume []mount.Mount, cli *client.Client, id string) error {
	// copy volume into container
	for _, v := range volume {
		target, err := createFolder(ctx, cli, id, v.Source, v.Target)
		if err != nil {
			log.Debugf("create folder %s previoully failed, err: %v", target, err)
		}
		log.Debugf("from %s to %s", v.Source, v.Target)
		srcInfo, err := archive.CopyInfoSourcePath(v.Source, true)
		if err != nil {
			log.Errorf("copy info source path, err: %v", err)
			return err
		}
		srcArchive, err := archive.TarResource(srcInfo)
		if err != nil {
			log.Errorf("tar resource failed, err: %v", err)
			return err
		}
		dstDir, preparedArchive, err := archive.PrepareArchiveCopy(srcArchive, srcInfo, archive.CopyInfo{Path: v.Target})
		if err != nil {
			log.Errorf("can not prepare archive copy, err: %v", err)
			return err
		}

		err = cli.CopyToContainer(ctx, id, dstDir, preparedArchive, types.CopyToContainerOptions{
			AllowOverwriteDirWithFile: true,
			CopyUIDGID:                true,
		})
		if err != nil {
			log.Infof("can not copy %s to container %s:%s, err: %v", v.Source, id, v.Target, err)
		}
	}
	return nil
}

func createFolder(ctx context.Context, cli *client.Client, id string, src string, target string) (string, error) {
	lstat, err := os.Lstat(src)
	if err != nil {
		return "", err
	}
	if !lstat.IsDir() {
		target = filepath.Dir(target)
	}
	var create types.IDResponse
	create, err = cli.ContainerExecCreate(ctx, id, types.ExecConfig{
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          []string{"mkdir", "-p", target},
	})
	if err != nil {
		log.Errorf("create folder %s previoully failed, err: %v", target, err)
		return "", err
	}
	err = cli.ContainerExecStart(ctx, create.ID, types.ExecStartCheck{})
	if err != nil {
		log.Errorf("create folder %s previoully failed, err: %v", target, err)
		return "", err
	}
	log.Infof("wait create folder %s in container %s to be done...", target, id)
	chanStop := make(chan struct{})
	wait.Until(func() {
		inspect, err := cli.ContainerExecInspect(ctx, create.ID)
		if err != nil {
			log.Warningf("can not inspect container %s, err: %v", id, err)
			return
		}
		if !inspect.Running {
			close(chanStop)
		}
	}, time.Second, chanStop)
	return target, nil
}

func checkOutOfMemory(spec *v1.PodTemplateSpec, cli *client.Client) (outOfMemory bool, err error) {
	var info types.Info
	info, err = cli.Info(context.Background())
	if err != nil {
		return
	}
	total := info.MemTotal
	var req int64
	for _, container := range spec.Spec.Containers {
		memory := container.Resources.Requests.Memory()
		if memory != nil {
			req += memory.Value()
		}
	}
	if req > total {
		outOfMemory = true
		return
	}
	return
}

func DoDev(ctx context.Context, devOption *Options, conf *util.SshConfig, flags *pflag.FlagSet, f cmdutil.Factory, transferImage bool) error {
	cli, dockerCli, err := util.GetClient()
	if err != nil {
		return err
	}
	mode := container.NetworkMode(devOption.Copts.netMode.NetworkMode())
	if mode.IsContainer() {
		var inspect types.ContainerJSON
		inspect, err = cli.ContainerInspect(ctx, mode.ConnectedContainer())
		if err != nil {
			return err
		}
		if inspect.State == nil {
			return fmt.Errorf("can not get container status, please make contianer name is valid")
		}
		if !inspect.State.Running {
			return fmt.Errorf("container %s status is %s, expect is running, please make sure your outer docker name is correct", mode.ConnectedContainer(), inspect.State.Status)
		}
	} else if mode.IsDefault() && util.RunningInContainer() {
		var hostname string
		if hostname, err = os.Hostname(); err != nil {
			return err
		}
		log.Infof("hostname %s", hostname)
		err = devOption.Copts.netMode.Set(fmt.Sprintf("container:%s", hostname))
		if err != nil {
			return err
		}
	}

	// connect to cluster, in container or host
	cancel, err := devOption.doConnect(ctx, f, conf, transferImage)
	if err != nil {
		log.Errorf("connect to cluster failed, err: %v", err)
		return err
	}
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()

	var tempContainerConfig *containerConfig
	err = validatePullOpt(devOption.Options.Pull)
	if err != nil {
		return err
	}
	proxyConfig := dockerCli.ConfigFile().ParseProxyConfig(dockerCli.Client().DaemonHost(), opts.ConvertKVStringsToMapWithNil(devOption.Copts.env.GetAll()))
	var newEnv []string
	for k, v := range proxyConfig {
		if v == nil {
			newEnv = append(newEnv, k)
		} else {
			newEnv = append(newEnv, fmt.Sprintf("%s=%s", k, *v))
		}
	}
	devOption.Copts.env = *opts.NewListOptsRef(&newEnv, nil)
	tempContainerConfig, err = parse(flags, devOption.Copts, dockerCli.ServerInfo().OSType)
	// just in case the parse does not exit
	if err != nil {
		return err
	}
	err = validateAPIVersion(tempContainerConfig, dockerCli.Client().ClientVersion())
	if err != nil {
		return err
	}

	return devOption.Main(ctx, tempContainerConfig)
}

// connect to cluster network on docker container or host
func (d *Options) doConnect(ctx context.Context, f cmdutil.Factory, conf *util.SshConfig, transferImage bool) (cancel func(), err error) {
	connect := &handler.ConnectOptions{
		Headers:     d.Headers,
		Workloads:   []string{d.Workload},
		ExtraCIDR:   d.ExtraCIDR,
		ExtraDomain: d.ExtraDomain,
		Engine:      d.Engine,
	}
	if err = connect.InitClient(f); err != nil {
		return
	}
	d.Namespace = connect.Namespace

	if err = connect.PreCheckResource(); err != nil {
		return
	}
	if len(connect.Workloads) > 1 {
		return nil, fmt.Errorf("can only dev one workloads at same time, workloads: %v", connect.Workloads)
	}
	if len(connect.Workloads) < 1 {
		return nil, fmt.Errorf("you must provide resource to dev, workloads : %v is invaild", connect.Workloads)
	}
	d.Workload = connect.Workloads[0]

	// if no-proxy is true, not needs to intercept traffic
	if d.NoProxy {
		if len(connect.Headers) != 0 {
			return nil, fmt.Errorf("not needs to provide headers if is no-proxy mode")
		}
		connect.Workloads = []string{}
	}

	switch d.ConnectMode {
	case ConnectModeHost:
		daemonCli := daemon.GetClient(false)
		if daemonCli == nil {
			return nil, fmt.Errorf("get nil daemon client")
		}
		var kubeconfig []byte
		var ns string
		kubeconfig, ns, err = util.ConvertToKubeconfigBytes(f)
		if err != nil {
			return
		}
		// not needs to ssh jump in daemon, because dev mode will hang up until user exit,
		// so just ssh jump in client is enough
		req := &rpc.ConnectRequest{
			KubeconfigBytes: string(kubeconfig),
			Namespace:       ns,
			Headers:         connect.Headers,
			Workloads:       connect.Workloads,
			ExtraCIDR:       connect.ExtraCIDR,
			ExtraDomain:     connect.ExtraDomain,
			UseLocalDNS:     connect.UseLocalDNS,
			Engine:          string(connect.Engine),
			TransferImage:   transferImage,
			Image:           config.Image,
			Level:           int32(log.DebugLevel),
			SshJump:         conf.ToRPC(),
		}
		cancel = disconnect(ctx, daemonCli)
		var resp rpc.Daemon_ConnectClient
		resp, err = daemonCli.Connect(ctx, req)
		if err != nil {
			log.Errorf("connect to cluster error: %s", err.Error())
			return
		}
		for {
			recv, err := resp.Recv()
			if err == io.EOF {
				return cancel, nil
			} else if err != nil {
				return cancel, err
			}
			fmt.Fprint(os.Stdout, recv.Message)
		}

	case ConnectModeContainer:
		var path = os.Getenv(config.EnvSSHJump)
		if path != "" {
			path, err = ConvertHost(path)
		} else {
			path, err = connect.GetKubeconfigPath()
		}
		if err != nil {
			return
		}
		var platform specs.Platform
		if d.Options.Platform != "" {
			platform, err = platforms.Parse(d.Options.Platform)
			if err != nil {
				return nil, pkgerr.Wrap(err, "error parsing specified platform")
			}
		}

		var connectContainer *RunConfig
		connectContainer, err = createConnectContainer(d.NoProxy, *connect, path, d.Cli, &platform)
		if err != nil {
			return
		}
		cancelCtx, cancelFunc := context.WithCancel(ctx)
		defer cancelFunc()
		var id string
		id, err = run(cancelCtx, connectContainer, d.Cli, d.DockerCli)
		if err != nil {
			return
		}
		h := interrupt.New(
			func(signal os.Signal) { return },
			func() {
				cancelFunc()
				_ = d.Cli.ContainerKill(context.Background(), id, "SIGTERM")
				_ = runLogsSinceNow(d.DockerCli, id, true)
			},
		)
		go h.Run(func() error { select {} })
		handler.RollbackFuncList = append(handler.RollbackFuncList, func() {
			h.Close()
		})
		err = runLogsWaitRunning(cancelCtx, d.DockerCli, id)
		if err != nil {
			// interrupt by signal KILL
			if errors.Is(err, context.Canceled) {
				err = nil
				return
			}
			return
		}
		err = d.Copts.netMode.Set(fmt.Sprintf("container:%s", id))
		return
	default:
		return nil, fmt.Errorf("unsupport connect mode: %s", d.ConnectMode)
	}
}

func disconnect(ctx context.Context, daemonClient rpc.DaemonClient) func() {
	return func() {
		resp, err := daemonClient.Disconnect(ctx, &rpc.DisconnectRequest{})
		if err != nil {
			log.Errorf("disconnect error: %v", err)
			return
		}
		for {
			msg, err := resp.Recv()
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

func createConnectContainer(noProxy bool, connect handler.ConnectOptions, path string, cli *client.Client, platform *specs.Platform) (*RunConfig, error) {
	var entrypoint []string
	if noProxy {
		entrypoint = []string{"kubevpn", "connect", "--foreground", "-n", connect.Namespace, "--kubeconfig", "/root/.kube/config", "--image", config.Image, "--engine", string(connect.Engine)}
		for _, v := range connect.ExtraCIDR {
			entrypoint = append(entrypoint, "--extra-cidr", v)
		}
		for _, v := range connect.ExtraDomain {
			entrypoint = append(entrypoint, "--extra-domain", v)
		}
	} else {
		entrypoint = []string{"kubevpn", "proxy", connect.Workloads[0], "--foreground", "-n", connect.Namespace, "--kubeconfig", "/root/.kube/config", "--image", config.Image, "--engine", string(connect.Engine)}
		for k, v := range connect.Headers {
			entrypoint = append(entrypoint, "--headers", fmt.Sprintf("%s=%s", k, v))
		}
		for _, v := range connect.ExtraCIDR {
			entrypoint = append(entrypoint, "--extra-cidr", v)
		}
		for _, v := range connect.ExtraDomain {
			entrypoint = append(entrypoint, "--extra-domain", v)
		}
	}

	runConfig := &container.Config{
		User:            "root",
		AttachStdin:     false,
		AttachStdout:    false,
		AttachStderr:    false,
		ExposedPorts:    nil,
		StdinOnce:       false,
		Env:             []string{fmt.Sprintf("%s=1", config.EnvStartSudoKubeVPNByKubeVPN)},
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
		PortBindings:  nil,
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
	kubevpnNetwork, err := createKubevpnNetwork(context.Background(), cli)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s_%s_%s", "kubevpn", "local", suffix)
	c := &RunConfig{
		config:     runConfig,
		hostConfig: hostConfig,
		networkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{name: {
				NetworkID: kubevpnNetwork,
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

	options := types.ContainerLogsOptions{
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
			if strings.Contains(buf.String(), "enjoy it") {
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

	options := types.ContainerLogsOptions{
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
