package handler

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

func TestSshConfigToRPC_Nil(t *testing.T) {
	result := SshConfigToRPC(nil)
	if result != nil {
		t.Fatal("nil config should produce nil RPC message")
	}
}

func TestSshConfigToRPC_RoundTrip(t *testing.T) {
	original := &ssh.SshConfig{
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

	rpcMsg := SshConfigToRPC(original)
	if rpcMsg.Addr != original.Addr {
		t.Fatalf("Addr: want %s, got %s", original.Addr, rpcMsg.Addr)
	}
	if rpcMsg.User != original.User {
		t.Fatalf("User: want %s, got %s", original.User, rpcMsg.User)
	}
	if rpcMsg.Password != original.Password {
		t.Fatalf("Password mismatch")
	}
	if rpcMsg.Keyfile != original.Keyfile {
		t.Fatalf("Keyfile mismatch")
	}
	if rpcMsg.Jump != original.Jump {
		t.Fatalf("Jump mismatch")
	}
	if rpcMsg.ConfigAlias != original.ConfigAlias {
		t.Fatalf("ConfigAlias mismatch")
	}
	if rpcMsg.RemoteKubeconfig != original.RemoteKubeconfig {
		t.Fatalf("RemoteKubeconfig mismatch")
	}
	if rpcMsg.GSSAPIPassword != original.GSSAPIPassword {
		t.Fatalf("GSSAPIPassword mismatch")
	}
	if rpcMsg.GSSAPIKeytabConf != original.GSSAPIKeytabConf {
		t.Fatalf("GSSAPIKeytabConf mismatch")
	}
	if rpcMsg.GSSAPICacheFile != original.GSSAPICacheFile {
		t.Fatalf("GSSAPICacheFile mismatch")
	}
}
