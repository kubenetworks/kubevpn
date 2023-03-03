package util

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"

	"golang.org/x/crypto/ssh"
)

type SshConfig struct {
	Addr     string
	User     string
	Password string
	Keyfile  string
}

func Main(ctx context.Context, remoteEndpoint, localEndpoint *netip.AddrPort, conf SshConfig, done chan struct{}) error {
	var auth []ssh.AuthMethod
	if conf.Keyfile != "" {
		auth = append(auth, publicKeyFile(conf.Keyfile))
	}
	if conf.Password != "" {
		auth = append(auth, ssh.Password(conf.Password))
	}
	// refer to https://godoc.org/golang.org/x/crypto/ssh for other authentication types
	sshConfig := &ssh.ClientConfig{
		// SSH connection username
		User:            conf.User,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// Connect to SSH remote server using serverEndpoint
	serverConn, err := ssh.Dial("tcp", conf.Addr, sshConfig)
	if err != nil {
		log.Fatalln(fmt.Printf("Dial INTO remote server error: %s", err))
	}

	// Listen on remote server port
	var lc net.ListenConfig
	listen, err := lc.Listen(ctx, "tcp", "localhost:0")
	if err != nil {
		return err
	}
	defer listen.Close()
	local, err := netip.ParseAddrPort(listen.Addr().String())
	if err != nil {
		return err
	}
	*localEndpoint = local
	done <- struct{}{}
	// handle incoming connections on reverse forwarded tunnel
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		accept, err := listen.Accept()
		if err != nil {
			return err
		}
		listener, err := serverConn.Dial("tcp", remoteEndpoint.String())
		if err != nil {
			return err
		}
		go handleClient(accept, listener)
	}
}

func publicKeyFile(file string) ssh.AuthMethod {
	buffer, err := os.ReadFile(file)
	if err != nil {
		log.Fatalln(fmt.Sprintf("Cannot read SSH public key file %s", file))
		return nil
	}

	key, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		log.Fatalln(fmt.Sprintf("Cannot parse SSH public key file %s", file))
		return nil
	}
	return ssh.PublicKeys(key)
}

// From https://sosedoff.com/2015/05/25/ssh-port-forwarding-with-go.html
func handleClient(client net.Conn, remote net.Conn) {
	defer client.Close()
	chDone := make(chan bool)

	// start remote -> local data transfer
	go func() {
		_, err := io.Copy(client, remote)
		if err != nil {
			log.Println(fmt.Sprintf("error while copy remote->local: %s", err))
		}
		chDone <- true
	}()

	// start local -> remote data transfer
	go func() {
		_, err := io.Copy(remote, client)
		if err != nil {
			log.Println(fmt.Sprintf("error while copy local->remote: %s", err))
		}
		chDone <- true
	}()

	<-chDone
}
