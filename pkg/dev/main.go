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
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
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
	"github.com/google/uuid"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	pkgerr "github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/interrupt"
	"k8s.io/kubectl/pkg/util/podutils"

	"github.com/wencaiwulue/kubevpn/pkg/config"
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

	// docker options
	Platform string
	//Pull         string // always, missing, never
	PublishAll   bool
	Entrypoint   string
	DockerImage  string
	Publish      opts.ListOpts
	Expose       opts.ListOpts
	ExtraHosts   opts.ListOpts
	NetMode      opts.NetworkOpt
	Env          opts.ListOpts
	Mounts       opts.MountOpt
	Volumes      opts.ListOpts
	VolumeDriver string
}

func (d Options) Main(ctx context.Context) error {
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
		return err
	}
	volume, err := GetVolume(ctx, d.Factory, d.Namespace, pod)
	if err != nil {
		return err
	}
	dns, err := GetDNS(ctx, d.Factory, d.Namespace, pod)
	if err != nil {
		return fmt.Errorf("can not get dns conf from pod: %s, err: %v", pod, err)
	}

	mesh.RemoveContainers(templateSpec)
	list := ConvertKubeResourceToContainer(d.Namespace, *templateSpec, env, volume, dns)
	err = fillOptions(list, d)
	if err != nil {
		return fmt.Errorf("can not fill docker options, err: %v", err)
	}
	var dockerCli *command.DockerCli
	var cli *client.Client
	cli, dockerCli, err = GetClient()
	if err != nil {
		return err
	}
	// check resource
	var outOfMemory bool
	outOfMemory, _ = checkOutOfMemory(templateSpec, cli)
	if outOfMemory {
		return fmt.Errorf("your pod resource request is bigger than docker-desktop resource, please adjust your docker-desktop resource")
	}
	mode := container.NetworkMode(d.NetMode.NetworkMode())
	if len(d.NetMode.Value()) != 0 {
		for _, runConfig := range list[:] {
			// remove expose port
			runConfig.config.ExposedPorts = nil
			runConfig.hostConfig.NetworkMode = mode
			if mode.IsContainer() {
				runConfig.hostConfig.PidMode = containertypes.PidMode(d.NetMode.NetworkMode())
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
		networkID, err = createKubevpnNetwork(ctx, cli)
		if err != nil {
			return err
		}

		list[0].networkingConfig.EndpointsConfig[list[0].containerName] = &network.EndpointSettings{
			NetworkID: networkID,
		}
		// skip first
		for _, runConfig := range list[1:] {
			// remove expose port
			runConfig.config.ExposedPorts = nil
			runConfig.hostConfig.NetworkMode = containertypes.NetworkMode("container:" + list[0].containerName)
			runConfig.hostConfig.PidMode = containertypes.PidMode("container:" + list[0].containerName)
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
		_ = list.Remove(ctx)
	})
	err = list.Run(ctx, volume)
	if err != nil {
		return err
	}
	return terminal(list[0].containerName, dockerCli)
}

type Run []*RunConfig

func (r Run) Remove(ctx context.Context) error {
	cli, _, err := GetClient()
	if err != nil {
		return err
	}

	for _, runConfig := range r {
		err = cli.NetworkDisconnect(ctx, runConfig.containerName, runConfig.containerName, true)
		if err != nil {
			log.Debug(err)
		}
		err = cli.ContainerRemove(ctx, runConfig.containerName, types.ContainerRemoveOptions{Force: true})
		if err != nil {
			log.Debug(err)
		}
	}
	var i types.NetworkResource
	i, err = cli.NetworkInspect(ctx, config.ConfigMapPodTrafficManager, types.NetworkInspectOptions{})
	if err != nil {
		return err
	}
	if len(i.Containers) == 0 {
		return cli.NetworkRemove(ctx, config.ConfigMapPodTrafficManager)
	}
	return nil
}

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

func (r Run) Run(ctx context.Context, volume map[string][]mount.Mount) error {
	cli, c, err := GetClient()
	if err != nil {
		return err
	}
	for _, runConfig := range r {
		var id string
		id, err = run(ctx, runConfig, cli, c)
		if err != nil {
			// try another way to startup container
			log.Infof("occur err: %v, try another way to startup container...", err)
			runConfig.hostConfig.Mounts = nil
			id, err = run(ctx, runConfig, cli, c)
			if err != nil {
				return err
			}
			err = r.copyToContainer(ctx, volume[runConfig.k8sContainerName], cli, id)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r Run) copyToContainer(ctx context.Context, volume []mount.Mount, cli *client.Client, id string) error {
	// copy volume into container
	for _, v := range volume {
		target, err := createFolder(ctx, cli, id, v.Source, v.Target)
		if err != nil {
			log.Debugf("create folder %s previoully faied, err: %v", target, err)
		}
		log.Debugf("from %s to %s", v.Source, v.Target)
		srcInfo, err := archive.CopyInfoSourcePath(v.Source, true)
		if err != nil {
			return fmt.Errorf("copy info source path, err: %v", err)
		}
		srcArchive, err := archive.TarResource(srcInfo)
		if err != nil {
			return fmt.Errorf("tar resource failed, err: %v", err)
		}
		dstDir, preparedArchive, err := archive.PrepareArchiveCopy(srcArchive, srcInfo, archive.CopyInfo{Path: v.Target})
		if err != nil {
			return fmt.Errorf("can not prepare archive copy, err: %v", err)
		}

		err = cli.CopyToContainer(ctx, id, dstDir, preparedArchive, types.CopyToContainerOptions{
			AllowOverwriteDirWithFile: true,
			CopyUIDGID:                true,
		})
		if err != nil {
			log.Info(fmt.Errorf("can not copy %s to container %s:%s, err: %v", v.Source, id, v.Target, err))
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
		return "", err
	}
	err = cli.ContainerExecStart(ctx, create.ID, types.ExecStartCheck{})
	if err != nil {
		return "", err
	}
	chanStop := make(chan struct{})
	wait.Until(func() {
		inspect, err := cli.ContainerExecInspect(ctx, create.ID)
		if err != nil {
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

func DoDev(devOptions Options, args []string, f cmdutil.Factory) error {
	connect := handler.ConnectOptions{
		Headers:     devOptions.Headers,
		Workloads:   args,
		ExtraCIDR:   devOptions.ExtraCIDR,
		ExtraDomain: devOptions.ExtraDomain,
	}

	mode := container.NetworkMode(devOptions.NetMode.NetworkMode())
	if mode.IsContainer() {
		client, _, err := GetClient()
		if err != nil {
			return err
		}
		var inspect types.ContainerJSON
		inspect, err = client.ContainerInspect(context.Background(), mode.ConnectedContainer())
		if err != nil {
			return err
		}
		if inspect.State == nil {
			return fmt.Errorf("can not get container status, please make contianer name is valid")
		}
		if !inspect.State.Running {
			return fmt.Errorf("container %s status is %s, expect is running, please make sure your outer docker name is correct", mode.ConnectedContainer(), inspect.State.Status)
		}
	}

	if err := connect.InitClient(f); err != nil {
		return err
	}
	err := connect.PreCheckResource()
	if err != nil {
		return err
	}

	if len(connect.Workloads) > 1 {
		return fmt.Errorf("can only dev one workloads at same time, workloads: %v", connect.Workloads)
	}
	if len(connect.Workloads) < 1 {
		return fmt.Errorf("you must provide resource to dev, workloads : %v is invaild", connect.Workloads)
	}

	var platform *specs.Platform
	if devOptions.Platform != "" {
		p, err := platforms.Parse(devOptions.Platform)
		if err != nil {
			return pkgerr.Wrap(err, "error parsing specified platform")
		}
		platform = &p
	}

	devOptions.Workload = connect.Workloads[0]
	// if no-proxy is true, not needs to intercept traffic
	if devOptions.NoProxy {
		if len(connect.Headers) != 0 {
			return fmt.Errorf("not needs to provide headers if is no-proxy mode")
		}
		connect.Workloads = []string{}
	}
	path, err := connect.GetKubeconfigPath()
	if err != nil {
		return err
	}
	switch devOptions.ConnectMode {
	case ConnectModeHost:
		defer func() {
			handler.Cleanup(syscall.SIGQUIT)
			select {}
		}()
		if err = connect.DoConnect(); err != nil {
			log.Errorln(err)
			return err
		}
	case ConnectModeContainer:
		var dockerCli *command.DockerCli
		var cli *client.Client
		cli, dockerCli, err = GetClient()
		if err != nil {
			return err
		}

		var entrypoint []string
		if devOptions.NoProxy {
			entrypoint = []string{"kubevpn", "connect", "-n", connect.Namespace, "--kubeconfig", "/root/.kube/config", "--image", config.Image}
			for _, v := range connect.ExtraCIDR {
				entrypoint = append(entrypoint, "--extra-cidr", v)
			}
			for _, v := range connect.ExtraDomain {
				entrypoint = append(entrypoint, "--extra-domain", v)
			}
		} else {
			entrypoint = []string{"kubevpn", "proxy", connect.Workloads[0], "-n", connect.Namespace, "--kubeconfig", "/root/.kube/config", "--image", config.Image}
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
			Binds:           []string{fmt.Sprintf("%s:%s", path, "/root/.kube/config")},
			LogConfig:       container.LogConfig{},
			PortBindings:    nil,
			RestartPolicy:   container.RestartPolicy{},
			AutoRemove:      true,
			VolumeDriver:    "",
			VolumesFrom:     nil,
			ConsoleSize:     [2]uint{},
			CapAdd:          strslice.StrSlice{"SYS_PTRACE", "SYS_ADMIN"}, // for dlv
			CgroupnsMode:    "",
			ExtraHosts:      nil,
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
			Sysctls:         nil,
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
		var kubevpnNetwork string
		kubevpnNetwork, err = createKubevpnNetwork(context.Background(), cli)
		if err != nil {
			return err
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
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var id string
		if id, err = run(ctx, c, cli, dockerCli); err != nil {
			return err
		}
		h := interrupt.New(func(signal os.Signal) {
			os.Exit(0)
		}, func() {
			cancel()
			_ = cli.ContainerKill(context.Background(), id, "SIGTERM")
			_ = runLogsSinceNow(dockerCli, id)
		})
		go h.Run(func() error { select {} })
		defer h.Close()
		if err = runLogsWaitRunning(ctx, dockerCli, id); err != nil {
			// interrupt by signal KILL
			if ctx.Err() == context.Canceled {
				return nil
			}
			return err
		}
		if err = devOptions.NetMode.Set("container:" + id); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupport connect mode: %s", devOptions.ConnectMode)
	}

	devOptions.Namespace = connect.Namespace
	err = devOptions.Main(context.Background())
	if err != nil {
		log.Errorln(err)
	}
	return err
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
	responseBody, err := dockerCli.Client().ContainerLogs(ctx, c.ID, options)
	if err != nil {
		return err
	}
	defer responseBody.Close()

	buf := bytes.NewBuffer(nil)
	writer := io.MultiWriter(buf, dockerCli.Out())

	var errChan = make(chan error)
	var stopChan = make(chan struct{})

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if strings.Contains(buf.String(), "enjoy it") {
				close(stopChan)
				return
			}
		}
	}()

	go func() {
		var err error
		if c.Config.Tty {
			_, err = io.Copy(writer, responseBody)
		} else {
			_, err = stdcopy.StdCopy(writer, dockerCli.Err(), responseBody)
		}
		if err != nil {
			errChan <- err
		}
	}()

	select {
	case err = <-errChan:
		return err
	case <-stopChan:
		return nil
	}
}

func runLogsSinceNow(dockerCli command.Cli, container string) error {
	ctx := context.Background()

	c, err := dockerCli.Client().ContainerInspect(ctx, container)
	if err != nil {
		return err
	}

	options := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      "0m",
		Follow:     true,
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
func parallelOperation(ctx context.Context, containers []string, op func(ctx context.Context, container string) error) chan error {
	if len(containers) == 0 {
		return nil
	}
	const defaultParallel int = 50
	sem := make(chan struct{}, defaultParallel)
	errChan := make(chan error)

	// make sure result is printed in correct order
	output := map[string]chan error{}
	for _, c := range containers {
		output[c] = make(chan error, 1)
	}
	go func() {
		for _, c := range containers {
			err := <-output[c]
			errChan <- err
		}
	}()

	go func() {
		for _, c := range containers {
			sem <- struct{}{} // Wait for active queue sem to drain.
			go func(container string) {
				output[container] <- op(ctx, container)
				<-sem
			}(c)
		}
	}()
	return errChan
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
