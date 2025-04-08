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

func SSHListener(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	plog.G(context.Background()).Infof("starting ssh server on port %s...", addr)
	return ln, err
}

func SSHHandler() Handler {
	return &sshHandler{}
}

type sshHandler struct {
}

func (s *sshHandler) Handle(ctx context.Context, conn net.Conn) {
	forwardHandler := &ssh.ForwardedTCPHandler{}
	server := ssh.Server{
		LocalPortForwardingCallback: ssh.LocalPortForwardingCallback(func(ctx ssh.Context, dhost string, dport uint32) bool {
			plog.G(ctx).Infoln("Accepted forward", dhost, dport)
			return true
		}),
		Handler: ssh.Handler(func(s ssh.Session) {
			io.WriteString(s, "Remote forwarding available...\n")
			select {}
		}),
		ReversePortForwardingCallback: ssh.ReversePortForwardingCallback(func(ctx ssh.Context, host string, port uint32) bool {
			plog.G(ctx).Infoln("attempt to bind", host, port, "granted")
			return true
		}),
		RequestHandlers: map[string]ssh.RequestHandler{
			"tcpip-forward":        forwardHandler.HandleSSHRequest,
			"cancel-tcpip-forward": forwardHandler.HandleSSHRequest,
		},
		SubsystemHandlers: ssh.DefaultSubsystemHandlers,
		ChannelHandlers:   ssh.DefaultChannelHandlers,
		HostSigners: func() []ssh.Signer {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
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
