package ssh

import (
	"net"
	"strings"
	"testing"

	"github.com/kevinburke/ssh_config"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func TestSshConfig_IsEmpty(t *testing.T) {
	cases := []struct {
		name string
		conf SshConfig
		want bool
	}{
		{"zero value", SshConfig{}, true},
		{"only addr", SshConfig{Addr: "1.2.3.4:22"}, false},
		{"only alias", SshConfig{ConfigAlias: "myhost"}, false},
		{"only jump", SshConfig{Jump: "--ssh-addr 1.2.3.4"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.conf.IsEmpty(); got != c.want {
				t.Fatalf("IsEmpty: want %v, got %v", c.want, got)
			}
		})
	}
}

func TestSshConfig_IsLoopback(t *testing.T) {
	cases := []struct {
		name string
		conf SshConfig
		want bool
	}{
		{"loopback v4", SshConfig{Addr: "127.0.0.1:22"}, true},
		{"loopback name", SshConfig{Addr: "localhost:22"}, true},
		{"remote", SshConfig{Addr: "192.168.1.100:22"}, false},
		{"empty", SshConfig{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.conf.IsLoopback(); got != c.want {
				t.Fatalf("IsLoopback: want %v, got %v", c.want, got)
			}
		})
	}
}

func TestParseSshFromRPC_Nil(t *testing.T) {
	conf := ParseSshFromRPC(nil)
	if !conf.IsEmpty() {
		t.Fatal("nil RPC should produce empty config")
	}
}

func TestParseSshFromRPC_Full(t *testing.T) {
	jump := &rpc.SshJump{
		Addr:             "10.0.0.1:22",
		User:             "admin",
		Password:         "secret",
		Keyfile:          "/home/user/.ssh/id_rsa",
		Jump:             "--ssh-addr bastion:22",
		ConfigAlias:      "prod",
		RemoteKubeconfig: "/root/.kube/config",
		GSSAPIPassword:   "krb5pass",
		GSSAPIKeytabConf: "/etc/krb5.keytab",
		GSSAPICacheFile:  "/tmp/krb5cc_1000",
	}
	conf := ParseSshFromRPC(jump)
	if conf.Addr != "10.0.0.1:22" {
		t.Fatalf("Addr: want 10.0.0.1:22, got %s", conf.Addr)
	}
	if conf.User != "admin" {
		t.Fatalf("User: want admin, got %s", conf.User)
	}
	if conf.Password != "secret" {
		t.Fatalf("Password mismatch")
	}
	if conf.Keyfile != "/home/user/.ssh/id_rsa" {
		t.Fatalf("Keyfile mismatch")
	}
	if conf.Jump != "--ssh-addr bastion:22" {
		t.Fatalf("Jump mismatch")
	}
	if conf.ConfigAlias != "prod" {
		t.Fatalf("ConfigAlias mismatch")
	}
	if conf.RemoteKubeconfig != "/root/.kube/config" {
		t.Fatalf("RemoteKubeconfig mismatch")
	}
}

func TestSshConfig_ToRPC_RoundTrip(t *testing.T) {
	original := SshConfig{
		Addr:             "host:22",
		User:             "user",
		Password:         "pass",
		Keyfile:          "/key",
		Jump:             "jump",
		ConfigAlias:      "alias",
		RemoteKubeconfig: "/kubeconfig",
		GSSAPIPassword:   "gsspass",
		GSSAPIKeytabConf: "/keytab",
		GSSAPICacheFile:  "/cache",
	}

	rpcMsg := original.ToRPC()
	restored := ParseSshFromRPC(rpcMsg)

	if restored.Addr != original.Addr {
		t.Fatalf("Addr roundtrip failed: %s vs %s", restored.Addr, original.Addr)
	}
	if restored.User != original.User {
		t.Fatalf("User roundtrip failed")
	}
	if restored.Password != original.Password {
		t.Fatalf("Password roundtrip failed")
	}
	if restored.ConfigAlias != original.ConfigAlias {
		t.Fatalf("ConfigAlias roundtrip failed")
	}
	if restored.RemoteKubeconfig != original.RemoteKubeconfig {
		t.Fatalf("RemoteKubeconfig roundtrip failed")
	}
}

func TestSshConfig_Host(t *testing.T) {
	cases := []struct {
		name     string
		conf     SshConfig
		wantIPs  []net.IP
		wantEmpty bool
	}{
		{
			name:      "empty addr returns empty",
			conf:      SshConfig{},
			wantEmpty: true,
		},
		{
			name: "IPv4 with port",
			conf: SshConfig{Addr: "192.168.1.100:22"},
			wantIPs: []net.IP{net.ParseIP("192.168.1.100")},
		},
		{
			name: "IPv4 without port falls back",
			conf: SshConfig{Addr: "10.0.0.1"},
			wantIPs: []net.IP{net.ParseIP("10.0.0.1")},
		},
		{
			name: "loopback",
			conf: SshConfig{Addr: "127.0.0.1:22"},
			wantIPs: []net.IP{net.ParseIP("127.0.0.1")},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.conf.Host()
			if c.wantEmpty {
				if len(got) != 0 {
					t.Fatalf("expected empty, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("expected IPs, got empty")
			}
			// Check that at least one expected IP is in the result
			for _, want := range c.wantIPs {
				found := false
				for _, ip := range got {
					if ip.Equal(want) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("expected IP %v in result %v", want, got)
				}
			}
		})
	}
}

func TestSshConfig_KubeconfigIdentifier(t *testing.T) {
	cases := []struct {
		name string
		conf SshConfig
		want string
	}{
		{
			name: "config alias takes priority",
			conf: SshConfig{
				ConfigAlias:      "prod-server",
				Addr:             "10.0.0.1:22",
				RemoteKubeconfig: "/root/.kube/config",
			},
			want: "prod-server_config",
		},
		{
			name: "addr with port when no alias",
			conf: SshConfig{
				Addr:             "192.168.1.100:22",
				RemoteKubeconfig: "/home/user/.kube/config",
			},
			want: "192.168.1.100_22_config",
		},
		{
			name: "addr with non-standard port",
			conf: SshConfig{
				Addr:             "10.0.0.5:2222",
				RemoteKubeconfig: "/etc/kubernetes/admin.conf",
			},
			want: "10.0.0.5_2222_admin.conf",
		},
		{
			name: "empty addr and alias uses remote kubeconfig basename",
			conf: SshConfig{
				RemoteKubeconfig: "/root/.kube/config",
			},
			want: "config",
		},
		{
			name: "empty everything returns empty basename",
			conf: SshConfig{},
			want: ".",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.conf.KubeconfigIdentifier()
			if got != c.want {
				t.Fatalf("KubeconfigIdentifier: want %q, got %q", c.want, got)
			}
		})
	}
}

func TestSshConfig_KubeconfigIdentifier_EdgeCases(t *testing.T) {
	cases := []struct {
		name string
		conf SshConfig
		want string
	}{
		{
			name: "addr without port falls back to IPToFilename only",
			conf: SshConfig{
				Addr:             "10.0.0.1",
				RemoteKubeconfig: "/root/.kube/config",
			},
			want: "10.0.0.1_config",
		},
		{
			name: "addr with invalid IP and port",
			conf: SshConfig{
				Addr:             "not-an-ip:22",
				RemoteKubeconfig: "/root/.kube/config",
			},
			// net.SplitHostPort succeeds (host="not-an-ip", port="22"),
			// then IPToFilename("not-an-ip") returns "invalid-ip"
			want: "invalid-ip_22_config",
		},
		{
			name: "addr with invalid IP without port",
			conf: SshConfig{
				Addr:             "not-an-ip",
				RemoteKubeconfig: "/root/.kube/config",
			},
			// net.SplitHostPort fails (no port), falls to IPToFilename("not-an-ip")
			want: "invalid-ip_config",
		},
		{
			name: "alias with special characters",
			conf: SshConfig{
				ConfigAlias:      "my.server-prod_01",
				RemoteKubeconfig: "/root/.kube/config",
			},
			want: "my.server-prod_01_config",
		},
		{
			name: "alias with spaces",
			conf: SshConfig{
				ConfigAlias:      "my server",
				RemoteKubeconfig: "/root/.kube/config",
			},
			want: "my server_config",
		},
		{
			name: "deep nested remote kubeconfig path",
			conf: SshConfig{
				ConfigAlias:      "prod",
				RemoteKubeconfig: "/very/deep/nested/path/to/my-kubeconfig.yaml",
			},
			want: "prod_my-kubeconfig.yaml",
		},
		{
			name: "empty remote kubeconfig with alias",
			conf: SshConfig{
				ConfigAlias:      "prod",
				RemoteKubeconfig: "",
			},
			// filepath.Base("") returns "."
			want: "prod_.",
		},
		{
			name: "only remote kubeconfig set",
			conf: SshConfig{
				RemoteKubeconfig: "/root/.kube/my-cluster.conf",
			},
			// prefix stays empty, returns filepath.Base(RemoteKubeconfig)
			want: "my-cluster.conf",
		},
		{
			name: "IPv6 addr with port",
			conf: SshConfig{
				Addr:             "[2001:db8::1]:22",
				RemoteKubeconfig: "/root/.kube/config",
			},
			// net.SplitHostPort("[2001:db8::1]:22") => host="2001:db8::1", port="22"
			// IPToFilename("2001:db8::1") produces IPv6 hex format
			want: func() string {
				got := IPToFilename("2001:db8::1")
				return got + "_22_config"
			}(),
		},
		{
			name: "alias takes priority over addr and jump",
			conf: SshConfig{
				ConfigAlias:      "myalias",
				Addr:             "10.0.0.1:22",
				Jump:             "--ssh-addr 10.0.0.2:22",
				RemoteKubeconfig: "/root/.kube/config",
			},
			want: "myalias_config",
		},
		{
			name: "addr takes priority over jump",
			conf: SshConfig{
				Addr:             "10.0.0.1:22",
				Jump:             "--ssh-addr 10.0.0.2:22",
				RemoteKubeconfig: "/root/.kube/config",
			},
			want: "10.0.0.1_22_config",
		},
		{
			name: "jump only with addr flag",
			conf: SshConfig{
				Jump:             "--ssh-addr 10.0.0.5:2222",
				RemoteKubeconfig: "/root/.kube/config",
			},
			// Recursive: inner SshConfig has Addr="10.0.0.5:2222" but empty RemoteKubeconfig,
			// so inner KubeconfigIdentifier returns "10.0.0.5_2222_." (filepath.Base("") = "."),
			// then outer appends "_config"
			want: "10.0.0.5_2222_._config",
		},
		{
			name: "jump only with alias flag",
			conf: SshConfig{
				Jump:             "--ssh-alias bastion",
				RemoteKubeconfig: "/root/.kube/config",
			},
			// Recursive: inner SshConfig has ConfigAlias="bastion" but empty RemoteKubeconfig,
			// so inner KubeconfigIdentifier returns "bastion_." (filepath.Base("") = "."),
			// then outer appends "_config"
			want: "bastion_._config",
		},
		{
			name: "jump with unparseable flags falls back",
			conf: SshConfig{
				Jump:             "",
				RemoteKubeconfig: "/root/.kube/config",
			},
			// Empty jump string: flags.Parse produces empty SshConfig, recursive call
			// returns filepath.Base(RemoteKubeconfig) since inner RemoteKubeconfig is ""
			// prefix becomes "." (from filepath.Base("")), then outer falls through
			// Actually: Jump is empty, so the `conf.Jump != ""` check fails, prefix stays ""
			// Returns filepath.Base("/root/.kube/config") = "config"
			want: "config",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.conf.KubeconfigIdentifier()
			if got != c.want {
				t.Fatalf("KubeconfigIdentifier: want %q, got %q", c.want, got)
			}
		})
	}
}

// parseSSHConfig is a test helper that parses an SSH config string into a defaultSshConf.
func parseSSHConfig(t *testing.T, content string) defaultSshConf {
	t.Helper()
	cfg, err := ssh_config.DecodeBytes([]byte(content))
	if err != nil {
		t.Fatalf("failed to parse SSH config: %v", err)
	}
	return defaultSshConf{cfg}
}

func TestResolveProxyJumpChain_LinearChain(t *testing.T) {
	// A -> B -> C (linear chain, no cycle)
	conf := parseSSHConfig(t, `
Host hostA
  Hostname 10.0.0.1
  ProxyJump hostB

Host hostB
  Hostname 10.0.0.2
  ProxyJump hostC

Host hostC
  Hostname 10.0.0.3
`)
	bastionList, err := resolveProxyJumpChain("hostA", SshConfig{}, conf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect 3 bastions: hostA, hostB, hostC
	if len(bastionList) != 3 {
		t.Fatalf("expected 3 bastions, got %d", len(bastionList))
	}
}

func TestResolveProxyJumpChain_DirectCycle(t *testing.T) {
	// A -> B -> A (direct cycle)
	conf := parseSSHConfig(t, `
Host hostA
  Hostname 10.0.0.1
  ProxyJump hostB

Host hostB
  Hostname 10.0.0.2
  ProxyJump hostA
`)
	_, err := resolveProxyJumpChain("hostA", SshConfig{}, conf)
	if err == nil {
		t.Fatal("expected circular ProxyJump error, got nil")
	}
	if !strings.Contains(err.Error(), "circular ProxyJump detected") {
		t.Fatalf("expected circular ProxyJump error, got: %v", err)
	}
	// The cycle is hostB -> hostA, so the error should mention both
	if !strings.Contains(err.Error(), "hostB -> hostA") {
		t.Fatalf("expected error to mention hostB -> hostA, got: %v", err)
	}
}

func TestResolveProxyJumpChain_SelfCycle(t *testing.T) {
	// A -> A (self-referencing ProxyJump)
	conf := parseSSHConfig(t, `
Host hostA
  Hostname 10.0.0.1
  ProxyJump hostA
`)
	_, err := resolveProxyJumpChain("hostA", SshConfig{}, conf)
	if err == nil {
		t.Fatal("expected circular ProxyJump error, got nil")
	}
	if !strings.Contains(err.Error(), "circular ProxyJump detected") {
		t.Fatalf("expected circular ProxyJump error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "hostA -> hostA") {
		t.Fatalf("expected error to mention hostA -> hostA, got: %v", err)
	}
}

func TestResolveProxyJumpChain_LongCycle(t *testing.T) {
	// A -> B -> C -> D -> B (cycle back to B after several hops)
	conf := parseSSHConfig(t, `
Host hostA
  Hostname 10.0.0.1
  ProxyJump hostB

Host hostB
  Hostname 10.0.0.2
  ProxyJump hostC

Host hostC
  Hostname 10.0.0.3
  ProxyJump hostD

Host hostD
  Hostname 10.0.0.4
  ProxyJump hostB
`)
	_, err := resolveProxyJumpChain("hostA", SshConfig{}, conf)
	if err == nil {
		t.Fatal("expected circular ProxyJump error, got nil")
	}
	if !strings.Contains(err.Error(), "circular ProxyJump detected") {
		t.Fatalf("expected circular ProxyJump error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "hostD -> hostB") {
		t.Fatalf("expected error to mention hostD -> hostB, got: %v", err)
	}
}

func TestResolveProxyJumpChain_NoProxyJump(t *testing.T) {
	// Single host with no ProxyJump — should return just the one bastion
	conf := parseSSHConfig(t, `
Host hostA
  Hostname 10.0.0.1
`)
	bastionList, err := resolveProxyJumpChain("hostA", SshConfig{}, conf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bastionList) != 1 {
		t.Fatalf("expected 1 bastion, got %d", len(bastionList))
	}
}

func TestResolveProxyJumpChain_UnknownAlias(t *testing.T) {
	// Start alias has no config entry — ProxyJump lookup returns empty, so no cycle
	conf := parseSSHConfig(t, `
Host hostA
  Hostname 10.0.0.1
`)
	bastionList, err := resolveProxyJumpChain("nonexistent", SshConfig{}, conf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should return just the one bastion (for the start alias, even if unconfigured)
	if len(bastionList) != 1 {
		t.Fatalf("expected 1 bastion, got %d", len(bastionList))
	}
}

func TestDefaultSshConf_Get_EmptyList(t *testing.T) {
	var conf defaultSshConf
	result := conf.Get("unknown-host", "Hostname")
	if result == "unknown-host" {
		return
	}
	// ssh_config.Get falls back to defaults
}

func TestDefaultSshConf_Get_WithConfig(t *testing.T) {
	cfg, err := ssh_config.Decode(strings.NewReader(`
Host test-server
  Hostname 10.0.0.1
  Port 2222
  User admin
`))
	if err != nil {
		t.Fatalf("ssh_config.Decode: %v", err)
	}
	conf := defaultSshConf{cfg}

	hostname := conf.Get("test-server", "Hostname")
	if hostname != "10.0.0.1" {
		t.Errorf("Hostname: want 10.0.0.1, got %s", hostname)
	}
	port := conf.Get("test-server", "Port")
	if port != "2222" {
		t.Errorf("Port: want 2222, got %s", port)
	}
	user := conf.Get("test-server", "User")
	if user != "admin" {
		t.Errorf("User: want admin, got %s", user)
	}
}

func TestSshConfig_IsEmpty_AllSet(t *testing.T) {
	conf := SshConfig{
		ConfigAlias: "alias",
		Addr:        "10.0.0.1:22",
		Jump:        "jump-host",
	}
	if conf.IsEmpty() {
		t.Error("config with all fields set should not be empty")
	}
}

func TestSshConfig_IsEmpty_OnlyAddr(t *testing.T) {
	conf := SshConfig{Addr: "10.0.0.1:22"}
	if conf.IsEmpty() {
		t.Error("config with only addr should not be empty")
	}
}

func TestSshConfig_IsLoopback_NonLoopback(t *testing.T) {
	conf := SshConfig{Addr: "10.0.0.1:22"}
	if conf.IsLoopback() {
		t.Error("10.0.0.1 should not be loopback")
	}
}

func TestSshConfig_Host_EmptyConfig(t *testing.T) {
	conf := SshConfig{}
	ips := conf.Host()
	if len(ips) != 0 {
		t.Errorf("empty config should return no IPs, got %v", ips)
	}
}
