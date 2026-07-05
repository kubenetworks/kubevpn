package util

import (
	"errors"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestClassifyDockerError(t *testing.T) {
	base := errors.New("exit status 1")
	tests := []struct {
		name   string
		output string
		want   error
	}{
		{"daemon down", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", config.ErrDockerDaemonNotRunning},
		{"image pull unable", "Unable to find image 'foo:bar' locally\ndocker: Error response from daemon: manifest unknown.", config.ErrDockerImagePull},
		{"image pull denied", "Error response from daemon: pull access denied for foo", config.ErrDockerImagePull},
		{"run failed", "docker: Error response from daemon: Conflict. The container name is already in use.", config.ErrDockerRun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyDockerError(base, []byte(tt.output))
			if !errors.Is(got, tt.want) {
				t.Fatalf("classifyDockerError(%q) does not wrap %v (got %v)", tt.output, tt.want, got)
			}
		})
	}
	if classifyDockerError(nil, []byte("x")) != nil {
		t.Fatal("classifyDockerError(nil, ...) should be nil")
	}
}
