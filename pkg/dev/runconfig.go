package dev

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types"
	typescommand "github.com/docker/docker/api/types/container"
	typescontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	"github.com/miekg/dns"
	"github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type RunConfig struct {
	containerName    string
	k8sContainerName string

	config           *typescontainer.Config
	hostConfig       *typescontainer.HostConfig
	networkingConfig *network.NetworkingConfig
	platform         *v1.Platform

	Options runOptions
	Copts   *containerOptions
}

type ConfigList []*RunConfig

func (c ConfigList) Remove(ctx context.Context, cli *client.Client) error {
	var remove = false
	for _, runConfig := range c {
		if runConfig.hostConfig.AutoRemove {
			remove = true
			break
		}
	}
	if !remove {
		return nil
	}
	for _, runConfig := range c {
		err := cli.NetworkDisconnect(ctx, runConfig.containerName, runConfig.containerName, true)
		if err != nil {
			log.Debug(err)
		}
		err = cli.ContainerRemove(ctx, runConfig.containerName, typescontainer.RemoveOptions{Force: true})
		if err != nil {
			log.Debug(err)
		}
	}
	inspect, err := cli.NetworkInspect(ctx, config.ConfigMapPodTrafficManager, types.NetworkInspectOptions{})
	if err != nil {
		return err
	}
	if len(inspect.Containers) == 0 {
		return cli.NetworkRemove(ctx, config.ConfigMapPodTrafficManager)
	}
	return nil
}

func (c ConfigList) Run(ctx context.Context, volume map[string][]mount.Mount, cli *client.Client, dockerCli *command.DockerCli) error {
	for index := len(c) - 1; index >= 0; index-- {
		runConfig := c[index]
		if index == 0 {
			err := runAndAttach(ctx, runConfig, dockerCli)
			if err != nil {
				return err
			}
		} else {
			id, err := run(ctx, runConfig, cli, dockerCli)
			if err != nil {
				// try another way to startup container
				log.Infof("occur err: %v, try to use copy to startup container...", err)
				runConfig.hostConfig.Mounts = nil
				id, err = run(ctx, runConfig, cli, dockerCli)
				if err != nil {
					return err
				}
				err = util.CopyVolumeIntoContainer(ctx, volume[runConfig.k8sContainerName], cli, id)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func convertKubeResourceToContainer(ns string, temp v12.PodTemplateSpec, envMap map[string][]string, mountVolume map[string][]mount.Mount, dnsConfig *dns.ClientConfig) (list ConfigList) {
	getHostname := func(containerName string) string {
		for _, envEntry := range envMap[containerName] {
			env := strings.Split(envEntry, "=")
			if len(env) == 2 && env[0] == "HOSTNAME" {
				return env[1]
			}
		}
		return temp.Spec.Hostname
	}

	for _, c := range temp.Spec.Containers {
		containerConf := &typescontainer.Config{
			Hostname:        getHostname(c.Name),
			Domainname:      temp.Spec.Subdomain,
			User:            "root",
			AttachStdin:     false,
			AttachStdout:    false,
			AttachStderr:    false,
			ExposedPorts:    nil,
			Tty:             c.TTY,
			OpenStdin:       c.Stdin,
			StdinOnce:       false,
			Env:             envMap[c.Name],
			Cmd:             c.Args,
			Healthcheck:     nil,
			ArgsEscaped:     false,
			Image:           c.Image,
			Volumes:         nil,
			WorkingDir:      c.WorkingDir,
			Entrypoint:      c.Command,
			NetworkDisabled: false,
			OnBuild:         nil,
			Labels:          temp.Labels,
			StopSignal:      "",
			StopTimeout:     nil,
			Shell:           nil,
		}
		if temp.DeletionGracePeriodSeconds != nil {
			containerConf.StopTimeout = (*int)(unsafe.Pointer(temp.DeletionGracePeriodSeconds))
		}
		hostConfig := &typescontainer.HostConfig{
			Binds:           []string{},
			ContainerIDFile: "",
			LogConfig:       typescontainer.LogConfig{},
			//NetworkMode:     "",
			PortBindings:    nil,
			RestartPolicy:   typescontainer.RestartPolicy{},
			AutoRemove:      false,
			VolumeDriver:    "",
			VolumesFrom:     nil,
			ConsoleSize:     [2]uint{},
			CapAdd:          strslice.StrSlice{"SYS_PTRACE", "SYS_ADMIN"}, // for dlv
			CgroupnsMode:    "",
			DNS:             dnsConfig.Servers,
			DNSOptions:      []string{fmt.Sprintf("ndots=%d", dnsConfig.Ndots)},
			DNSSearch:       dnsConfig.Search,
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
			Resources:       typescontainer.Resources{},
			Mounts:          mountVolume[c.Name],
			MaskedPaths:     nil,
			ReadonlyPaths:   nil,
			Init:            nil,
		}
		var portMap = nat.PortMap{}
		var portSet = nat.PortSet{}
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
		hostConfig.PortBindings = portMap
		containerConf.ExposedPorts = portSet
		if c.SecurityContext != nil && c.SecurityContext.Capabilities != nil {
			hostConfig.CapAdd = append(hostConfig.CapAdd, *(*strslice.StrSlice)(unsafe.Pointer(&c.SecurityContext.Capabilities.Add))...)
			hostConfig.CapDrop = *(*strslice.StrSlice)(unsafe.Pointer(&c.SecurityContext.Capabilities.Drop))
		}
		var suffix string
		newUUID, err := uuid.NewUUID()
		if err == nil {
			suffix = strings.ReplaceAll(newUUID.String(), "-", "")[:5]
		}

		var r RunConfig
		r.containerName = fmt.Sprintf("%s_%s_%s_%s", c.Name, ns, "kubevpn", suffix)
		r.k8sContainerName = c.Name
		r.config = containerConf
		r.hostConfig = hostConfig
		r.networkingConfig = &network.NetworkingConfig{EndpointsConfig: make(map[string]*network.EndpointSettings)}
		r.platform = nil
		list = append(list, &r)
	}
	return list
}

func run(ctx context.Context, runConfig *RunConfig, cli *client.Client, c *command.DockerCli) (id string, err error) {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	var config = runConfig.config
	var hostConfig = runConfig.hostConfig
	var platform = runConfig.platform
	var networkConfig = runConfig.networkingConfig
	var name = runConfig.containerName

	var needPull bool
	var img types.ImageInspect
	img, _, err = cli.ImageInspectWithRaw(ctx, config.Image)
	if errdefs.IsNotFound(err) {
		log.Infof("needs to pull image %s", config.Image)
		needPull = true
		err = nil
	} else if err != nil {
		log.Errorf("image inspect failed: %v", err)
		return
	}
	if platform != nil && platform.Architecture != "" && platform.OS != "" {
		if img.Os != platform.OS || img.Architecture != platform.Architecture {
			needPull = true
		}
	}
	if needPull {
		err = util.PullImage(ctx, runConfig.platform, cli, c, config.Image, nil)
		if err != nil {
			log.Errorf("Failed to pull image: %s, err: %s", config.Image, err)
			return
		}
	}

	var create typescommand.CreateResponse
	create, err = cli.ContainerCreate(ctx, config, hostConfig, networkConfig, platform, name)
	if err != nil {
		log.Errorf("Failed to create container: %s, err: %s", name, err)
		return
	}
	id = create.ID
	log.Infof("Created container: %s", name)
	defer func() {
		if err != nil && runConfig.hostConfig.AutoRemove {
			_ = cli.ContainerRemove(ctx, id, typescontainer.RemoveOptions{Force: true})
		}
	}()

	err = cli.ContainerStart(ctx, create.ID, typescontainer.StartOptions{})
	if err != nil {
		log.Errorf("failed to startup container %s: %v", name, err)
		return
	}
	log.Infof("Wait container %s to be running...", name)
	var inspect types.ContainerJSON
	ctx2, cancelFunc := context.WithTimeout(ctx, time.Minute*5)
	wait.UntilWithContext(ctx2, func(ctx context.Context) {
		inspect, err = cli.ContainerInspect(ctx, create.ID)
		if errdefs.IsNotFound(err) {
			cancelFunc()
			return
		} else if err != nil {
			cancelFunc()
			return
		}
		if inspect.State != nil && (inspect.State.Status == "exited" || inspect.State.Status == "dead" || inspect.State.Dead) {
			cancelFunc()
			err = errors.New(fmt.Sprintf("container status: %s", inspect.State.Status))
			return
		}
		if inspect.State != nil && inspect.State.Running {
			cancelFunc()
			return
		}
	}, time.Second)
	if err != nil {
		log.Errorf("failed to wait container to be ready: %v", err)
		_ = runLogsSinceNow(c, id, false)
		return
	}

	log.Infof("Container %s is running now", name)
	return
}

func runAndAttach(ctx context.Context, runConfig *RunConfig, cli *command.DockerCli) error {
	c := &containerConfig{
		Config:           runConfig.config,
		HostConfig:       runConfig.hostConfig,
		NetworkingConfig: runConfig.networkingConfig,
	}
	return runContainer(ctx, cli, &runConfig.Options, runConfig.Copts, c)
}
