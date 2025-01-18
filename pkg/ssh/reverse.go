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
	"net"
	"net/netip"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// ExposeLocalPortToRemote
// remote forwarding port (on remote SSH server network)
// local service to be forwarded
func ExposeLocalPortToRemote(ctx context.Context, remoteSSHServer, remotePort, localPort netip.AddrPort) error {
	// refer to https://godoc.org/golang.org/x/crypto/ssh for other authentication types
	sshConfig := &ssh.ClientConfig{
		Auth:            []ssh.AuthMethod{},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second * 10,
	}

	// Connect to SSH remote server using serverEndpoint
	serverConn, err := ssh.Dial("tcp", remoteSSHServer.String(), sshConfig)
	if err != nil {
		log.Errorf("Dial INTO remote server error: %s", err)
		return err
	}

	// Listen on remote server port
	listener, err := serverConn.Listen("tcp", remotePort.String())
	if err != nil {
		log.Errorf("Listen open port ON remote server error: %s", err)
		return err
	}
	defer listener.Close()

	// handle incoming connections on reverse forwarded tunnel
	for {
		client, err := listener.Accept()
		if err != nil {
			log.Errorf("Accept on remote service error: %s", err)
			return err
		}
		go func(client net.Conn) {
			defer client.Close()
			// Open a (local) connection to localEndpoint whose content will be forwarded so serverEndpoint
			local, err := net.Dial("tcp", localPort.String())
			if err != nil {
				log.Errorf("Dial INTO local service error: %s", err)
				return
			}
			defer local.Close()
			copyStream(ctx, client, local)
		}(client)
	}
}
