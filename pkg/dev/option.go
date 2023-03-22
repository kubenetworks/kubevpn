package dev

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/docker/cli/cli/compose/loader"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types/container"
	mounttypes "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	v12 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func fillOptions(r Run, copts Options) error {
	if copts.ContainerName != "" {
		var index = -1
		for i, config := range r {
			if config.k8sContainerName == copts.ContainerName {
				index = i
				break
			}
		}
		if index != -1 {
			r[0], r[index] = r[index], r[0]
		}
	}

	config := r[0]
	config.hostConfig.PublishAllPorts = copts.PublishAll

	if copts.DockerImage != "" {
		config.config.Image = copts.DockerImage
	}

	if copts.Entrypoint != "" {
		if strings.Count(copts.Entrypoint, " ") != 0 {
			split := strings.Split(copts.Entrypoint, " ")
			config.config.Entrypoint = split
		} else {
			config.config.Entrypoint = strslice.StrSlice{copts.Entrypoint}
		}
		config.config.Cmd = []string{}
	}
	if copts.Platform != "" {
		split := strings.Split(copts.Platform, "/")
		if len(split) != 2 {
			return errors.Errorf("invalid port format for --platform: %s", copts.Platform)
		}
		config.platform = &v12.Platform{
			OS:           split[0],
			Architecture: split[1],
		}
	}

	// collect all the environment variables for the container
	envVariables, err := opts.ReadKVEnvStrings([]string{}, copts.Env.GetAll())
	if err != nil {
		return err
	}
	config.config.Env = append(config.config.Env, envVariables...)

	publishOpts := copts.Publish.GetAll()
	var (
		ports         map[nat.Port]struct{}
		portBindings  map[nat.Port][]nat.PortBinding
		convertedOpts []string
	)

	convertedOpts, err = convertToStandardNotation(publishOpts)
	if err != nil {
		return err
	}

	ports, portBindings, err = nat.ParsePortSpecs(convertedOpts)
	if err != nil {
		return err
	}

	// Merge in exposed ports to the map of published ports
	for _, e := range copts.Expose.GetAll() {
		if strings.Contains(e, ":") {
			return errors.Errorf("invalid port format for --expose: %s", e)
		}
		// support two formats for expose, original format <portnum>/[<proto>]
		// or <startport-endport>/[<proto>]
		proto, port := nat.SplitProtoPort(e)
		// parse the start and end port and create a sequence of ports to expose
		// if expose a port, the start and end port are the same
		start, end, err := nat.ParsePortRange(port)
		if err != nil {
			return errors.Errorf("invalid range format for --expose: %s, error: %s", e, err)
		}
		for i := start; i <= end; i++ {
			p, err := nat.NewPort(proto, strconv.FormatUint(i, 10))
			if err != nil {
				return err
			}
			if _, exists := ports[p]; !exists {
				ports[p] = struct{}{}
			}
		}
	}
	for port, bindings := range portBindings {
		config.hostConfig.PortBindings[port] = bindings
	}
	for port, s := range ports {
		config.config.ExposedPorts[port] = s
	}

	mounts := copts.Mounts.Value()
	if len(mounts) > 0 && copts.VolumeDriver != "" {
		logrus.Warn("`--volume-driver` is ignored for volumes specified via `--mount`. Use `--mount type=volume,volume-driver=...` instead.")
	}
	var binds []string
	volumes := copts.Volumes.GetMap()
	// add any bind targets to the list of container volumes
	for bind := range copts.Volumes.GetMap() {
		parsed, _ := loader.ParseVolume(bind)

		if parsed.Source != "" {
			toBind := bind

			if parsed.Type == string(mounttypes.TypeBind) {
				if arr := strings.SplitN(bind, ":", 2); len(arr) == 2 {
					hostPart := arr[0]
					if strings.HasPrefix(hostPart, "."+string(filepath.Separator)) || hostPart == "." {
						if absHostPart, err := filepath.Abs(hostPart); err == nil {
							hostPart = absHostPart
						}
					}
					toBind = hostPart + ":" + arr[1]
				}
			}

			// after creating the bind mount we want to delete it from the copts.volumes values because
			// we do not want bind mounts being committed to image configs
			binds = append(binds, toBind)
			// We should delete from the map (`volumes`) here, as deleting from copts.volumes will not work if
			// there are duplicates entries.
			delete(volumes, bind)
		}
	}

	config.hostConfig.Binds = binds
	networkOpts, err := parseNetworkOpts(copts)
	if err != nil {
		return err
	}
	config.networkingConfig = &network.NetworkingConfig{EndpointsConfig: networkOpts}

	return nil
}

func convertToStandardNotation(ports []string) ([]string, error) {
	optsList := []string{}
	for _, publish := range ports {
		if strings.Contains(publish, "=") {
			params := map[string]string{"protocol": "tcp"}
			for _, param := range strings.Split(publish, ",") {
				opt := strings.Split(param, "=")
				if len(opt) < 2 {
					return optsList, errors.Errorf("invalid publish opts format (should be name=value but got '%s')", param)
				}

				params[opt[0]] = opt[1]
			}
			optsList = append(optsList, fmt.Sprintf("%s:%s/%s", params["published"], params["target"], params["protocol"]))
		} else {
			optsList = append(optsList, publish)
		}
	}
	return optsList, nil
}

// parseNetworkOpts converts --network advanced options to endpoint-specs, and combines
// them with the old --network-alias and --links. If returns an error if conflicting options
// are found.
//
// this function may return _multiple_ endpoints, which is not currently supported
// by the daemon, but may be in future; it's up to the daemon to produce an error
// in case that is not supported.
func parseNetworkOpts(copts Options) (map[string]*network.EndpointSettings, error) {
	var (
		endpoints                         = make(map[string]*network.EndpointSettings, len(copts.NetMode.Value()))
		hasUserDefined, hasNonUserDefined bool
	)

	for i, n := range copts.NetMode.Value() {
		n := n
		if container.NetworkMode(n.Target).IsUserDefined() {
			hasUserDefined = true
		} else {
			hasNonUserDefined = true
		}
		ep, err := parseNetworkAttachmentOpt(n)
		if err != nil {
			return nil, err
		}
		if _, ok := endpoints[n.Target]; ok {
			return nil, errdefs.InvalidParameter(errors.Errorf("network %q is specified multiple times", n.Target))
		}

		// For backward compatibility: if no custom options are provided for the network,
		// and only a single network is specified, omit the endpoint-configuration
		// on the client (the daemon will still create it when creating the container)
		if i == 0 && len(copts.NetMode.Value()) == 1 {
			if ep == nil || reflect.DeepEqual(*ep, network.EndpointSettings{}) {
				continue
			}
		}
		endpoints[n.Target] = ep
	}
	if hasUserDefined && hasNonUserDefined {
		return nil, errdefs.InvalidParameter(errors.New("conflicting options: cannot attach both user-defined and non-user-defined network-modes"))
	}
	return endpoints, nil
}

func parseNetworkAttachmentOpt(ep opts.NetworkAttachmentOpts) (*network.EndpointSettings, error) {
	if strings.TrimSpace(ep.Target) == "" {
		return nil, errors.New("no name set for network")
	}
	if !container.NetworkMode(ep.Target).IsUserDefined() {
		if len(ep.Aliases) > 0 {
			return nil, errors.New("network-scoped aliases are only supported for user-defined networks")
		}
		if len(ep.Links) > 0 {
			return nil, errors.New("links are only supported for user-defined networks")
		}
	}

	epConfig := &network.EndpointSettings{
		NetworkID: ep.Target,
	}
	epConfig.Aliases = append(epConfig.Aliases, ep.Aliases...)
	if len(ep.DriverOpts) > 0 {
		epConfig.DriverOpts = make(map[string]string)
		epConfig.DriverOpts = ep.DriverOpts
	}
	if len(ep.Links) > 0 {
		epConfig.Links = ep.Links
	}
	if ep.IPv4Address != "" || ep.IPv6Address != "" || len(ep.LinkLocalIPs) > 0 {
		epConfig.IPAMConfig = &network.EndpointIPAMConfig{
			IPv4Address:  ep.IPv4Address,
			IPv6Address:  ep.IPv6Address,
			LinkLocalIPs: ep.LinkLocalIPs,
		}
	}
	return epConfig, nil
}
