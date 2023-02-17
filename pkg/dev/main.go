package dev

import (
	"context"
	"math/rand"
	"sort"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"

	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/mesh"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type Options struct {
	Headers       map[string]string
	Namespace     string
	Workload      string
	Factory       cmdutil.Factory
	ContainerName string

	// docker options
	Platform string
	//Pull         string // always, missing, never
	PublishAll   bool
	Entrypoint   string
	DockerImage  string
	Publish      opts.ListOpts
	Expose       opts.ListOpts
	ExtraHosts   opts.ListOpts
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

	env, err := GetEnv(ctx, d.Factory, d.Namespace, pod)
	if err != nil {
		return err
	}
	volume, err := GetVolume(ctx, d.Factory, d.Namespace, pod)
	if err != nil {
		return err
	}
	dns, err := GetDNS(ctx, d.Factory, d.Namespace, pod)
	if err != nil {
		return err
	}

	mesh.RemoveContainers(templateSpec)
	list := ConvertKubeResourceToContainer(d.Namespace, *templateSpec, env, volume, dns)
	err = fillOptions(list, d)
	if err != nil {
		return err
	}

	// skip first
	for _, config := range list[1:] {
		// remove expose port
		config.config.ExposedPorts = nil
		config.hostConfig.NetworkMode = containertypes.NetworkMode("container:" + list[0].containerName)
		config.hostConfig.PidMode = containertypes.PidMode("container:" + list[0].containerName)
		config.hostConfig.PortBindings = nil

		// remove dns
		config.hostConfig.DNS = nil
		config.hostConfig.DNSOptions = nil
		config.hostConfig.DNSSearch = nil
		config.hostConfig.PublishAllPorts = false
		config.config.Hostname = ""
	}

	handler.RollbackFuncList = append(handler.RollbackFuncList, func() {
		_ = list.Remove(ctx)
	})
	err = list.Run(ctx)
	if err != nil {
		return err
	}
	_, cli, err := GetClient()
	if err != nil {
		return err
	}
	return terminal(list[0].containerName, cli)
}

type Run []*RunConfig

func (r Run) Remove(ctx context.Context) error {
	cli, _, err := GetClient()
	if err != nil {
		return err
	}

	for _, config := range r {
		//err = cli.NetworkDisconnect(ctx, config.networkName, config.containerName, true)
		//if err != nil {
		//	log.Errorln(err)
		//}
		err = cli.ContainerRemove(ctx, config.containerName, types.ContainerRemoveOptions{Force: true})
		if err != nil {
			log.Errorln(err)
		}
	}
	//for _, config := range r {
	//	_, err = cli.NetworkInspect(ctx, config.networkName, types.NetworkInspectOptions{})
	//	if err == nil {
	//		err = cli.NetworkRemove(ctx, config.networkName)
	//		if err != nil {
	//			log.Errorln(err)
	//		}
	//	}
	//}
	return nil
}

func GetClient() (*client.Client, *command.DockerCli, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, nil, err
	}
	var dockerCli *command.DockerCli
	dockerCli, err = command.NewDockerCli(command.WithAPIClient(cli))
	if err != nil {
		return nil, nil, err
	}
	err = dockerCli.Initialize(flags.NewClientOptions())
	if err != nil {
		return nil, nil, err
	}
	return cli, dockerCli, nil
}

func (r Run) Run(ctx context.Context) error {
	cli, _, err := GetClient()
	if err != nil {
		return err
	}
	for _, config := range r {
		err = run(ctx, config, cli)
		if err != nil {
			return err
		}
	}
	return nil
}
