package handler

import "testing"

func TestGetVersionFromOutput(t *testing.T) {
	tests := []struct {
		output  string
		version string
	}{
		{
			output: `KubeVPN: CLI
    Version: v2.2.3
    Daemon: v2.2.3
    Image: docker.io/naison/kubevpn:v2.2.3
    Branch: feat/ssh-heartbeat
    Git commit: 1272e86a337d3075427ee3a1c3681d378558d133
    Built time: 2024-03-08 17:14:49
    Built OS/Arch: darwin/arm64
    Built Go version: go1.20.5`,
			version: "v2.2.3",
		},
		{
			output: `KubeVPN: CLI
    Version: v2.2.3
    Daemon: unknown
    Image: docker.io/naison/kubevpn:v2.2.3
    Branch: feat/ssh-heartbeat
    Git commit: 1272e86a337d3075427ee3a1c3681d378558d133
    Built time: 2024-03-08 17:14:49
    Built OS/Arch: darwin/arm64
    Built Go version: go1.20.5`,
			version: "unknown",
		},
		{
			output:  "hello",
			version: "",
		},
		{
			output:  "",
			version: "",
		},
	}
	for _, test := range tests {
		version := getDaemonVersionFromOutput([]byte(test.output))
		if version != test.version {
			t.Failed()
		}
	}
}
