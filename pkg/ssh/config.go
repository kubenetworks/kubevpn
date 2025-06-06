package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/util/homedir"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

type SshConfig struct {
	Addr             string
	User             string
	Password         string
	Keyfile          string
	Jump             string
	ConfigAlias      string
	RemoteKubeconfig string
	// GSSAPI
	GSSAPIKeytabConf string
	GSSAPIPassword   string
	GSSAPICacheFile  string
}

func (conf SshConfig) Clone() SshConfig {
	return SshConfig{
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

func ParseSshFromRPC(sshJump *rpc.SshJump) *SshConfig {
	if sshJump == nil {
		return &SshConfig{}
	}
	return &SshConfig{
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

func (conf SshConfig) ToRPC() *rpc.SshJump {
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

func (conf SshConfig) IsEmpty() bool {
	return conf.ConfigAlias == "" && conf.Addr == "" && conf.Jump == ""
}

func (conf SshConfig) GetAuth() ([]ssh.AuthMethod, error) {
	host, _, _ := net.SplitHostPort(conf.Addr)
	var auth []ssh.AuthMethod
	var c Krb5InitiatorClient
	var err error
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
	return auth, nil
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

func (conf SshConfig) AliasRecursion(ctx context.Context, stopChan <-chan struct{}) (client *ssh.Client, err error) {
	var name = conf.ConfigAlias
	var jumper = "ProxyJump"
	var bastionList = []SshConfig{GetBastion(name, conf)}
	for {
		value := defaultSshConfigList.Get(name, jumper)
		if value != "" {
			bastionList = append(bastionList, GetBastion(value, conf))
			name = value
			continue
		}
		break
	}
	for i := len(bastionList) - 1; i >= 0; i-- {
		if client == nil {
			client, err = bastionList[i].Dial(ctx, stopChan)
			if err != nil {
				err = errors.Wrap(err, fmt.Sprintf("Failed to connect to %v", bastionList[i]))
				return
			}
		} else {
			client, err = JumpTo(ctx, client, bastionList[i], stopChan)
			if err != nil {
				err = errors.Wrap(err, fmt.Sprintf("Failed to jump to %v", bastionList[i]))
				return
			}
		}
	}
	return
}

func (conf SshConfig) JumpRecursion(ctx context.Context, stopChan <-chan struct{}) (client *ssh.Client, err error) {
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	var sshConf = &SshConfig{}
	AddSshFlags(flags, sshConf)
	err = flags.Parse(strings.Split(conf.Jump, " "))
	if err != nil {
		return nil, err
	}
	var baseClient *ssh.Client
	baseClient, err = DialSshRemote(ctx, sshConf, stopChan)
	if err != nil {
		return nil, err
	}

	var bastionList []SshConfig
	if conf.ConfigAlias != "" {
		var name = conf.ConfigAlias
		var jumper = "ProxyJump"
		bastionList = append(bastionList, GetBastion(name, conf))
		for {
			value := defaultSshConfigList.Get(name, jumper)
			if value != "" {
				bastionList = append(bastionList, GetBastion(value, conf))
				name = value
				continue
			}
			break
		}
	}
	if conf.Addr != "" {
		bastionList = append(bastionList, conf)
	}

	for _, sshConfig := range bastionList {
		client, err = JumpTo(ctx, baseClient, sshConfig, stopChan)
		if err != nil {
			err = errors.Wrap(err, fmt.Sprintf("Failed to jump to %s", sshConfig))
			return
		}
	}
	return
}

func (conf SshConfig) Dial(ctx context.Context, stopChan <-chan struct{}) (client *ssh.Client, err error) {
	if _, _, err = net.SplitHostPort(conf.Addr); err != nil {
		// use default ssh port 22
		conf.Addr = net.JoinHostPort(conf.Addr, "22")
		err = nil
	}
	// connect to the bastion host
	authMethod, err := conf.GetAuth()
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: time.Second * 10, KeepAlive: config.KeepAliveTime}
	conn, err := d.DialContext(ctx, "tcp", conf.Addr)
	if err != nil {
		return nil, err
	}
	go func() {
		if stopChan != nil {
			<-stopChan
			conn.Close()
			if client != nil {
				client.Close()
			}
		}
	}()
	defer func() {
		if err != nil {
			if conn != nil {
				conn.Close()
			}
			if client != nil {
				client.Close()
			}
		}
	}()
	c, chans, reqs, err := ssh.NewClientConn(conn, conf.Addr, &ssh.ClientConfig{
		User:            conf.User,
		Auth:            authMethod,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		//BannerCallback:  ssh.BannerDisplayStderr(),
		Timeout: time.Second * 10,
	})
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func GetBastion(name string, defaultValue SshConfig) SshConfig {
	var host, port string
	conf := SshConfig{
		ConfigAlias: name,
	}
	var propertyList = []string{"ProxyJump", "Hostname", "User", "Port", "IdentityFile"}
	for i, s := range propertyList {
		value := defaultSshConfigList.Get(name, s)
		switch i {
		case 0:

		case 1:
			host = value
		case 2:
			conf.User = value
		case 3:
			if port = value; port == "" {
				port = strconv.Itoa(22)
			}
		case 4:
			if value == "" {
				conf.Keyfile = defaultValue.Keyfile
				conf.Password = defaultValue.Password
				conf.GSSAPIKeytabConf = defaultValue.GSSAPIKeytabConf
				conf.GSSAPIPassword = defaultValue.GSSAPIPassword
				conf.GSSAPICacheFile = defaultValue.GSSAPICacheFile
			} else {
				conf.Keyfile = value
			}
		}
	}
	conf.Addr = net.JoinHostPort(host, port)
	return conf
}

type defaultSshConf []*ssh_config.Config

func (c defaultSshConf) Get(alias string, key string) string {
	for _, s := range c {
		if v, err := s.Get(alias, key); err == nil {
			return v
		}
	}
	return ssh_config.Get(alias, key)
}

var once sync.Once

var defaultSshConfigList defaultSshConf

func init() {
	once.Do(func() {
		paths := []string{
			filepath.Join(homedir.HomeDir(), ".ssh", "config"),
			filepath.Join("/", "etc", "ssh", "ssh_config"),
		}
		for _, path := range paths {
			file, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			cfg, err := ssh_config.DecodeBytes(file)
			if err != nil {
				continue
			}
			defaultSshConfigList = append(defaultSshConfigList, cfg)
		}
	})
}

func newSshClientWrap(client *ssh.Client, cancel context.CancelFunc) *sshClientWrap {
	return &sshClientWrap{Client: client, cancel: cancel}
}

type sshClientWrap struct {
	cancel context.CancelFunc
	*ssh.Client
}

func (c *sshClientWrap) Close() error {
	c.cancel()
	return c.Client.Close()
}

func AddSshFlags(flags *pflag.FlagSet, sshConf *SshConfig) {
	// for ssh jumper host
	flags.StringVar(&sshConf.Addr, "ssh-addr", "", "Optional ssh jump server address to dial as <hostname>:<port>, eg: 127.0.0.1:22")
	flags.StringVar(&sshConf.User, "ssh-username", "", "Optional username for ssh jump server")
	flags.StringVar(&sshConf.Password, "ssh-password", "", "Optional password for ssh jump server")
	flags.StringVar(&sshConf.Keyfile, "ssh-keyfile", "", "Optional file with private key for SSH authentication")
	flags.StringVar(&sshConf.ConfigAlias, "ssh-alias", "", "Optional config alias with ~/.ssh/config for SSH authentication")
	flags.StringVar(&sshConf.Jump, "ssh-jump", "", "Optional bastion jump config string, eg: '--ssh-addr jumpe.naison.org --ssh-username naison --gssapi-password xxx'")
	flags.StringVar(&sshConf.GSSAPIPassword, "gssapi-password", "", "GSSAPI password")
	flags.StringVar(&sshConf.GSSAPIKeytabConf, "gssapi-keytab", "", "GSSAPI keytab file path")
	flags.StringVar(&sshConf.GSSAPICacheFile, "gssapi-cache", "", "GSSAPI cache file path, use command `kinit -c /path/to/cache USERNAME@RELAM` to generate")
	flags.StringVar(&sshConf.RemoteKubeconfig, "remote-kubeconfig", "~/.kube/config", "Remote kubeconfig abstract path of ssh server, default is /home/$USERNAME/.kube/config")
}

func keepAlive(cl *ssh.Client, conn net.Conn, done <-chan struct{}) error {
	const keepAliveInterval = time.Second * 10
	t := time.NewTicker(keepAliveInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_, _, err := cl.SendRequest("keepalive@golang.org", true, nil)
			if err != nil && err != io.EOF {
				return errors.Wrap(err, "failed to send keep alive")
			}
		case <-done:
			return nil
		}
	}
}
