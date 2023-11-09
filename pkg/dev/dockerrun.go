package dev

import (
	"context"
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

type RunOptions struct {
	createOptions
	Detach     bool
	sigProxy   bool
	detachKeys string
}

func attachContainer(ctx context.Context, dockerCli command.Cli, errCh *chan error, config *container.Config, containerID string) (func(), error) {
	options := types.ContainerAttachOptions{
		Stream:     true,
		Stdin:      config.AttachStdin,
		Stdout:     config.AttachStdout,
		Stderr:     config.AttachStderr,
		DetachKeys: dockerCli.ConfigFile().DetachKeys,
	}

	resp, errAttach := dockerCli.Client().ContainerAttach(ctx, containerID, options)
	if errAttach != nil {
		return nil, errors.Errorf("failed to attach to container: %s, err: %v", containerID, errAttach)
	}

	var (
		out, cerr io.Writer
		in        io.ReadCloser
	)
	if config.AttachStdin {
		in = dockerCli.In()
	}
	if config.AttachStdout {
		out = dockerCli.Out()
	}
	if config.AttachStderr {
		if config.Tty {
			cerr = dockerCli.Out()
		} else {
			cerr = dockerCli.Err()
		}
	}

	ch := make(chan error, 1)
	*errCh = ch

	if in != nil && out != nil && cerr != nil {

	}

	go func() {
		ch <- func() error {
			streamer := hijackedIOStreamer{
				streams:      dockerCli,
				inputStream:  in,
				outputStream: out,
				errorStream:  cerr,
				resp:         resp,
				tty:          config.Tty,
				detachKeys:   options.DetachKeys,
			}

			if errHijack := streamer.stream(ctx); errHijack != nil {
				return errHijack
			}
			return errAttach
		}()
	}()
	return resp.Close, nil
}

// reportError is a utility method that prints a user-friendly message
// containing the error that occurred during parsing and a suggestion to get help
func reportError(stderr io.Writer, name string, str string, withHelp bool) {
	str = strings.TrimSuffix(str, ".") + "."
	if withHelp {
		str += "\nSee 'docker " + name + " --help'."
	}
	log.Error(str)
	_, _ = fmt.Fprintln(stderr, "docker:", str)
}

// if container start fails with 'not found'/'no such' error, return 127
// if container start fails with 'permission denied' error, return 126
// return 125 for generic docker daemon failures
func runStartContainerErr(err error) error {
	trimmedErr := strings.TrimPrefix(err.Error(), "Error response from daemon: ")
	statusError := cli.StatusError{StatusCode: 125}
	if strings.Contains(trimmedErr, "executable file not found") ||
		strings.Contains(trimmedErr, "no such file or directory") ||
		strings.Contains(trimmedErr, "system cannot find the file specified") {
		statusError = cli.StatusError{StatusCode: 127}
	} else if strings.Contains(trimmedErr, syscall.EACCES.Error()) {
		statusError = cli.StatusError{StatusCode: 126}
	}

	return statusError
}
