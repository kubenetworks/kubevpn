package dev

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unsafe"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types"
	typescontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/miekg/dns"
	"github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"
	v12 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type RunConfig struct {
	name string

	config           *typescontainer.Config
	hostConfig       *typescontainer.HostConfig
	networkingConfig *network.NetworkingConfig
	platform         *v1.Platform

	Options RunOptions
	Copts   ContainerOptions
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
		err := cli.NetworkDisconnect(ctx, runConfig.name, runConfig.name, true)
		if err != nil {
			log.Warnf("Failed to disconnect container network: %v", err)
		}
		err = cli.ContainerRemove(ctx, runConfig.name, typescontainer.RemoveOptions{Force: true})
		if err != nil {
			log.Warnf("Failed to remove container: %v", err)
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
			err := runContainer(ctx, dockerCli, runConfig)
			if err != nil {
				return err
			}
		}
		_, err := run(ctx, cli, dockerCli, runConfig)
		if err != nil {
			// try to copy volume into container, why?
			runConfig.hostConfig.Mounts = nil
			id, err1 := run(ctx, cli, dockerCli, runConfig)
			if err1 != nil {
				// return first error
				return err
			}
			err = util.CopyVolumeIntoContainer(ctx, volume[runConfig.name], cli, id)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func ConvertPodToContainer(ns string, temp v12.PodTemplateSpec, envMap map[string][]string, mountVolume map[string][]mount.Mount, dnsConfig *dns.ClientConfig) (list ConfigList) {
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
			Hostname:        getHostname(util.Join(ns, c.Name)),
			Domainname:      temp.Spec.Subdomain,
			User:            "root",
			AttachStdin:     c.Stdin,
			AttachStdout:    false,
			AttachStderr:    false,
			ExposedPorts:    nil,
			Tty:             c.TTY,
			OpenStdin:       c.Stdin,
			StdinOnce:       c.StdinOnce,
			Env:             envMap[util.Join(ns, c.Name)],
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
			Privileged:      ptr.Deref(ptr.Deref(c.SecurityContext, v12.SecurityContext{}).Privileged, false),
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
			Mounts:          mountVolume[util.Join(ns, c.Name)],
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

		var r = RunConfig{
			name:             util.Join(ns, c.Name),
			config:           containerConf,
			hostConfig:       hostConfig,
			networkingConfig: &network.NetworkingConfig{EndpointsConfig: make(map[string]*network.EndpointSettings)},
			platform:         nil,
			Options:          RunOptions{Pull: PullImageMissing},
		}

		list = append(list, &r)
	}

	return list
}

func MergeDockerOptions(list ConfigList, options *Options, config *Config, hostConfig *HostConfig) {
	conf := list[0]
	conf.Options = options.RunOptions
	conf.Copts = *options.ContainerOptions

	if options.RunOptions.Platform != "" {
		p, _ := platforms.Parse(options.RunOptions.Platform)
		conf.platform = &p
	}

	// container config
	var entrypoint = conf.config.Entrypoint
	var args = conf.config.Cmd
	// if special --entrypoint, then use it
	if len(config.Entrypoint) != 0 {
		entrypoint = config.Entrypoint
		args = config.Cmd
	}
	if len(config.Cmd) != 0 {
		args = config.Cmd
	}
	conf.config.Entrypoint = entrypoint
	conf.config.Cmd = args
	if options.DevImage != "" {
		conf.config.Image = options.DevImage
	}
	conf.config.Volumes = util.Merge[string, struct{}](conf.config.Volumes, config.Volumes)
	for k, v := range config.ExposedPorts {
		if _, found := conf.config.ExposedPorts[k]; !found {
			conf.config.ExposedPorts[k] = v
		}
	}
	conf.config.StdinOnce = config.StdinOnce
	conf.config.AttachStdin = config.AttachStdin
	conf.config.AttachStdout = config.AttachStdout
	conf.config.AttachStderr = config.AttachStderr
	conf.config.Tty = config.Tty
	conf.config.OpenStdin = config.OpenStdin

	// host config
	var hosts []string
	for _, domain := range options.ExtraRouteInfo.ExtraDomain {
		ips, err := net.LookupIP(domain)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if ip.To4() != nil {
				hosts = append(hosts, fmt.Sprintf("%s:%s", domain, ip.To4().String()))
				break
			}
		}
	}
	conf.hostConfig.ExtraHosts = hosts
	conf.hostConfig.AutoRemove = hostConfig.AutoRemove
	conf.hostConfig.Privileged = hostConfig.Privileged
	conf.hostConfig.PublishAllPorts = hostConfig.PublishAllPorts
	conf.hostConfig.Mounts = append(conf.hostConfig.Mounts, hostConfig.Mounts...)
	conf.hostConfig.Binds = append(conf.hostConfig.Binds, hostConfig.Binds...)
	for port, bindings := range hostConfig.PortBindings {
		if v, ok := conf.hostConfig.PortBindings[port]; ok {
			conf.hostConfig.PortBindings[port] = append(v, bindings...)
		} else {
			conf.hostConfig.PortBindings[port] = bindings
		}
	}
}
