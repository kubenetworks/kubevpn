package dev

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	miekgdns "github.com/miekg/dns"
	"github.com/moby/term"
	v12 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"k8s.io/api/core/v1"
	v13 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/cp"
	"github.com/wencaiwulue/kubevpn/pkg/dns"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
)

type RunConfig struct {
	config           *container.Config
	hostConfig       *container.HostConfig
	networkingConfig *network.NetworkingConfig
	platform         *v12.Platform
	containerName    string
	k8sContainerName string
}

func ConvertKubeResourceToContainer(namespace string, temp v1.PodTemplateSpec, envMap map[string][]string, mountVolume map[string][]mount.Mount, dnsConfig *miekgdns.ClientConfig) (runConfigList Run) {
	spec := temp.Spec
	for _, c := range spec.Containers {
		var r RunConfig
		config := &container.Config{
			Hostname: func() string {
				var hostname = spec.Hostname
				if hostname == "" {
					for _, envEntry := range envMap[c.Name] {
						env := strings.Split(envEntry, "=")
						if len(env) == 2 && env[0] == "HOSTNAME" {
							hostname = env[1]
							break
						}
					}
				}
				return hostname
			}(),
			Domainname:      spec.Subdomain,
			User:            "",
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
			MacAddress:      "",
			OnBuild:         nil,
			Labels:          temp.Labels,
			StopSignal:      "",
			StopTimeout:     nil,
			Shell:           nil,
		}
		if temp.DeletionGracePeriodSeconds != nil {
			config.StopTimeout = (*int)(unsafe.Pointer(temp.DeletionGracePeriodSeconds))
		}
		hostConfig := &container.HostConfig{
			Binds:           []string{},
			ContainerIDFile: "",
			LogConfig:       container.LogConfig{},
			//NetworkMode:     "",
			PortBindings:    nil,
			RestartPolicy:   container.RestartPolicy{},
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
			Resources:       container.Resources{},
			Mounts:          mountVolume[c.Name],
			MaskedPaths:     nil,
			ReadonlyPaths:   nil,
			Init:            nil,
		}
		var portmap = nat.PortMap{}
		var portset = nat.PortSet{}
		for _, port := range c.Ports {
			port1 := nat.Port(fmt.Sprintf("%d/%s", port.ContainerPort, strings.ToLower(string(port.Protocol))))
			if port.HostPort != 0 {
				binding := []nat.PortBinding{{HostPort: strconv.FormatInt(int64(port.HostPort), 10)}}
				portmap[port1] = binding
			}
			portset[port1] = struct{}{}
		}
		hostConfig.PortBindings = portmap
		config.ExposedPorts = portset
		if c.SecurityContext != nil && c.SecurityContext.Capabilities != nil {
			hostConfig.CapAdd = append(hostConfig.CapAdd, *(*strslice.StrSlice)(unsafe.Pointer(&c.SecurityContext.Capabilities.Add))...)
			hostConfig.CapDrop = *(*strslice.StrSlice)(unsafe.Pointer(&c.SecurityContext.Capabilities.Drop))
		}
		var suffix string
		newUUID, err := uuid.NewUUID()
		if err == nil {
			suffix = strings.ReplaceAll(newUUID.String(), "-", "")[:5]
		}
		r.containerName = fmt.Sprintf("%s_%s_%s_%s", c.Name, namespace, "kubevpn", suffix)
		r.k8sContainerName = c.Name
		r.config = config
		r.hostConfig = hostConfig
		r.networkingConfig = &network.NetworkingConfig{EndpointsConfig: make(map[string]*network.EndpointSettings)}
		r.platform = /*&v12.Platform{Architecture: "amd64", OS: "linux"}*/ nil

		runConfigList = append(runConfigList, &r)
	}
	return runConfigList
}

func GetDNS(ctx context.Context, f util.Factory, ns, pod string) (*miekgdns.ClientConfig, error) {
	clientSet, err := f.KubernetesClientSet()
	if err != nil {
		return nil, err
	}
	_, err = clientSet.CoreV1().Pods(ns).Get(ctx, pod, v13.GetOptions{})
	if err != nil {
		return nil, err
	}
	config, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	client, err := f.RESTClient()
	if err != nil {
		return nil, err
	}

	fromPod, err := dns.GetDNSServiceIPFromPod(clientSet, client, config, pod, ns)
	if err != nil {
		return nil, err
	}
	return fromPod, nil
}

// GetVolume key format: [container name]-[volume mount name]
func GetVolume(ctx context.Context, f util.Factory, ns, pod string) (map[string][]mount.Mount, error) {
	clientSet, err := f.KubernetesClientSet()
	if err != nil {
		return nil, err
	}
	var get *v1.Pod
	get, err = clientSet.CoreV1().Pods(ns).Get(ctx, pod, v13.GetOptions{})
	if err != nil {
		return nil, err
	}
	result := map[string][]mount.Mount{}
	for _, c := range get.Spec.Containers {
		// if container name is vpn or envoy-proxy, not need to download volume
		if c.Name == config.ContainerSidecarVPN || c.Name == config.ContainerSidecarEnvoyProxy {
			continue
		}
		var m []mount.Mount
		for _, volumeMount := range c.VolumeMounts {
			if volumeMount.MountPath == "/tmp" {
				continue
			}
			join := filepath.Join(os.TempDir(), strconv.Itoa(rand.Int()))
			err = os.MkdirAll(join, 0755)
			if err != nil {
				return nil, err
			}
			if volumeMount.SubPath != "" {
				join = filepath.Join(join, volumeMount.SubPath)
			}
			handler.RollbackFuncList = append(handler.RollbackFuncList, func() {
				_ = os.RemoveAll(join)
			})
			// pod-namespace/pod-name:path
			remotePath := fmt.Sprintf("%s/%s:%s", ns, pod, volumeMount.MountPath)
			stdIn, stdOut, stdErr := term.StdStreams()
			copyOptions := cp.NewCopyOptions(genericclioptions.IOStreams{In: stdIn, Out: stdOut, ErrOut: stdErr})
			copyOptions.Container = c.Name
			copyOptions.MaxTries = 10
			err = copyOptions.Complete(f, &cobra.Command{}, []string{remotePath, join})
			if err != nil {
				return nil, err
			}
			err = copyOptions.Run()
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "failed to download volume %s path %s to %s, err: %v, ignore...\n", volumeMount.Name, remotePath, join, err)
				continue
			}
			m = append(m, mount.Mount{
				Type:   mount.TypeBind,
				Source: join,
				Target: volumeMount.MountPath,
			})
			fmt.Printf("%s:%s\n", join, volumeMount.MountPath)
		}
		result[c.Name] = m
	}
	return result, nil
}
