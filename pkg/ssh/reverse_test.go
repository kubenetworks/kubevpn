package ssh

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestGenKubeconfigTempPath_WithRemoteKubeconfig(t *testing.T) {
	cases := []struct {
		name       string
		conf       *SshConfig
		kubeBytes  []byte
		wantPrefix string
		wantSuffix string
	}{
		{
			name: "alias with remote kubeconfig",
			conf: &SshConfig{
				ConfigAlias:      "prod-server",
				RemoteKubeconfig: "/root/.kube/config",
			},
			kubeBytes:  []byte("apiVersion: v1"),
			wantPrefix: "prod-server_config_",
		},
		{
			name: "addr with remote kubeconfig",
			conf: &SshConfig{
				Addr:             "10.0.0.1:22",
				RemoteKubeconfig: "/root/.kube/config",
			},
			kubeBytes:  []byte("apiVersion: v1"),
			wantPrefix: "10.0.0.1_22_config_",
		},
		{
			name: "addr non-standard port with remote kubeconfig",
			conf: &SshConfig{
				Addr:             "192.168.1.5:2222",
				RemoteKubeconfig: "/etc/kubernetes/admin.conf",
			},
			kubeBytes:  []byte("apiVersion: v1"),
			wantPrefix: "192.168.1.5_2222_admin.conf_",
		},
	}

	tempPath := config.GetTempPath()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := time.Now().Unix()
			got := GenKubeconfigTempPath(c.conf, c.kubeBytes)
			after := time.Now().Unix()

			// Must be under the temp directory
			if !strings.HasPrefix(got, tempPath) {
				t.Fatalf("path %q does not start with temp dir %q", got, tempPath)
			}

			base := filepath.Base(got)
			if !strings.HasPrefix(base, c.wantPrefix) {
				t.Fatalf("base %q does not start with prefix %q", base, c.wantPrefix)
			}

			// Extract the unix timestamp suffix
			suffix := strings.TrimPrefix(base, c.wantPrefix)
			if suffix == "" {
				t.Fatal("expected unix timestamp suffix, got empty")
			}
			// The suffix should be a number within [before, after]
			var ts int64
			n, err := parseUnixTimestamp(suffix)
			if err != nil {
				t.Fatalf("failed to parse timestamp suffix %q: %v", suffix, err)
			}
			ts = n
			if ts < before || ts > after {
				t.Fatalf("timestamp %d not in range [%d, %d]", ts, before, after)
			}
		})
	}
}

func TestGenKubeconfigTempPath_NilConf(t *testing.T) {
	// When conf is nil, falls through to pkgutil.GenKubeconfigTempPath
	got := GenKubeconfigTempPath(nil, []byte("apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: test-cluster\ncontexts:\n- context:\n    cluster: test-cluster\n    namespace: default\n  name: test-context\ncurrent-context: test-context"))
	tempPath := config.GetTempPath()
	if !strings.HasPrefix(got, tempPath) {
		t.Fatalf("path %q does not start with temp dir %q", got, tempPath)
	}
}

func TestGenKubeconfigTempPath_EmptyRemoteKubeconfig(t *testing.T) {
	// conf exists but RemoteKubeconfig is empty => delegates to pkgutil
	conf := &SshConfig{
		Addr: "10.0.0.1:22",
	}
	got := GenKubeconfigTempPath(conf, []byte("apiVersion: v1"))
	tempPath := config.GetTempPath()
	if !strings.HasPrefix(got, tempPath) {
		t.Fatalf("path %q does not start with temp dir %q", got, tempPath)
	}
}

func TestGenKubeconfigTempPath_Deterministic_Prefix(t *testing.T) {
	// Same config should produce same prefix (only timestamp differs)
	conf := &SshConfig{
		ConfigAlias:      "staging",
		RemoteKubeconfig: "/home/user/.kube/config",
	}
	path1 := GenKubeconfigTempPath(conf, nil)
	path2 := GenKubeconfigTempPath(conf, nil)

	base1 := filepath.Base(path1)
	base2 := filepath.Base(path2)

	// Both should start with "staging_config_"
	prefix := "staging_config_"
	if !strings.HasPrefix(base1, prefix) {
		t.Fatalf("path1 base %q does not have prefix %q", base1, prefix)
	}
	if !strings.HasPrefix(base2, prefix) {
		t.Fatalf("path2 base %q does not have prefix %q", base2, prefix)
	}
}

func parseUnixTimestamp(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, &parseError{s}
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

type parseError struct {
	s string
}

func (e *parseError) Error() string {
	return "not a numeric string: " + e.s
}
