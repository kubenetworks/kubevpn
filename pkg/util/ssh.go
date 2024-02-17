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
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/util/homedir"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

type SshConfig struct {
	Addr             string
	User             string
	Password         string
	Keyfile          string
	ConfigAlias      string
	RemoteKubeconfig string
	// GSSAPI
	GSSAPIKeytabConf string
	GSSAPIPassword   string
	GSSAPICacheFile  string
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
		GSSAPIKeytabConf: sshJump.GSSAPIKeytabConf,
		GSSAPIPassword:   sshJump.GSSAPIPassword,
		GSSAPICacheFile:  sshJump.GSSAPICacheFile,
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
		GSSAPIKeytabConf: s.GSSAPIKeytabConf,
		GSSAPIPassword:   s.GSSAPIPassword,
		GSSAPICacheFile:  s.GSSAPICacheFile,
	}
}

// DialSshRemote https://github.com/golang/go/issues/21478
func DialSshRemote(ctx context.Context, conf *SshConfig) (*ssh.Client, error) {
	var remote *ssh.Client
	var err error
	if conf.ConfigAlias != "" {
		remote, err = jumpRecursion(conf.ConfigAlias)
	} else {
		if strings.Index(conf.Addr, ":") < 0 {
			// use default ssh port 22
			conf.Addr = net.JoinHostPort(conf.Addr, "22")
		}
		host, _, _ := net.SplitHostPort(conf.Addr)
		var auth []ssh.AuthMethod
		var c Krb5InitiatorClient
		var krb5Conf = GetKrb5Path()
		if conf.Password != "" {
			auth = append(auth, ssh.Password(conf.Password))
		} else if conf.GSSAPIPassword != "" {
			c, err = NewKrb5InitiatorClientWithPassword(conf.User, conf.GSSAPIPassword, krb5Conf)
			if err != nil {
				return nil, err
			}
			auth = append(auth, ssh.GSSAPIWithMICAuthMethod(&c, host))
		} else if conf.GSSAPIKeytabConf != "" {
			c, err = NewKrb5InitiatorClientWithKeytab(conf.User, krb5Conf, conf.GSSAPIKeytabConf)
			if err != nil {
				return nil, err
			}
		} else if conf.GSSAPICacheFile != "" {
			c, err = NewKrb5InitiatorClientWithCache(krb5Conf, conf.GSSAPICacheFile)
			if err != nil {
				return nil, err
			}
			auth = append(auth, ssh.GSSAPIWithMICAuthMethod(&c, host))
		} else {
			if conf.Keyfile == "" {
				conf.Keyfile = filepath.Join(homedir.HomeDir(), ".ssh", "id_rsa")
			}
			var keyFile ssh.AuthMethod
			keyFile, err = publicKeyFile(conf.Keyfile)
			if err != nil {
				return nil, err
			}
			auth = append(auth, keyFile)
		}

		// refer to https://godoc.org/golang.org/x/crypto/ssh for other authentication types
		sshConfig := &ssh.ClientConfig{
			// SSH connection username
			User:            conf.User,
			Auth:            auth,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			BannerCallback:  ssh.BannerDisplayStderr(),
			Timeout:         time.Second * 10,
		}
		// Connect to SSH remote server using serverEndpoint
		remote, err = ssh.Dial("tcp", conf.Addr, sshConfig)
	}

	// ref: https://github.com/golang/go/issues/21478
	if err == nil {
		go func() {
			ticker := time.NewTicker(time.Second * 5)
			defer ticker.Stop()
			for ctx.Err() == nil {
				select {
				case <-ticker.C:
					_, _, er := remote.SendRequest("keepalive@golang.org", true, nil)
					if er != nil {
						log.Errorf("failed to send keep alive error: %s", er)
						return
					}
				}
			}
		}()
	}
	return remote, err
}

func RemoteRun(client *ssh.Client, cmd string, env map[string]string) (output []byte, errOut []byte, err error) {
	var session *ssh.Session
	session, err = client.NewSession()
	if err != nil {
		return
	}
	defer session.Close()
	for k, v := range env {
		// /etc/ssh/sshd_config
		// AcceptEnv DEBIAN_FRONTEND
		if err = session.Setenv(k, v); err != nil {
			log.Warn(err)
			err = nil
		}
	}
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

func copyStream(local net.Conn, remote net.Conn) {
	chDone := make(chan bool, 2)

	// start remote -> local data transfer
	go func() {
		_, err := io.Copy(local, remote)
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
		_, err := io.Copy(remote, local)
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
		BannerCallback:  ssh.BannerDisplayStderr(),
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
		BannerCallback:  ssh.BannerDisplayStderr(),
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

func PortMapUntil(ctx context.Context, conf *SshConfig, remote, local netip.AddrPort) error {
	// Listen on remote server port
	var lc net.ListenConfig
	localListen, err := lc.Listen(ctx, "tcp", local.String())
	if err != nil {
		return err
	}

	var lock sync.Mutex
	var cancelFunc context.CancelFunc
	var sshClient *ssh.Client

	var getRemoteConnFunc = func() (net.Conn, error) {
		lock.Lock()
		defer lock.Unlock()

		if sshClient != nil {
			remoteConn, err := sshClient.Dial("tcp", remote.String())
			if err == nil {
				return remoteConn, nil
			}
			sshClient.Close()
			if cancelFunc != nil {
				cancelFunc()
			}
		}
		var ctx2 context.Context
		ctx2, cancelFunc = context.WithCancel(ctx)
		sshClient, err = DialSshRemote(ctx2, conf)
		if err != nil {
			cancelFunc()
			cancelFunc = nil
			log.Errorf("failed to dial remote ssh server: %v", err)
			return nil, err
		}
		return sshClient.Dial("tcp", remote.String())
	}

	go func() {
		defer localListen.Close()

		for ctx.Err() == nil {
			localConn, err := localListen.Accept()
			if err != nil {
				log.Errorf("failed to accept conn: %v", err)
				return
			}
			go func() {
				defer localConn.Close()

				remoteConn, err := getRemoteConnFunc()
				if err != nil {
					log.Errorf("Failed to dial %s: %s", remote.String(), err)
					return
				}
				defer remoteConn.Close()
				copyStream(localConn, remoteConn)
			}()
		}
	}()
	return nil
}
