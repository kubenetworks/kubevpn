package run

import (
	"context"
	"fmt"
	"net"
	"path/filepath"

	typescontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"
	"github.com/joho/godotenv"
	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type RunConfig struct {
	name    string
	options []string
	image   string
	args    []string
	command []string
}

type ConfigList []*RunConfig

func (l ConfigList) Remove(ctx context.Context, useAnotherContainerNet bool) error {
	for index, runConfig := range l {
		if !useAnotherContainerNet && index == len(l)-1 {
			output, err := util.NetworkDisconnect(ctx, runConfig.name)
			if err != nil {
				plog.G(ctx).Warnf("Failed to disconnect container network: %s: %v", string(output), err)
			}
		}
		output, err := util.ContainerRemove(ctx, runConfig.name)
		if err != nil {
			plog.G(ctx).Warnf("Failed to remove container: %s: %v", string(output), err)
		}
	}
	name := config.ConfigMapPodTrafficManager
	inspect, err := util.NetworkInspect(ctx, name)
	if err != nil {
		return err
	}
	if len(inspect.Containers) == 0 {
		return util.NetworkRemove(ctx, name)
	}
	return nil
}

// Run starts containers in reverse order: sidecars first (detached), dev container last (foreground).
func (l ConfigList) Run(ctx context.Context) error {
	for index := len(l) - 1; index >= 0; index-- {
		conf := l[index]
		if err := util.RunContainer(ctx, convertToDockerArgs(conf)); err != nil {
			return err
		}
		if index != 0 {
			if err := util.WaitDockerContainerRunning(ctx, conf.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// ConvertPodToContainerConfigList converts a K8s PodTemplateSpec into Docker container configs.
//
// Container layout:
//   - index 0: "dev" container (user interactive, foreground)
//   - index 1..N: sidecar containers (detached)
//
// Network topology (when no explicit --network is set):
//   - The last container owns the network namespace (DNS, ports, docker network)
//   - All other containers attach to it via --network container:<lastName>
func (option *Options) ConvertPodToContainerConfigList(
	ctx context.Context,
	podCtx *PodContext,
	conf *Config,
	hostConfig *HostConfig,
) (ConfigList, error) {
	temp := *podCtx.TemplateSpec
	inject.RemoveContainers(&temp.Spec)
	moveDevContainerFirst(&temp, option.ContainerName)

	portMap, portSet := collectExposedPorts(conf, hostConfig)
	lastIdx := len(temp.Spec.Containers) - 1
	lastName := util.Join(option.Namespace, temp.Spec.Containers[lastIdx].Name, randomSuffix())
	hasUserNetMode := len(option.ContainerOptions.netMode.Value()) != 0
	userNetMode := option.ContainerOptions.netMode.NetworkMode()

	var configList ConfigList
	for index, container := range temp.Spec.Containers {
		isLast := index == lastIdx
		isDev := index == 0
		envKey := util.Join(option.Namespace, container.Name)
		containerName := util.If(isLast, lastName, util.Join(envKey, randomSuffix()))

		opts, err := buildContainerOptions(container, temp, envKey, containerName, podCtx.EnvMap, podCtx.MountVolume)
		if err != nil {
			return nil, err
		}

		addContainerPortBindings(container.Ports, portMap, portSet)

		if err := appendNetworkOptions(&opts, ctx, index, lastIdx, lastName, hasUserNetMode, userNetMode, podCtx.DNSConfig, podCtx.EnvMap[envKey], container.Name, portMap, hostConfig); err != nil {
			return nil, err
		}

		r := RunConfig{
			name:    containerName,
			options: util.If(!isDev, append(opts, "--detach"), opts),
			image:   util.If(isDev && option.DevImage != "", option.DevImage, container.Image),
			args:    util.If(isDev && len(conf.Cmd) != 0, conf.Cmd, container.Args),
			command: util.If(isDev && len(conf.Entrypoint) != 0, conf.Entrypoint, container.Command),
		}
		if isDev {
			mergeDevContainerOptions(&r, option, conf, hostConfig)
		}
		configList = append(configList, &r)
	}

	if hostConfig.AutoRemove {
		for i := range configList {
			configList[i].options = append(configList[i].options, "--rm")
		}
	}
	return configList, nil
}

func moveDevContainerFirst(temp *corev1.PodTemplateSpec, containerName string) {
	for i, c := range temp.Spec.Containers {
		if c.Name == containerName {
			temp.Spec.Containers[0], temp.Spec.Containers[i] = temp.Spec.Containers[i], temp.Spec.Containers[0]
			return
		}
	}
}

func collectExposedPorts(conf *Config, hostConfig *HostConfig) (nat.PortMap, nat.PortSet) {
	portMap := nat.PortMap{}
	portSet := nat.PortSet{}
	for k, v := range conf.ExposedPorts {
		portSet[k] = v
	}
	for k, v := range hostConfig.PortBindings {
		portMap[k] = v
	}
	return portMap, portSet
}

func buildContainerOptions(
	container corev1.Container,
	temp corev1.PodTemplateSpec,
	envKey, containerName string,
	envMap map[string]string,
	mountVolume map[string][]mount.Mount,
) ([]string, error) {
	localEnv, err := filepath.Abs(envMap[envKey])
	if err != nil {
		return nil, err
	}
	opts := []string{
		"--env-file", localEnv,
		"--domainname", temp.Spec.Subdomain,
		"--workdir", container.WorkingDir,
	}
	opts = append(opts, containerSecurityOpts()...)
	opts = append(opts,
		"--pull", ConvertK8sImagePullPolicyToDocker(container.ImagePullPolicy),
		"--name", containerName,
		"--user", "root",
		"--env", "LC_ALL=C.UTF-8",
	)
	for k, v := range temp.Labels {
		opts = append(opts, "--label", fmt.Sprintf("%s=%s", k, v))
	}
	if container.TTY {
		opts = append(opts, "--tty")
	}
	if ptr.Deref(ptr.Deref(container.SecurityContext, corev1.SecurityContext{}).Privileged, false) {
		opts = append(opts, "--privileged")
	}
	for _, m := range mountVolume[envKey] {
		localDir, err := filepath.Abs(m.Source)
		if err != nil {
			return nil, err
		}
		opts = append(opts, "--volume", fmt.Sprintf("%s:%s", localDir, m.Target))
	}
	return opts, nil
}

func appendNetworkOptions(
	opts *[]string,
	ctx context.Context,
	index, lastIdx int,
	lastName string,
	hasUserNetMode bool,
	userNetMode string,
	dnsConfig *dns.ClientConfig,
	envFilePath, containerName string,
	portMap nat.PortMap,
	hostConfig *HostConfig,
) error {
	if hasUserNetMode {
		netMode := typescontainer.NetworkMode(userNetMode)
		*opts = append(*opts, "--network", string(netMode))
		if netMode.IsContainer() {
			*opts = append(*opts, "--pid", string(netMode))
		}
		return nil
	}

	if index == lastIdx {
		return appendNetworkOwnerOptions(opts, ctx, dnsConfig, envFilePath, containerName, portMap, hostConfig)
	}

	*opts = append(*opts, "--network", util.ContainerNet(lastName))
	*opts = append(*opts, "--pid", util.ContainerNet(lastName))
	return nil
}

// appendNetworkOwnerOptions configures the "network owner" container with DNS, ports, and docker network.
func appendNetworkOwnerOptions(
	opts *[]string,
	ctx context.Context,
	dnsConfig *dns.ClientConfig,
	envFilePath, containerName string,
	portMap nat.PortMap,
	hostConfig *HostConfig,
) error {
	*opts = append(*opts,
		"--dns-option", fmt.Sprintf("ndots=%d", dnsConfig.Ndots),
		"--hostname", getEnvByKey(envFilePath, "HOSTNAME", containerName),
	)
	for _, server := range dnsConfig.Servers {
		*opts = append(*opts, "--dns", server)
	}
	for _, search := range dnsConfig.Search {
		*opts = append(*opts, "--dns-search", search)
	}
	for p, bindings := range portMap {
		for _, binding := range bindings {
			*opts = append(*opts, "--publish", fmt.Sprintf("%s:%s", binding.HostPort, p.Port()))
		}
		*opts = append(*opts, "--expose", p.Port())
	}
	if hostConfig.PublishAllPorts {
		*opts = append(*opts, "--publish-all")
	}
	_, err := util.CreateNetwork(ctx, config.ConfigMapPodTrafficManager)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create network: %v", err)
		return err
	}
	plog.G(ctx).Infof("Create docker network %s", config.ConfigMapPodTrafficManager)
	*opts = append(*opts, "--network", config.ConfigMapPodTrafficManager)
	return nil
}

// mergeDevContainerOptions applies user-specified Docker options and resolves
// extra domains to --add-host entries for the dev (foreground) container.
func mergeDevContainerOptions(conf *RunConfig, options *Options, config *Config, hostConfig *HostConfig) {
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
	appendExtraHostEntries(conf, options.ExtraRouteInfo.ExtraDomain)
}

// appendExtraHostEntries resolves extra domains to IPs and adds --add-host entries.
func appendExtraHostEntries(conf *RunConfig, domains []string) {
	for _, domain := range domains {
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

func getEnvByKey(filepath string, key string, defaultValue string) string {
	parse, err := godotenv.Read(filepath)
	if err != nil {
		return defaultValue
	}
	value, ok := parse[key]
	if !ok {
		return defaultValue
	}
	return value
}
