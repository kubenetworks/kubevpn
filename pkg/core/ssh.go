package core

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// sshHostKeyBits is the RSA key size used for the ephemeral SSH server host key.
const sshHostKeyBits = 2048

// SSHListener creates a TCP listener for the SSH server protocol handler.
func SSHListener(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	plog.G(context.Background()).Infof("[SSH] Listening on %s", addr)
	return ln, err
}

// SSHHandler creates a Handler for SSH protocol connections.
func SSHHandler() Handler {
	return &sshHandler{}
}

type sshHandler struct {
}

func (s *sshHandler) Handle(ctx context.Context, conn net.Conn) {
	forwardHandler := &ssh.ForwardedTCPHandler{}
	server := ssh.Server{
		LocalPortForwardingCallback: ssh.LocalPortForwardingCallback(func(ctx ssh.Context, dhost string, dport uint32) bool {
			plog.G(ctx).Infof("[SSH] Accepted local forward to %s:%d", dhost, dport)
			return true
		}),
		Handler: ssh.Handler(func(s ssh.Session) {
			io.WriteString(s, "Remote forwarding available...\n")
			<-s.Context().Done()
		}),
		ReversePortForwardingCallback: ssh.ReversePortForwardingCallback(func(ctx ssh.Context, host string, port uint32) bool {
			plog.G(ctx).Infof("[SSH] Reverse port forward granted: %s:%d", host, port)
			return true
		}),
		RequestHandlers: map[string]ssh.RequestHandler{
			"tcpip-forward":        forwardHandler.HandleSSHRequest,
			"cancel-tcpip-forward": forwardHandler.HandleSSHRequest,
		},
		SubsystemHandlers: ssh.DefaultSubsystemHandlers,
		ChannelHandlers:   ssh.DefaultChannelHandlers,
		HostSigners: func() []ssh.Signer {
			key, err := rsa.GenerateKey(rand.Reader, sshHostKeyBits)
			if err != nil {
				return nil
			}
			fromKey, err := gossh.NewSignerFromKey(key)
			if err != nil {
				return nil
			}
			return []ssh.Signer{fromKey}
		}(),
	}
	server.HandleConn(conn)
}
