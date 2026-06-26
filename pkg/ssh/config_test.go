package ssh

import (
	"net"
	"testing"

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
