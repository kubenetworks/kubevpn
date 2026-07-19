package ssh

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"
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
