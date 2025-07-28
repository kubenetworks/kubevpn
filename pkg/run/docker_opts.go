package run

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
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
)

// ContainerOptions is a data object with all the options for creating a container
type ContainerOptions struct {
	attach     opts.ListOpts
	volumes    opts.ListOpts
	mounts     opts.MountOpt
	publish    opts.ListOpts
	expose     opts.ListOpts
	privileged bool
	publishAll bool
	stdin      bool
	tty        bool
	entrypoint string
	netMode    opts.NetworkOpt
	autoRemove bool

	Image string
	Args  []string
}

// AddFlags adds all command line flags that will be used by Parse to the FlagSet
func AddFlags(flags *pflag.FlagSet) *ContainerOptions {
	copts := &ContainerOptions{
		attach:  opts.NewListOpts(validateAttach),
		expose:  opts.NewListOpts(nil),
		publish: opts.NewListOpts(nil),
		volumes: opts.NewListOpts(nil),
	}

	// General purpose flags
	flags.VarP(&copts.attach, "attach", "a", "Attach to STDIN, STDOUT or STDERR")
	_ = flags.MarkHidden("attach")
	flags.StringVar(&copts.entrypoint, "entrypoint", "", "Overwrite the default ENTRYPOINT of the image")
	flags.BoolVarP(&copts.stdin, "interactive", "i", true, "Keep STDIN open even if not attached")
	_ = flags.MarkHidden("interactive")
	flags.BoolVarP(&copts.tty, "tty", "t", true, "Allocate a pseudo-TTY")
	_ = flags.MarkHidden("tty")
	flags.BoolVar(&copts.autoRemove, "rm", false, "Automatically remove the container when it exits")

	// Security
	flags.BoolVar(&copts.privileged, "privileged", true, "Give extended privileges to this container")

	flags.Var(&copts.expose, "expose", "Expose a port or a range of ports")
	flags.VarP(&copts.publish, "publish", "p", "Publish a container's port(s) to the host")
	flags.BoolVarP(&copts.publishAll, "publish-all", "P", false, "Publish all exposed ports to random ports")

	// Logging and storage
	flags.VarP(&copts.volumes, "volume", "v", "Bind mount a volume")
	return copts
}

// Parse parses the args for the specified command and generates a Config,
// a HostConfig and returns them with the specified command.
// If the specified args are not valid, it will return an error.
func Parse(flags *pflag.FlagSet, copts *ContainerOptions) (*Config, *HostConfig, error) {
	var (
		attachStdin  = copts.attach.Get("stdin")
		attachStdout = copts.attach.Get("stdout")
		attachStderr = copts.attach.Get("stderr")
	)

	if copts.stdin {
		attachStdin = true
	}
	// If -a is not set, attach to stdout and stderr
	if copts.attach.Len() == 0 {
		attachStdout = true
		attachStderr = true
	}

	var err error

	mounts := copts.mounts.Value()
	var binds []string
	volumes := copts.volumes.GetMap()
	// add any bind targets to the list of container volumes
	for bind := range copts.volumes.GetMap() {
		parsed, err := loader.ParseVolume(bind)
		if err != nil {
			return nil, nil, err
		}

		if parsed.Source != "" {
			toBind := bind

			if parsed.Type == string(mounttypes.TypeBind) {
				if hostPart, targetPath, ok := strings.Cut(bind, ":"); ok {
					if strings.HasPrefix(hostPart, "."+string(filepath.Separator)) || hostPart == "." {
						if absHostPart, err := filepath.Abs(hostPart); err == nil {
							hostPart = absHostPart
						}
					}
					toBind = hostPart + ":" + targetPath
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

	var (
		runCmd     strslice.StrSlice
		entrypoint strslice.StrSlice
	)

	if len(copts.Args) > 0 {
		runCmd = copts.Args
	}

	if copts.entrypoint != "" {
		entrypoint = strslice.StrSlice{copts.entrypoint}
	} else if flags.Changed("entrypoint") {
		// if `--entrypoint=` is parsed then Entrypoint is reset
		entrypoint = []string{""}
	}

	publishOpts := copts.publish.GetAll()
	var (
		ports         map[nat.Port]struct{}
		portBindings  map[nat.Port][]nat.PortBinding
		convertedOpts []string
	)

	convertedOpts, err = convertToStandardNotation(publishOpts)
	if err != nil {
		return nil, nil, err
	}

	ports, portBindings, err = nat.ParsePortSpecs(convertedOpts)
	if err != nil {
		return nil, nil, err
	}

	// Merge in exposed ports to the map of published ports
	for _, e := range copts.expose.GetAll() {
		if strings.Contains(e, ":") {
			return nil, nil, errors.Errorf("invalid port format for --expose: %s", e)
		}
		// support two formats for expose, original format <portnum>/[<proto>]
		// or <startport-endport>/[<proto>]
		proto, port := nat.SplitProtoPort(e)
		// Parse the start and end port and create a sequence of ports to expose
		// if expose a port, the start and end port are the same
		start, end, err := nat.ParsePortRange(port)
		if err != nil {
			return nil, nil, errors.Errorf("invalid range format for --expose: %s, error: %s", e, err)
		}
		for i := start; i <= end; i++ {
			p, err := nat.NewPort(proto, strconv.FormatUint(i, 10))
			if err != nil {
				return nil, nil, err
			}
			if _, exists := ports[p]; !exists {
				ports[p] = struct{}{}
			}
		}
	}

	config := &Config{
		ExposedPorts: ports,
		Tty:          copts.tty,
		OpenStdin:    copts.stdin,
		AttachStdin:  attachStdin,
		AttachStdout: attachStdout,
		AttachStderr: attachStderr,
		Cmd:          runCmd,
		Volumes:      volumes,
		Entrypoint:   entrypoint,
	}

	hostConfig := &HostConfig{
		Binds:           binds,
		AutoRemove:      copts.autoRemove,
		Privileged:      copts.privileged,
		PortBindings:    portBindings,
		PublishAllPorts: copts.publishAll,
		Mounts:          mounts,
	}

	return config, hostConfig, nil
}

func convertToStandardNotation(ports []string) ([]string, error) {
	optsList := []string{}
	for _, publish := range ports {
		if strings.Contains(publish, "=") {
			params := map[string]string{"protocol": "tcp"}
			for _, param := range strings.Split(publish, ",") {
				k, v, ok := strings.Cut(param, "=")
				if !ok || k == "" {
					return optsList, errors.Errorf("invalid publish opts format (should be name=value but got '%s')", param)
				}
				params[k] = v
			}
			optsList = append(optsList, fmt.Sprintf("%s:%s/%s", params["published"], params["target"], params["protocol"]))
		} else {
			optsList = append(optsList, publish)
		}
	}
	return optsList, nil
}

// validateAttach validates that the specified string is a valid attach option.
func validateAttach(val string) (string, error) {
	s := strings.ToLower(val)
	for _, str := range []string{"stdin", "stdout", "stderr"} {
		if s == str {
			return s, nil
		}
	}
	return val, errors.Errorf("valid streams are STDIN, STDOUT and STDERR")
}

type Config struct {
	AttachStdin  bool                // Attach the standard input, makes possible user interaction
	AttachStdout bool                // Attach the standard output
	AttachStderr bool                // Attach the standard error
	ExposedPorts nat.PortSet         `json:",omitempty"` // List of exposed ports
	Tty          bool                // Attach standard streams to a tty, including stdin if it is not closed.
	OpenStdin    bool                // Open stdin
	StdinOnce    bool                // If true, close stdin after the 1 attached client disconnects.
	Cmd          strslice.StrSlice   // Command to run when starting the container
	Volumes      map[string]struct{} // List of volumes (mounts) used for the container
	Entrypoint   strslice.StrSlice   // Entrypoint to run when starting the container
}

type HostConfig struct {
	Binds        []string    // List of volume bindings for this container
	PortBindings nat.PortMap // Port mapping between the exposed port (container) and the host
	AutoRemove   bool        // Automatically remove container when it exits

	Privileged      bool               // Is the container in privileged mode
	PublishAllPorts bool               // Should docker publish all exposed port for the container
	Mounts          []mounttypes.Mount `json:",omitempty"`
}

type RunOptions struct {
	Platform string
	Pull     string // always, missing, never
}
