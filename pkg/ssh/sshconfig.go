package ssh

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/kevinburke/ssh_config"
	"k8s.io/client-go/util/homedir"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

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
