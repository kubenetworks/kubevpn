package ssh

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kevinburke/ssh_config"
	"github.com/spf13/pflag"
	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/util/homedir"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// SshConfig holds the configuration for an SSH connection including authentication credentials and jump host settings.
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

func (conf *SshConfig) KubeconfigIdentifier() string {
	var prefix string
	if conf.ConfigAlias != "" {
		prefix = conf.ConfigAlias
	} else if conf.Addr != "" {
		if host, port, err := net.SplitHostPort(conf.Addr); err == nil {
			prefix = fmt.Sprintf("%s_%s", IPToFilename(host), port)
		} else {
			prefix = IPToFilename(conf.Addr)
		}
	} else if conf.Jump != "" {
		flags := pflag.NewFlagSet("", pflag.ContinueOnError)
		sshConf := &SshConfig{}
		AddSshFlags(flags, sshConf)
		_ = flags.Parse(strings.Split(conf.Jump, " "))
		prefix = sshConf.KubeconfigIdentifier()
	}

	if prefix == "" {
		return filepath.Base(conf.RemoteKubeconfig)
	}

	return fmt.Sprintf("%s_%s", prefix, filepath.Base(conf.RemoteKubeconfig))
}

// ParseSshFromRPC converts an RPC SshJump message into an SshConfig struct.
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

// ToRPC converts the SshConfig into an RPC SshJump message for gRPC transport.
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

// IsEmpty reports whether the SSH config has no address, alias, or jump host configured.
func (conf SshConfig) IsEmpty() bool {
	return conf.ConfigAlias == "" && conf.Addr == "" && conf.Jump == ""
}

// IsLoopback TODO support alias and proxyJump
func (conf SshConfig) IsLoopback() bool {
	for _, ip := range conf.Host() {
		if ip.IsLoopback() {
			return true
		}
	}
	return false
}

// Host resolves the SSH server address to a list of IP addresses via DNS lookup.
func (conf SshConfig) Host() []net.IP {
	if conf.Addr != "" {
		var host string
		var err error
		if host, _, err = net.SplitHostPort(conf.Addr); err != nil {
			host = conf.Addr
		}
		ip, err := net.LookupIP(host)
		if err != nil {
			return []net.IP{}
		}
		return ip
	}
	return []net.IP{}
}

// GetAuth returns the SSH authentication methods derived from the config (password, GSSAPI, or public key).
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
		auth = append(auth, ssh.GSSAPIWithMICAuthMethod(&c, host))
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
	if len(file) != 0 && file[0] == '~' {
		file = filepath.Join(homedir.HomeDir(), file[1:])
	}
	file, err := filepath.Abs(file)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve SSH key file path %s: %w", file, err)
	}
	buffer, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("cannot read SSH key file %s: %w", file, err)
	}
	key, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		return nil, fmt.Errorf("cannot parse SSH key file %s: %w", file, err)
	}
	return ssh.PublicKeys(key), nil
}

func (conf SshConfig) AliasRecursion(ctx context.Context, stopChan <-chan struct{}) (client *ssh.Client, err error) {
	name := conf.ConfigAlias
	jumper := "ProxyJump"
	bastionList := []SshConfig{GetBastion(name, conf)}
	list := getDefaultSSHConfigList()
	visited := map[string]bool{name: true}
	for {
		value := list.Get(name, jumper)
		if value == "" {
			break
		}
		if visited[value] {
			return nil, fmt.Errorf("circular ProxyJump detected: %s -> %s", name, value)
		}
		visited[value] = true
		bastionList = append(bastionList, GetBastion(value, conf))
		name = value
	}
	for i := len(bastionList) - 1; i >= 0; i-- {
		if client == nil {
			client, err = bastionList[i].Dial(ctx, stopChan)
			if err != nil {
				err = fmt.Errorf("failed to connect to %v: %w", bastionList[i], err)
				return
			}
		} else {
			client, err = JumpTo(ctx, client, bastionList[i], stopChan)
			if err != nil {
				err = fmt.Errorf("failed to jump to %v: %w", bastionList[i], err)
				return
			}
		}
	}
	return
}

func (conf SshConfig) JumpRecursion(ctx context.Context, stopChan <-chan struct{}) (client *ssh.Client, err error) {
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	sshConf := &SshConfig{}
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
		name := conf.ConfigAlias
		jumper := "ProxyJump"
		bastionList = append(bastionList, GetBastion(name, conf))
		list := getDefaultSSHConfigList()
		visited := map[string]bool{name: true}
		for {
			value := list.Get(name, jumper)
			if value == "" {
				break
			}
			if visited[value] {
				return nil, fmt.Errorf("circular ProxyJump detected: %s -> %s", name, value)
			}
			visited[value] = true
			bastionList = append(bastionList, GetBastion(value, conf))
			name = value
		}
	}
	if conf.Addr != "" {
		bastionList = append(bastionList, conf)
	}

	for _, sshConfig := range bastionList {
		client, err = JumpTo(ctx, baseClient, sshConfig, stopChan)
		if err != nil {
			err = fmt.Errorf("failed to jump to %s: %w", sshConfig, err)
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

// GetBastion resolves SSH config values for the given alias name from ~/.ssh/config, falling back to defaultValue credentials.
func GetBastion(name string, defaultValue SshConfig) SshConfig {
	list := getDefaultSSHConfigList()
	host := list.Get(name, "Hostname")
	port := list.Get(name, "Port")
	if port == "" {
		port = "22"
	}
	keyfile := list.Get(name, "IdentityFile")

	conf := SshConfig{
		ConfigAlias: name,
		User:        list.Get(name, "User"),
		Addr:        net.JoinHostPort(host, port),
	}
	if keyfile != "" {
		conf.Keyfile = keyfile
	} else {
		conf.Keyfile = defaultValue.Keyfile
		conf.Password = defaultValue.Password
		conf.GSSAPIKeytabConf = defaultValue.GSSAPIKeytabConf
		conf.GSSAPIPassword = defaultValue.GSSAPIPassword
		conf.GSSAPICacheFile = defaultValue.GSSAPICacheFile
	}
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

func getDefaultSSHConfigList() defaultSshConf {
	var defaultSshConfigList defaultSshConf
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
	return defaultSshConfigList
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

// AddSshFlags registers SSH-related command-line flags (addr, username, password, keyfile, etc.) on the flag set.
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
	flags.StringVar(&sshConf.RemoteKubeconfig, "remote-kubeconfig", "", "Abstract path of kubeconfig on ssh remote server")
}
