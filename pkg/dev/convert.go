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
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	v13 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/cp"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
	util2 "github.com/wencaiwulue/kubevpn/pkg/util"
)

type RunConfig struct {
	containerName    string
	k8sContainerName string

	config           *container.Config
	hostConfig       *container.HostConfig
	networkingConfig *network.NetworkingConfig
	platform         *v12.Platform

	Options RunOptions
	Copts   *ContainerOptions
}

func ConvertKubeResourceToContainer(namespace string, temp v1.PodTemplateSpec, envMap map[string][]string, mountVolume map[string][]mount.Mount, dnsConfig *miekgdns.ClientConfig) (runConfigList ConfigList) {
	spec := temp.Spec
	for _, c := range spec.Containers {
		tmpConfig := &container.Config{
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
			MacAddress:      "",
			OnBuild:         nil,
			Labels:          temp.Labels,
			StopSignal:      "",
			StopTimeout:     nil,
			Shell:           nil,
		}
		if temp.DeletionGracePeriodSeconds != nil {
			tmpConfig.StopTimeout = (*int)(unsafe.Pointer(temp.DeletionGracePeriodSeconds))
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
			} else {
				binding := []nat.PortBinding{{HostPort: strconv.FormatInt(int64(port.ContainerPort), 10)}}
				portmap[port1] = binding
			}
			portset[port1] = struct{}{}
		}
		hostConfig.PortBindings = portmap
		tmpConfig.ExposedPorts = portset
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
		r.containerName = fmt.Sprintf("%s_%s_%s_%s", c.Name, namespace, "kubevpn", suffix)
		r.k8sContainerName = c.Name
		r.config = tmpConfig
		r.hostConfig = hostConfig
		r.networkingConfig = &network.NetworkingConfig{EndpointsConfig: make(map[string]*network.EndpointSettings)}
		r.platform = nil

		runConfigList = append(runConfigList, &r)
	}
	return runConfigList
}

func GetDNS(ctx context.Context, f util.Factory, ns, pod string) (*miekgdns.ClientConfig, error) {
	clientSet, err := f.KubernetesClientSet()
	if err != nil {
		err = errors.Wrap(err, "Failed to get Kubernetes client set.")
		return nil, err
	}
	_, err = clientSet.CoreV1().Pods(ns).Get(ctx, pod, v13.GetOptions{})
	if err != nil {
		err = errors.Wrap(err, "Failed to get pods.")
		return nil, err
	}
	config, err := f.ToRESTConfig()
	if err != nil {
		err = errors.Wrap(err, "Failed to get REST configuration.")
		return nil, err
	}

	client, err := f.RESTClient()
	if err != nil {
		err = errors.Wrap(err, "Failed to get REST client.")
		return nil, err
	}

	clientConfig, err := util2.GetDNSServiceIPFromPod(clientSet, client, config, pod, ns)
	if err != nil {
		err = errors.Wrap(err, "Failed to get DNS service IP from pod.")
		return nil, err
	}
	return clientConfig, nil
}

// GetVolume key format: [container name]-[volume mount name]
func GetVolume(ctx context.Context, f util.Factory, ns, pod string, d *Options) (map[string][]mount.Mount, error) {
	clientSet, err := f.KubernetesClientSet()
	if err != nil {
		err = errors.Wrap(err, "Failed to get Kubernetes client set.")
		return nil, err
	}
	var get *v1.Pod
	get, err = clientSet.CoreV1().Pods(ns).Get(ctx, pod, v13.GetOptions{})
	if err != nil {
		err = errors.Wrap(err, "Failed to get pods.")
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
				err = errors.Wrap(err, "Failed to create directory.")
				return nil, err
			}
			if volumeMount.SubPath != "" {
				join = filepath.Join(join, volumeMount.SubPath)
			}
			d.AddRollbackFunc(func() error {
				return os.RemoveAll(join)
			})
			// pod-namespace/pod-name:path
			remotePath := fmt.Sprintf("%s/%s:%s", ns, pod, volumeMount.MountPath)
			stdIn, stdOut, stdErr := term.StdStreams()
			copyOptions := cp.NewCopyOptions(genericclioptions.IOStreams{In: stdIn, Out: stdOut, ErrOut: stdErr})
			copyOptions.Container = c.Name
			copyOptions.MaxTries = 10
			err = copyOptions.Complete(f, &cobra.Command{}, []string{remotePath, join})
			if err != nil {
				err = errors.Wrap(err, "Failed to complete copy options.")
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
			log.Infof("%s:%s", join, volumeMount.MountPath)
		}
		result[c.Name] = m
	}
	return result, nil
}
