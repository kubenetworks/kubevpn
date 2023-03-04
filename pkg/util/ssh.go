package util

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/util/homedir"
)

type SshConfig struct {
	Addr        string
	User        string
	Password    string
	Keyfile     string
	ConfigAlias string
}

func Main(remoteEndpoint, localEndpoint *netip.AddrPort, conf SshConfig, done chan struct{}) error {
	var remote *ssh.Client
	var err error
	if conf.ConfigAlias != "" {
		remote, err = jumpRecursion(conf.ConfigAlias)
	} else {
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
		remote, err = ssh.Dial("tcp", conf.Addr, sshConfig)
	}
	if err != nil {
		log.Errorf("Dial INTO remote server error: %s", err)
		return err
	}

	// Listen on remote server port
	listen, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return err
	}
	defer listen.Close()

	*localEndpoint, err = netip.ParseAddrPort(listen.Addr().String())
	if err != nil {
		return err
	}
	done <- struct{}{}
	// handle incoming connections on reverse forwarded tunnel
	for {
		local, err := listen.Accept()
		if err != nil {
			log.Error(err)
			continue
		}
		go func() {
			defer local.Close()
			var conn net.Conn
			var err error
			for i := 0; i < 5; i++ {
				conn, err = remote.Dial("tcp", remoteEndpoint.String())
				if err == nil {
					break
				}
				time.Sleep(time.Second)
			}
			if conn == nil {
				return
			}
			handleClient(local, conn)
		}()
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

func handleClient(client net.Conn, remote net.Conn) {
	chDone := make(chan bool)

	// start remote -> local data transfer
	go func() {
		_, err := io.Copy(client, remote)
		if err != nil {
			log.Debugf("error while copy remote->local: %s", err)
		}
		chDone <- true
	}()

	// start local -> remote data transfer
	go func() {
		_, err := io.Copy(remote, client)
		if err != nil {
			log.Debugf("error while copy local->remote: %s", err)
		}
		chDone <- true
	}()

	<-chDone
}

func jumpRecursion(name string) (client *ssh.Client, err error) {
	var jumper = "ProxyJump"
	var bastionList = []*SshConfig{getBastion(name)}
	for {
		value := confList.Get(name, jumper)
		if value != "" {
			bastionList = append(bastionList, getBastion(value))
			name = value
			continue
		}
		break
	}
	for i := len(bastionList) - 1; i >= 0; i-- {
		if bastionList[i] == nil {
			return nil, errors.New("config is nil")
		}
		if client == nil {
			client, err = dial(bastionList[i])
			if err != nil {
				return
			}
		} else {
			client, err = jump(client, bastionList[i])
			if err != nil {
				return
			}
		}
	}
	return
}

func getBastion(name string) *SshConfig {
	var host, port string
	config := SshConfig{
		ConfigAlias: name,
	}
	var propertyList = []string{"ProxyJump", "Hostname", "User", "Port", "IdentityFile"}
	for i, s := range propertyList {
		value := confList.Get(name, s)
		switch i {
		case 0:

		case 1:
			host = value
		case 2:
			config.User = value
		case 3:
			if port = value; port == "" {
				port = strconv.Itoa(22)
			}
		case 4:
			config.Keyfile = value
		}
	}
	config.Addr = fmt.Sprintf("%s:%s", host, port)
	return &config
}

func dial(from *SshConfig) (*ssh.Client, error) {
	// connect to the bastion host
	return ssh.Dial("tcp", from.Addr, &ssh.ClientConfig{
		User:            from.User,
		Auth:            []ssh.AuthMethod{publicKeyFile(from.Keyfile)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
}

func jump(bClient *ssh.Client, to *SshConfig) (*ssh.Client, error) {
	// Dial a connection to the service host, from the bastion
	conn, err := bClient.Dial("tcp", to.Addr)
	if err != nil {
		return nil, err
	}

	ncc, chans, reqs, err := ssh.NewClientConn(conn, to.Addr, &ssh.ClientConfig{
		User:            to.User,
		Auth:            []ssh.AuthMethod{publicKeyFile(to.Keyfile)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		return nil, err
	}

	sClient := ssh.NewClient(ncc, chans, reqs)
	return sClient, nil
}

type conf []*ssh_config.Config

func (c conf) Get(alias string, key string) string {
	for _, s := range c {
		if v, err := s.Get(alias, key); err == nil {
			return v
		}
	}
	return ""
}

var once sync.Once
var confList conf

func init() {
	once.Do(func() {
		strings := []string{
			filepath.Join(homedir.HomeDir(), ".ssh", "config"),
			filepath.Join("/", "etc", "ssh", "ssh_config"),
		}
		for _, s := range strings {
			file, err := os.ReadFile(s)
			if err != nil {
				continue
			}
			cfg, err := ssh_config.DecodeBytes(file)
			if err != nil {
				continue
			}
			confList = append(confList, cfg)
		}
	})
}
