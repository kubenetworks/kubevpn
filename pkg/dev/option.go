package dev

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/cli/cli/compose/loader"
	"github.com/docker/cli/opts"
	mounttypes "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
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
