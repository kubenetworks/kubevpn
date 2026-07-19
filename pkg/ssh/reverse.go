/*
SSH Reverse Tunnel, the equivalent of below SSH command:
   ssh -R 8080:127.0.0.1:8080 operatore@146.148.22.123
which opens a tunnel between the two endpoints and permit to exchange information on this direction:
   server:8080 -----> client:8080
   once authenticated a process on the SSH server can interact with the service answering to port 8080 of the client
   without any NAT rule via firewall
*/

package ssh

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	gossh "golang.org/x/crypto/ssh"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// ExposeLocalPortToRemote opens a reverse SSH tunnel, forwarding a remote port on the SSH server to a local service.
func ExposeLocalPortToRemote(ctx context.Context, remoteSSHServer, remotePort, localPort netip.AddrPort) error {
	// refer to https://godoc.org/golang.org/x/crypto/ssh for other authentication types
	sshConfig := &gossh.ClientConfig{
		Auth:            []gossh.AuthMethod{},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         sshDialTimeout,
	}

	// Connect to SSH remote server using serverEndpoint
	serverConn, err := gossh.Dial("tcp", remoteSSHServer.String(), sshConfig)
	if err != nil {
		return fmt.Errorf("failed to dial remote SSH server %s: %w", remoteSSHServer.String(), err)
	}
	defer serverConn.Close()

	// Listen on remote server port
	listener, err := serverConn.Listen("tcp", remotePort.String())
	if err != nil {
		return fmt.Errorf("failed to open remote listener on %s: %w", remotePort.String(), err)
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		listener.Close()
		serverConn.Close()
	}()

	// handle incoming connections on reverse forwarded tunnel
	for {
		client, err := listener.Accept()
		if err != nil {
			// A closed listener during teardown is expected, not an error.
			plog.G(ctx).Debugf("stop accepting on remote listener: %v", err)
			return err
		}
		go func(client net.Conn) {
			defer client.Close()
			// Open a (local) connection to localEndpoint whose content will be forwarded so serverEndpoint
			local, err := net.Dial("tcp", localPort.String())
			if err != nil {
				plog.G(ctx).Debugf("failed to dial local service %s: %v", localPort.String(), err)
				return
			}
			defer local.Close()
			copyStream(ctx, client, local)
		}(client)
	}
}
