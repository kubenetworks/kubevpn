package dev

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/archive"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/mesh"
	"github.com/wencaiwulue/kubevpn/pkg/tun"
	"github.com/wencaiwulue/kubevpn/pkg/util"
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
	Aliases      opts.ListOpts
	Links        opts.ListOpts
	LinkLocalIPs opts.ListOpts
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
	if mode.IsBridge() || mode.IsHost() || mode.IsContainer() || mode.IsNone() {
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
		networkID, err = createNetwork(ctx, cli, list[0].containerName)
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

func createNetwork(ctx context.Context, cli *client.Client, networkName string) (string, error) {
	getInterface, err := tun.GetInterface()
	if err != nil {
		return "", err
	}
	addrs, err := getInterface.Addrs()
	if err != nil {
		return "", err
	}
	cidr, _, err := net.ParseCIDR(addrs[0].String())
	if err != nil {
		return "", err
	}
	by := map[string]string{"created_by": config.ConfigMapPodTrafficManager}
	create, err := cli.NetworkCreate(ctx, networkName, types.NetworkCreate{
		Driver: "bridge",
		Scope:  "local",
		IPAM: &network.IPAM{
			Driver:  "",
			Options: nil,
			Config: []network.IPAMConfig{
				{
					Subnet:  config.CIDR.String(),
					Gateway: cidr.String(),
				},
			},
		},
		Internal: true,
		Labels:   by,
	})
	if err != nil {
		if errdefs.IsForbidden(err) {
			list, _ := cli.NetworkList(ctx, types.NetworkListOptions{})
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
	for _, runConfig := range r {
		_, err = cli.NetworkInspect(ctx, runConfig.containerName, types.NetworkInspectOptions{})
		if err == nil {
			err = cli.NetworkRemove(ctx, runConfig.containerName)
			if err != nil {
				log.Debug(err)
			}
		}
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
	for _, config := range r {
		var id string
		id, err = run(ctx, config, cli, c)
		if err != nil {
			// try another way to startup container
			log.Infof("occur err: %v, try another way to startup container...", err)
			config.hostConfig.Mounts = nil
			id, err = run(ctx, config, cli, c)
			if err != nil {
				return err
			}
			err = r.copyToContainer(ctx, volume[config.k8sContainerName], cli, id)
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
