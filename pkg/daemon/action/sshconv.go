package action

import (
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

// parseSshFromRPC converts an RPC SshJump message into an SshConfig struct.
// This lives in the daemon/action layer to avoid pkg/ssh depending on pkg/daemon/rpc.
func parseSshFromRPC(sshJump *rpc.SshJump) *ssh.SshConfig {
	if sshJump == nil {
		return &ssh.SshConfig{}
	}
	return &ssh.SshConfig{
		Addr:             sshJump.Addr,
		User:             sshJump.User,
		Password:         sshJump.Password,
		Keyfile:          sshJump.Keyfile,
		Jump:             sshJump.Jump,
		ConfigAlias:      sshJump.ConfigAlias,
		RemoteKubeconfig: sshJump.RemoteKubeconfig,
		GSSAPIKeytabConf: sshJump.GSSAPIKeytabConf,
		GSSAPIPassword:   sshJump.GSSAPIPassword,
		GSSAPICacheFile:  sshJump.GSSAPICacheFile,
	}
}
