package ssh

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/kevinburke/ssh_config"
	"github.com/spf13/pflag"
	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/util/homedir"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
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
			return nil, fmt.Errorf("%w: %w", err, config.ErrGSSAPI)
		}
		auth = append(auth, ssh.GSSAPIWithMICAuthMethod(&c, host))
	} else if conf.GSSAPIKeytabConf != "" {
		c, err = NewKrb5InitiatorClientWithKeytab(conf.User, krb5Conf, conf.GSSAPIKeytabConf)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", err, config.ErrGSSAPI)
		}
		auth = append(auth, ssh.GSSAPIWithMICAuthMethod(&c, host))
	} else if conf.GSSAPICacheFile != "" {
		c, err = NewKrb5InitiatorClientWithCache(krb5Conf, conf.GSSAPICacheFile)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", err, config.ErrGSSAPI)
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
		return nil, fmt.Errorf("cannot resolve SSH key file path %s: %w: %w", file, err, config.ErrSSHAuth)
	}
	buffer, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("cannot read SSH key file %s: %w: %w", file, err, config.ErrSSHAuth)
	}
	key, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		return nil, fmt.Errorf("cannot parse SSH key file %s: %w: %w", file, err, config.ErrSSHAuth)
	}
	return ssh.PublicKeys(key), nil
}

// wrapDialError classifies an SSH dial/handshake error for exit-code purposes.
// x/crypto/ssh exposes no typed authentication error, so an auth failure is detected
// by its message text; GSSAPI/Kerberos token failures surface during the handshake too.
func wrapDialError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "kerberos") || strings.Contains(msg, "gssapi"):
		return fmt.Errorf("%w: %w", err, config.ErrGSSAPI)
	case strings.Contains(msg, "unable to authenticate"):
		return fmt.Errorf("%w: %w", err, config.ErrSSHAuth)
	default:
		return fmt.Errorf("%w: %w", err, config.ErrSSHConnect)
	}
}

// resolveProxyJumpChain walks the ProxyJump chain starting from the given alias,
// returning the ordered list of bastion configs. It returns an error if a cycle is detected.
func resolveProxyJumpChain(startAlias string, defaults SshConfig, list defaultSshConf) ([]SshConfig, error) {
	bastionList := []SshConfig{GetBastion(startAlias, defaults)}
	visited := map[string]bool{startAlias: true}
	name := startAlias
	for {
		value := list.Get(name, "ProxyJump")
		if value == "" {
			break
		}
		if visited[value] {
			return nil, fmt.Errorf("circular ProxyJump detected: %s -> %s: %w", name, value, config.ErrSSHConfig)
		}
		visited[value] = true
		bastionList = append(bastionList, GetBastion(value, defaults))
		name = value
	}
	return bastionList, nil
}

func (conf SshConfig) AliasRecursion(ctx context.Context, stopChan <-chan struct{}) (client *ssh.Client, err error) {
	list := getDefaultSSHConfigList()
	bastionList, err := resolveProxyJumpChain(conf.ConfigAlias, conf, list)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("parse ssh jump %q: %w: %w", conf.Jump, err, config.ErrSSHConfig)
	}
	var baseClient *ssh.Client
	baseClient, err = DialSshRemote(ctx, sshConf, stopChan)
	if err != nil {
		return nil, err
	}
	// Close baseClient only on error; on success it is the transport for the
	// jumped client (or the returned client itself when there is no further hop).
	defer func() {
		if err != nil && baseClient != nil {
			_ = baseClient.Close()
		}
	}()

	var bastionList []SshConfig
	if conf.ConfigAlias != "" {
		list := getDefaultSSHConfigList()
		bastionList, err = resolveProxyJumpChain(conf.ConfigAlias, conf, list)
		if err != nil {
			return nil, err
		}
	}
	if conf.Addr != "" {
		bastionList = append(bastionList, conf)
	}

	// No further hop configured: the jump host itself is the target.
	if len(bastionList) == 0 {
		return baseClient, nil
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
	d := net.Dialer{Timeout: sshOpTimeout, KeepAlive: config.KeepAliveTime}
	conn, err := d.DialContext(ctx, "tcp", conf.Addr)
	if err != nil {
		return nil, fmt.Errorf("dial ssh %s: %w: %w", conf.Addr, err, config.ErrSSHConnect)
	}
	go func() {
		if stopChan != nil {
			<-stopChan
			// Closing the underlying conn tears down both an in-progress handshake
			// and an established client (which is layered on conn). Avoid reading
			// the named return `client` here — it races with the function's return.
			_ = conn.Close()
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
		Timeout: sshOpTimeout,
	})
	if err != nil {
		return nil, wrapDialError(err)
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
