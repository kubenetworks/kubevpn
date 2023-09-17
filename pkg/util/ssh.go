package util

import (
	"bytes"
	"context"
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
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/util/homedir"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

type SshConfig struct {
	Addr             string
	User             string
	Password         string
	Keyfile          string
	ConfigAlias      string
	RemoteKubeconfig string
}

func ParseSshFromRPC(sshJump *rpc.SshJump) *SshConfig {
	if sshJump == nil {
		return &SshConfig{}
	}
	return &SshConfig{
		Addr:             sshJump.Addr,
		User:             sshJump.User,
		Password:         sshJump.Password,
		Keyfile:          sshJump.Keyfile,
		ConfigAlias:      sshJump.ConfigAlias,
		RemoteKubeconfig: sshJump.RemoteKubeconfig,
	}
}

func (s *SshConfig) ToRPC() *rpc.SshJump {
	return &rpc.SshJump{
		Addr:             s.Addr,
		User:             s.User,
		Password:         s.Password,
		Keyfile:          s.Keyfile,
		ConfigAlias:      s.ConfigAlias,
		RemoteKubeconfig: s.RemoteKubeconfig,
	}
}

func Main(pctx context.Context, remoteEndpoint, localEndpoint netip.AddrPort, conf *SshConfig, done chan struct{}) error {
	ctx, cancelFunc := context.WithCancel(pctx)
	defer cancelFunc()

	var remote *ssh.Client
	var err error
	if conf.ConfigAlias != "" {
		remote, err = jumpRecursion(conf.ConfigAlias)
	} else {
		var auth []ssh.AuthMethod
		if conf.Keyfile != "" {
			var keyFile ssh.AuthMethod
			keyFile, err = publicKeyFile(conf.Keyfile)
			if err != nil {
				return err
			}
			auth = append(auth, keyFile)
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
			Timeout:         time.Second * 10,
		}
		// Connect to SSH remote server using serverEndpoint
		remote, err = ssh.Dial("tcp", conf.Addr, sshConfig)
	}
	if err != nil {
		log.Errorf("Dial into remote server error: %s", err)
		return err
	}

	// Listen on remote server port
	var lc net.ListenConfig
	listen, err := lc.Listen(ctx, "tcp", localEndpoint.String())
	if err != nil {
		return err
	}
	defer listen.Close()

	select {
	case done <- struct{}{}:
	default:
	}
	// handle incoming connections on reverse forwarded tunnel
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		local, err := listen.Accept()
		if err != nil {
			return err
		}
		go func(local net.Conn) {
			defer local.Close()
			var conn net.Conn
			var err error
			for i := 0; i < 5; i++ {
				conn, err = remote.Dial("tcp", remoteEndpoint.String())
				if err == nil {
					break
				}
				time.Sleep(time.Millisecond * 200)
			}
			if conn == nil {
				cancelFunc()
				return
			}
			defer conn.Close()
			handleClient(local, conn)
		}(local)
	}
}

func Run(conf *SshConfig, cmd string, env []string) (output []byte, errOut []byte, err error) {
	var remote *ssh.Client
	if conf.ConfigAlias != "" {
		remote, err = jumpRecursion(conf.ConfigAlias)
	} else {
		var auth []ssh.AuthMethod
		if conf.Keyfile != "" {
			var keyFile ssh.AuthMethod
			keyFile, err = publicKeyFile(conf.Keyfile)
			if err != nil {
				return
			}
			auth = append(auth, keyFile)
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
			Timeout:         time.Second * 10,
		}
		// Connect to SSH remote server using serverEndpoint
		remote, err = ssh.Dial("tcp", conf.Addr, sshConfig)
	}
	if err != nil {
		log.Errorf("Dial into remote server error: %s", err)
		return
	}
	defer remote.Close()
	var session *ssh.Session
	session, err = remote.NewSession()
	if err != nil {
		return
	}
	if len(env) == 2 {
		// /etc/ssh/sshd_config
		// AcceptEnv DEBIAN_FRONTEND
		if err = session.Setenv(env[0], env[1]); err != nil {
			log.Warn(err)
			err = nil
		}
	}
	defer remote.Close()
	var out bytes.Buffer
	var er bytes.Buffer
	session.Stdout = &out
	session.Stderr = &er
	err = session.Run(cmd)
	return out.Bytes(), er.Bytes(), err
}

func publicKeyFile(file string) (ssh.AuthMethod, error) {
	var err error
	if len(file) != 0 && file[0] == '~' {
		file = filepath.Join(homedir.HomeDir(), file[1:])
	}
	file, err = filepath.Abs(file)
	if err != nil {
		err = errors.Wrap(err, fmt.Sprintf("Cannot read SSH public key file %s", file))
		return nil, err
	}
	buffer, err := os.ReadFile(file)
	if err != nil {
		err = errors.Wrap(err, fmt.Sprintf("Cannot read SSH public key file %s", file))
		return nil, err
	}

	key, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		err = errors.Wrap(err, fmt.Sprintf("Cannot parse SSH public key file %s", file))
		return nil, err
	}
	return ssh.PublicKeys(key), nil
}

func handleClient(client net.Conn, remote net.Conn) {
	chDone := make(chan bool, 2)

	// start remote -> local data transfer
	go func() {
		_, err := io.Copy(client, remote)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Debugf("error while copy remote->local: %s", err)
		}
		select {
		case chDone <- true:
		default:
		}
	}()

	// start local -> remote data transfer
	go func() {
		_, err := io.Copy(remote, client)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Debugf("error while copy local->remote: %s", err)
		}
		select {
		case chDone <- true:
		default:
		}
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
	config.Addr = net.JoinHostPort(host, port)
	return &config
}

func dial(from *SshConfig) (*ssh.Client, error) {
	// connect to the bastion host
	authMethod, err := publicKeyFile(from.Keyfile)
	if err != nil {
		return nil, err
	}
	return ssh.Dial("tcp", from.Addr, &ssh.ClientConfig{
		User:            from.User,
		Auth:            []ssh.AuthMethod{authMethod},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second * 10,
	})
}

func jump(bClient *ssh.Client, to *SshConfig) (*ssh.Client, error) {
	// Dial a connection to the service host, from the bastion
	conn, err := bClient.Dial("tcp", to.Addr)
	if err != nil {
		return nil, err
	}

	authMethod, err := publicKeyFile(to.Keyfile)
	if err != nil {
		return nil, err
	}
	ncc, chans, reqs, err := ssh.NewClientConn(conn, to.Addr, &ssh.ClientConfig{
		User:            to.User,
		Auth:            []ssh.AuthMethod{authMethod},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second * 10,
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
	return ssh_config.Get(alias, key)
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
