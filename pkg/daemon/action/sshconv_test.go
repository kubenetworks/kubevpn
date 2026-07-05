package action

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func TestParseSshFromRPC_Nil(t *testing.T) {
	conf := parseSshFromRPC(nil)
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
	conf := parseSshFromRPC(jump)
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
