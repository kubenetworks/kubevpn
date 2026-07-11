package handler

import (
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

// SshConfigToRPC converts an SshConfig into an RPC SshJump message for gRPC transport.
// This lives in the handler layer to avoid pkg/ssh depending on pkg/daemon/rpc.
func SshConfigToRPC(conf *ssh.SshConfig) *rpc.SshJump {
	if conf == nil {
		return nil
	}
	return &rpc.SshJump{
		Addr:             conf.Addr,
		User:             conf.User,
		Password:         conf.Password,
		Keyfile:          conf.Keyfile,
		Jump:             conf.Jump,
		ConfigAlias:      conf.ConfigAlias,
		RemoteKubeconfig: conf.RemoteKubeconfig,
		GSSAPIKeytabConf: conf.GSSAPIKeytabConf,
		GSSAPIPassword:   conf.GSSAPIPassword,
		GSSAPICacheFile:  conf.GSSAPICacheFile,
	}
}
