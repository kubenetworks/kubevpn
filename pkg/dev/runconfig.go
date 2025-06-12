package dev

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	typescontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type RunConfig struct {
	name string

	options []string
	image   string
	args    []string
	command []string

	//platform *v1.Platform
	//Options  RunOptions
	//Copts    ContainerOptions
}

type ConfigList []*RunConfig

func (l ConfigList) Remove(ctx context.Context, userAnotherContainerNet bool) error {
	for index, runConfig := range l {
		if !userAnotherContainerNet && index == len(l)-1 {
			output, err := NetworkDisconnect(ctx, runConfig.name)
			if err != nil {
				plog.G(ctx).Warnf("Failed to disconnect container network: %s: %v", string(output), err)
			}
		}
		output, err := ContainerRemove(ctx, runConfig.name)
		if err != nil {
			plog.G(ctx).Warnf("Failed to remove container: %s: %v", string(output), err)
		}
	}
	name := config.ConfigMapPodTrafficManager
	inspect, err := NetworkInspect(ctx, name)
	if err != nil {
		return err
	}
	if len(inspect.Containers) == 0 {
		return NetworkRemove(ctx, name)
	}
	return nil
}

func (l ConfigList) Run(ctx context.Context) error {
	for index := len(l) - 1; index >= 0; index-- {
		conf := l[index]

		err := RunContainer(ctx, conf)
		if err != nil {
			return err
		}

		if index != 0 {
			err = WaitDockerContainerRunning(ctx, conf.name)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// ConvertPodToContainerConfigList
/**
 if len(option.ContainerOptions.netMode.Value()) is not empty
 		--> use other container network, needs to clear dns config and exposePort config
 if len(option.ContainerOptions.netMode.Value()) is empty
		--> use last container(not dev container) network, other container network, needs to clear dns config and exposePort config
*/
func (option *Options) ConvertPodToContainerConfigList(
	ctx context.Context,
	temp corev1.PodTemplateSpec,
	conf *Config,
	hostConfig *HostConfig,
	envMap map[string]string,
	mountVolume map[string][]mount.Mount,
	dnsConfig *dns.ClientConfig,
) (configList ConfigList, err error) {
	inject.RemoveContainers(&temp)
	// move dev container to location first
	for index, c := range temp.Spec.Containers {
		if option.ContainerName == c.Name {
			temp.Spec.Containers[0], temp.Spec.Containers[index] = temp.Spec.Containers[index], temp.Spec.Containers[0]
			break
		}
	}

	var allPortMap = nat.PortMap{}
	var allPortSet = nat.PortSet{}
	for k, v := range hostConfig.PortBindings {
		if oldValue, ok := allPortMap[k]; ok {
			allPortMap[k] = append(oldValue, v...)
		} else {
			allPortMap[k] = v
		}
	}
	for k, v := range conf.ExposedPorts {
		allPortSet[k] = v
	}

	lastContainerIdx := len(temp.Spec.Containers) - 1
	lastContainerRandomName := util.Join(option.Namespace, temp.Spec.Containers[lastContainerIdx].Name, strings.ReplaceAll(uuid.New().String(), "-", "")[:5])
	for index, container := range temp.Spec.Containers {
		name := util.Join(option.Namespace, container.Name)
		randomName := util.Join(name, strings.ReplaceAll(uuid.New().String(), "-", "")[:5])
		var options = []string{
			"--env-file", envMap[name],
			"--domainname", temp.Spec.Subdomain,
			"--workdir", container.WorkingDir,
			"--cap-add", "SYS_PTRACE",
			"--cap-add", "SYS_ADMIN",
			"--cap-add", "SYS_PTRACE",
			"--cap-add", "SYS_ADMIN",
			"--security-opt", "apparmor=unconfined",
			"--security-opt", "seccomp=unconfined",
			"--pull", ConvertK8sImagePullPolicyToDocker(container.ImagePullPolicy),
			"--name", util.If(index == lastContainerIdx, lastContainerRandomName, randomName),
			"--user", "root",
			"--env", "LC_ALL=C.UTF-8",
		}
		for k, v := range temp.Labels {
			options = append(options, "--label", fmt.Sprintf("%s=%s", k, v))
		}
		if container.TTY {
			options = append(options, "--tty")
		}
		if ptr.Deref(ptr.Deref(container.SecurityContext, corev1.SecurityContext{}).Privileged, false) {
			options = append(options, "--privileged")
		}
		for _, m := range mountVolume[name] {
			options = append(options, "--volume", fmt.Sprintf("%s:%s", m.Source, m.Target))
		}

		for _, port := range container.Ports {
			p := nat.Port(fmt.Sprintf("%d/%s", port.ContainerPort, strings.ToLower(string(port.Protocol))))
			var portBinding nat.PortBinding
			if port.HostPort != 0 {
				portBinding = nat.PortBinding{HostPort: strconv.FormatInt(int64(port.HostPort), 10)}
			} else {
				portBinding = nat.PortBinding{HostPort: strconv.FormatInt(int64(port.ContainerPort), 10)}
			}
			if oldValue, ok := allPortMap[p]; ok {
				allPortMap[p] = append(oldValue, portBinding)
			} else {
				allPortMap[p] = []nat.PortBinding{portBinding}
			}
			allPortSet[p] = struct{}{}
		}

		// if netMode is empty, then 0 ~ last-1 use last container network
		if len(option.ContainerOptions.netMode.Value()) == 0 {
			// set last container
			if lastContainerIdx == index {
				options = append(options,
					"--dns-option", fmt.Sprintf("ndots=%d", dnsConfig.Ndots),
					"--hostname", GetEnvByKey(envMap[name], "HOSTNAME", container.Name),
				)
				for _, server := range dnsConfig.Servers {
					options = append(options, "--dns", server)
				}
				for _, search := range dnsConfig.Search {
					options = append(options, "--dns-search", search)
				}
				for p, bindings := range allPortMap {
					for _, binding := range bindings {
						options = append(options, "--publish", fmt.Sprintf("%s:%s", p.Port(), binding.HostPort))
					}
					options = append(options, "--expose", p.Port())
				}
				if hostConfig.PublishAllPorts {
					options = append(options, "--publish-all")
				}
				_, err = CreateNetwork(ctx, config.ConfigMapPodTrafficManager)
				if err != nil {
					plog.G(ctx).Errorf("Failed to create network: %v", err)
					return nil, err
				}
				plog.G(ctx).Infof("Create docker network %s", config.ConfigMapPodTrafficManager)
				options = append(options, "--network", config.ConfigMapPodTrafficManager)
			} else { // set 0 to last-1 container to use last container network
				options = append(options, "--network", util.ContainerNet(lastContainerRandomName))
				options = append(options, "--pid", util.ContainerNet(lastContainerRandomName))
			}
		} else { // set all containers to use network mode
			plog.G(ctx).Infof("Network mode is %s", option.ContainerOptions.netMode.NetworkMode())
			options = append(options, "--network", option.ContainerOptions.netMode.NetworkMode())
			if typescontainer.NetworkMode(option.ContainerOptions.netMode.NetworkMode()).IsContainer() {
				options = append(options, "--pid", option.ContainerOptions.netMode.NetworkMode())
			}
		}

		var r = RunConfig{
			name:    util.If(index == lastContainerIdx, lastContainerRandomName, randomName),
			options: util.If(index != 0, append(options, "--detach"), options),
			image:   util.If(index == 0 && option.DevImage != "", option.DevImage, container.Image),
			args:    util.If(index == 0 && len(conf.Cmd) != 0, conf.Cmd, container.Args),
			command: util.If(index == 0 && len(conf.Entrypoint) != 0, conf.Entrypoint, container.Command),
		}
		if index == 0 {
			MergeDockerOptions(&r, option, conf, hostConfig)
		}
		configList = append(configList, &r)
	}

	if hostConfig.AutoRemove {
		for index := range configList {
			configList[index].options = append(configList[index].options, "--rm")
		}
	}
	return configList, nil
}

func MergeDockerOptions(conf *RunConfig, options *Options, config *Config, hostConfig *HostConfig) {
	conf.options = append(conf.options, "--pull", options.RunOptions.Pull)
	if options.RunOptions.Platform != "" {
		conf.options = append(conf.options, "--platform", options.RunOptions.Platform)
	}
	if config.AttachStdin {
		conf.options = append(conf.options, "--attach", "STDIN")
	}
	if config.AttachStdout {
		conf.options = append(conf.options, "--attach", "STDOUT")
	}
	if config.AttachStderr {
		conf.options = append(conf.options, "--attach", "STDERR")
	}
	if config.Tty {
		conf.options = append(conf.options, "--tty")
	}
	if config.OpenStdin {
		conf.options = append(conf.options, "--interactive")
	}
	if hostConfig.Privileged {
		conf.options = append(conf.options, "--privileged")
	}
	for _, bind := range hostConfig.Binds {
		conf.options = append(conf.options, "--volume", bind)
	}

	// host config
	for _, domain := range options.ExtraRouteInfo.ExtraDomain {
		ips, err := net.LookupIP(domain)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if ip.To4() != nil {
				conf.options = append(conf.options, "--add-host", fmt.Sprintf("%s:%s", domain, ip.To4().String()))
				break
			}
		}
	}
}

func GetEnvByKey(filepath string, key string, defaultValue string) string {
	content, err := os.ReadFile(filepath)
	if err != nil {
		return defaultValue
	}

	for _, kv := range strings.Split(string(content), "\n") {
		env := strings.Split(kv, "=")
		if len(env) == 2 && env[0] == key {
			return env[1]
		}
	}
	return defaultValue
}
