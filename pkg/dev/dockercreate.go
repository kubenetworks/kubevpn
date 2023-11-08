package dev

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/image"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/versions"
	apiclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/registry"
	specs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

// Pull constants
const (
	PullImageAlways  = "always"
	PullImageMissing = "missing" // Default (matches previous behavior)
	PullImageNever   = "never"
)

type createOptions struct {
	Name      string
	Platform  string
	Untrusted bool
	Pull      string // always, missing, never
	Quiet     bool
}

func pullImage(ctx context.Context, dockerCli command.Cli, image string, platform string, out io.Writer) error {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		err = errors.Wrap(err, "reference.ParseNormalizedNamed(image): ")
		return err
	}

	// Resolve the Repository name from fqn to RepositoryInfo
	repoInfo, err := registry.ParseRepositoryInfo(ref)
	if err != nil {
		err = errors.Wrap(err, "registry.ParseRepositoryInfo(ref): ")
		return err
	}

	authConfig := command.ResolveAuthConfig(ctx, dockerCli, repoInfo.Index)
	encodedAuth, err := command.EncodeAuthToBase64(authConfig)
	if err != nil {
		err = errors.Wrap(err, "command.EncodeAuthToBase64(authConfig): ")
		return err
	}

	options := types.ImageCreateOptions{
		RegistryAuth: encodedAuth,
		Platform:     platform,
	}

	responseBody, err := dockerCli.Client().ImageCreate(ctx, image, options)
	if err != nil {
		err = errors.Wrap(err, "dockerCli.Client().ImageCreate(ctx, image, options): ")
		return err
	}
	defer responseBody.Close()

	return jsonmessage.DisplayJSONMessagesStream(
		responseBody,
		out,
		dockerCli.Out().FD(),
		dockerCli.Out().IsTerminal(),
		nil)
}

type cidFile struct {
	path    string
	file    *os.File
	written bool
}

func (cid *cidFile) Close() error {
	if cid.file == nil {
		return nil
	}
	cid.file.Close()

	if cid.written {
		return nil
	}
	if err := os.Remove(cid.path); err != nil {
		return errors.Wrapf(err, "failed to remove the CID file '%s'", cid.path)
	}

	return nil
}

func (cid *cidFile) Write(id string) error {
	if cid.file == nil {
		return nil
	}
	if _, err := cid.file.Write([]byte(id)); err != nil {
		return errors.Wrap(err, "failed to write the container ID to the file")
	}
	cid.written = true
	return nil
}

func newCIDFile(path string) (*cidFile, error) {
	if path == "" {
		return &cidFile{}, nil
	}
	if _, err := os.Stat(path); err == nil {
		return nil, errors.Errorf("container ID file found, make sure the other container isn't running or delete %s", path)
	}

	f, err := os.Create(path)
	if err != nil {
		err = errors.Wrap(err, "os.Create(path): ")
		return nil, errors.Wrap(err, "failed to create the container ID file")
	}

	return &cidFile{path: path, file: f}, nil
}

//nolint:gocyclo
func createContainer(ctx context.Context, dockerCli command.Cli, containerConfig *containerConfig, opts *createOptions) (*container.CreateResponse, error) {
	config := containerConfig.Config
	hostConfig := containerConfig.HostConfig
	networkingConfig := containerConfig.NetworkingConfig
	stderr := dockerCli.Err()

	warnOnOomKillDisable(*hostConfig, stderr)
	warnOnLocalhostDNS(*hostConfig, stderr)

	var (
		trustedRef reference.Canonical
		namedRef   reference.Named
	)

	containerIDFile, err := newCIDFile(hostConfig.ContainerIDFile)
	if err != nil {
		err = errors.Wrap(err, "newCIDFile(hostConfig.ContainerIDFile): ")
		return nil, err
	}
	defer containerIDFile.Close()

	ref, err := reference.ParseAnyReference(config.Image)
	if err != nil {
		err = errors.Wrap(err, "reference.ParseAnyReference(config.Image): ")
		return nil, err
	}
	if named, ok := ref.(reference.Named); ok {
		namedRef = reference.TagNameOnly(named)

		if taggedRef, ok := namedRef.(reference.NamedTagged); ok && !opts.Untrusted {
			var err error
			trustedRef, err = image.TrustedReference(ctx, dockerCli, taggedRef, nil)
			if err != nil {
				err = errors.Wrap(err, "image.TrustedReference(ctx, dockerCli, taggedRef, nil): ")
				return nil, err
			}
			config.Image = reference.FamiliarString(trustedRef)
		}
	}

	pullAndTagImage := func() error {
		pullOut := stderr
		if opts.Quiet {
			pullOut = io.Discard
		}
		if err := pullImage(ctx, dockerCli, config.Image, opts.Platform, pullOut); err != nil {
			return err
		}
		if taggedRef, ok := namedRef.(reference.NamedTagged); ok && trustedRef != nil {
			return image.TagTrusted(ctx, dockerCli, trustedRef, taggedRef)
		}
		return nil
	}

	var platform *specs.Platform
	// Engine API version 1.41 first introduced the option to specify platform on
	// create. It will produce an error if you try to set a platform on older API
	// versions, so check the API version here to maintain backwards
	// compatibility for CLI users.
	if opts.Platform != "" && versions.GreaterThanOrEqualTo(dockerCli.Client().ClientVersion(), "1.41") {
		p, err := platforms.Parse(opts.Platform)
		if err != nil {
			err = errors.Wrap(err, "platforms.Parse(opts.Platform): ")
			return nil, errors.Wrap(err, "error parsing specified platform")
		}
		platform = &p
	}

	if opts.Pull == PullImageAlways {
		if err := pullAndTagImage(); err != nil {
			return nil, err
		}
	}

	hostConfig.ConsoleSize[0], hostConfig.ConsoleSize[1] = dockerCli.Out().GetTtySize()

	response, err := dockerCli.Client().ContainerCreate(ctx, config, hostConfig, networkingConfig, platform, opts.Name)
	if err != nil {
		// Pull image if it does not exist locally and we have the PullImageMissing option. Default behavior.
		if apiclient.IsErrNotFound(err) && namedRef != nil && opts.Pull == PullImageMissing {
			if !opts.Quiet {
				// we don't want to write to stdout anything apart from container.ID
				fmt.Fprintf(stderr, "Unable to find image '%s' locally\n", reference.FamiliarString(namedRef))
			}

			if err := pullAndTagImage(); err != nil {
				return nil, err
			}

			var retryErr error
			response, retryErr = dockerCli.Client().ContainerCreate(ctx, config, hostConfig, networkingConfig, platform, opts.Name)
			if retryErr != nil {
				return nil, retryErr
			}
		} else {
			return nil, err
		}
	}

	for _, warning := range response.Warnings {
		fmt.Fprintf(stderr, "WARNING: %s\n", warning)
	}
	err = containerIDFile.Write(response.ID)
	return &response, err
}

func warnOnOomKillDisable(hostConfig container.HostConfig, stderr io.Writer) {
	if hostConfig.OomKillDisable != nil && *hostConfig.OomKillDisable && hostConfig.Memory == 0 {
		fmt.Fprintln(stderr, "WARNING: Disabling the OOM killer on containers without setting a '-m/--memory' limit may be dangerous.")
	}
}

// check the DNS settings passed via --dns against localhost regexp to warn if
// they are trying to set a DNS to a localhost address
func warnOnLocalhostDNS(hostConfig container.HostConfig, stderr io.Writer) {
	for _, dnsIP := range hostConfig.DNS {
		if isLocalhost(dnsIP) {
			fmt.Fprintf(stderr, "WARNING: Localhost DNS setting (--dns=%s) may fail in containers.\n", dnsIP)
			return
		}
	}
}

// IPLocalhost is a regex pattern for IPv4 or IPv6 loopback range.
const ipLocalhost = `((127\.([0-9]{1,3}\.){2}[0-9]{1,3})|(::1)$)`

var localhostIPRegexp = regexp.MustCompile(ipLocalhost)

// IsLocalhost returns true if ip matches the localhost IP regular expression.
// Used for determining if nameserver settings are being passed which are
// localhost addresses
func isLocalhost(ip string) bool {
	return localhostIPRegexp.MatchString(ip)
}

func validatePullOpt(val string) error {
	switch val {
	case PullImageAlways, PullImageMissing, PullImageNever, "":
		// valid option, but nothing to do yet
		return nil
	default:
		return errors.Errorf(
			"invalid pull option: '%s': must be one of %q, %q or %q",
			val,
			PullImageAlways,
			PullImageMissing,
			PullImageNever,
		)
	}
}
