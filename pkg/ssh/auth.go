package ssh

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	gossh "golang.org/x/crypto/ssh"
	"k8s.io/client-go/util/homedir"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// GetAuth returns the SSH authentication methods derived from the config (password, GSSAPI, or public key).
func (conf SshConfig) GetAuth() ([]gossh.AuthMethod, error) {
	host, _, _ := net.SplitHostPort(conf.Addr)
	var auth []gossh.AuthMethod
	var c Krb5InitiatorClient
	var err error
	var krb5Conf = GetKrb5Path()
	if conf.Password != "" {
		auth = append(auth, gossh.Password(conf.Password))
	} else if conf.GSSAPIPassword != "" {
		c, err = NewKrb5InitiatorClientWithPassword(conf.User, conf.GSSAPIPassword, krb5Conf)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", err, config.ErrGSSAPI)
		}
		auth = append(auth, gossh.GSSAPIWithMICAuthMethod(&c, host))
	} else if conf.GSSAPIKeytabConf != "" {
		c, err = NewKrb5InitiatorClientWithKeytab(conf.User, krb5Conf, conf.GSSAPIKeytabConf)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", err, config.ErrGSSAPI)
		}
		auth = append(auth, gossh.GSSAPIWithMICAuthMethod(&c, host))
	} else if conf.GSSAPICacheFile != "" {
		c, err = NewKrb5InitiatorClientWithCache(krb5Conf, conf.GSSAPICacheFile)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", err, config.ErrGSSAPI)
		}
		auth = append(auth, gossh.GSSAPIWithMICAuthMethod(&c, host))
	} else {
		if conf.Keyfile == "" {
			conf.Keyfile = filepath.Join(homedir.HomeDir(), ".ssh", "id_rsa")
		}
		var keyFile gossh.AuthMethod
		keyFile, err = publicKeyFile(conf.Keyfile)
		if err != nil {
			return nil, err
		}
		auth = append(auth, keyFile)
	}
	return auth, nil
}

func publicKeyFile(file string) (gossh.AuthMethod, error) {
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
	key, err := gossh.ParsePrivateKey(buffer)
	if err != nil {
		return nil, fmt.Errorf("cannot parse SSH key file %s: %w: %w", file, err, config.ErrSSHAuth)
	}
	return gossh.PublicKeys(key), nil
}
