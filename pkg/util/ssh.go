package util

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	"k8s.io/client-go/util/homedir"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/pointer"

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

func (s SshConfig) Clone() SshConfig {
	return SshConfig{
		Addr:             s.Addr,
		User:             s.User,
		Password:         s.Password,
		Keyfile:          s.Keyfile,
		Jump:             s.Jump,
		ConfigAlias:      s.ConfigAlias,
		RemoteKubeconfig: s.RemoteKubeconfig,
		GSSAPIKeytabConf: s.GSSAPIKeytabConf,
		GSSAPIPassword:   s.GSSAPIPassword,
		GSSAPICacheFile:  s.GSSAPICacheFile,
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

func (config *SshConfig) ToRPC() *rpc.SshJump {
	return &rpc.SshJump{
		Addr:             config.Addr,
		User:             config.User,
		Password:         config.Password,
		Keyfile:          config.Keyfile,
		Jump:             config.Jump,
		ConfigAlias:      config.ConfigAlias,
		RemoteKubeconfig: config.RemoteKubeconfig,
		GSSAPIKeytabConf: config.GSSAPIKeytabConf,
		GSSAPIPassword:   config.GSSAPIPassword,
		GSSAPICacheFile:  config.GSSAPICacheFile,
	}
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
	flags.StringVar(&sshConf.RemoteKubeconfig, "remote-kubeconfig", "", "Remote kubeconfig abstract path of ssh server, default is /home/$USERNAME/.kube/config")
	lookup := flags.Lookup("remote-kubeconfig")
	lookup.NoOptDefVal = "~/.kube/config"
}

// DialSshRemote https://github.com/golang/go/issues/21478
func DialSshRemote(ctx context.Context, conf *SshConfig) (remote *ssh.Client, err error) {
	defer func() {
		if err != nil {
			if remote != nil {
				remote.Close()
			}
		}
	}()

	if conf.ConfigAlias != "" {
		remote, err = conf.AliasRecursion(ctx)
	} else if conf.Jump != "" {
		remote, err = conf.JumpRecursion(ctx)
	} else {
		remote, err = conf.Dial(ctx)
	}

	// ref: https://github.com/golang/go/issues/21478
	if err == nil {
		go func() {
			defer remote.Close()
			for ctx.Err() == nil {
				time.Sleep(time.Second * 15)
				_, _, err := remote.SendRequest("keepalive@golang.org", true, nil)
				if err == nil || err.Error() == "request failed" {
					// Any response is a success.
					continue
				}
				if err != nil {
					return
				}
			}
		}()
	}
	return remote, err
}

func (config SshConfig) GetAuth() ([]ssh.AuthMethod, error) {
	host, _, _ := net.SplitHostPort(config.Addr)
	var auth []ssh.AuthMethod
	var c Krb5InitiatorClient
	var err error
	var krb5Conf = GetKrb5Path()
	if config.Password != "" {
		auth = append(auth, ssh.Password(config.Password))
	} else if config.GSSAPIPassword != "" {
		c, err = NewKrb5InitiatorClientWithPassword(config.User, config.GSSAPIPassword, krb5Conf)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.GSSAPIWithMICAuthMethod(&c, host))
	} else if config.GSSAPIKeytabConf != "" {
		c, err = NewKrb5InitiatorClientWithKeytab(config.User, krb5Conf, config.GSSAPIKeytabConf)
		if err != nil {
			return nil, err
		}
	} else if config.GSSAPICacheFile != "" {
		c, err = NewKrb5InitiatorClientWithCache(krb5Conf, config.GSSAPICacheFile)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.GSSAPIWithMICAuthMethod(&c, host))
	} else {
		if config.Keyfile == "" {
			config.Keyfile = filepath.Join(homedir.HomeDir(), ".ssh", "id_rsa")
		}
		var keyFile ssh.AuthMethod
		keyFile, err = publicKeyFile(config.Keyfile)
		if err != nil {
			return nil, err
		}
		auth = append(auth, keyFile)
	}
	return auth, nil
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

func copyStream(ctx context.Context, local net.Conn, remote net.Conn) {
	chDone := make(chan bool, 2)

	// start remote -> local data transfer
	go func() {
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])
		_, err := io.CopyBuffer(local, remote, buf)
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
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])
		_, err := io.CopyBuffer(remote, local, buf)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Debugf("error while copy local->remote: %s", err)
		}
		select {
		case chDone <- true:
		default:
		}
	}()

	select {
	case <-chDone:
		return
	case <-ctx.Done():
		return
	}
}

func (config SshConfig) AliasRecursion(ctx context.Context) (client *ssh.Client, err error) {
	var name = config.ConfigAlias
	var jumper = "ProxyJump"
	var bastionList = []SshConfig{GetBastion(name, config)}
	for {
		value := confList.Get(name, jumper)
		if value != "" {
			bastionList = append(bastionList, GetBastion(value, config))
			name = value
			continue
		}
		break
	}
	for i := len(bastionList) - 1; i >= 0; i-- {
		if client == nil {
			client, err = bastionList[i].Dial(ctx)
			if err != nil {
				return
			}
		} else {
			client, err = JumpTo(ctx, client, bastionList[i])
			if err != nil {
				return
			}
		}
	}
	return
}

func (config SshConfig) JumpRecursion(ctx context.Context) (client *ssh.Client, err error) {
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	var sshConf = &SshConfig{}
	AddSshFlags(flags, sshConf)
	err = flags.Parse(strings.Split(config.Jump, " "))
	if err != nil {
		return nil, err
	}
	var baseClient *ssh.Client
	baseClient, err = DialSshRemote(ctx, sshConf)
	if err != nil {
		return nil, err
	}

	var bastionList []SshConfig
	if config.ConfigAlias != "" {
		var name = config.ConfigAlias
		var jumper = "ProxyJump"
		bastionList = append(bastionList, GetBastion(name, config))
		for {
			value := confList.Get(name, jumper)
			if value != "" {
				bastionList = append(bastionList, GetBastion(value, config))
				name = value
				continue
			}
			break
		}
	}
	if config.Addr != "" {
		bastionList = append(bastionList, config)
	}

	for _, sshConfig := range bastionList {
		client, err = JumpTo(ctx, baseClient, sshConfig)
		if err != nil {
			return
		}
	}
	return
}

func GetBastion(name string, defaultValue SshConfig) SshConfig {
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
			if value == "" {
				config.Keyfile = defaultValue.Keyfile
				config.Password = defaultValue.Password
				config.GSSAPIKeytabConf = defaultValue.GSSAPIKeytabConf
				config.GSSAPIPassword = defaultValue.GSSAPIPassword
				config.GSSAPICacheFile = defaultValue.GSSAPICacheFile
			} else {
				config.Keyfile = value
			}
		}
	}
	config.Addr = net.JoinHostPort(host, port)
	return config
}

func (config SshConfig) Dial(ctx context.Context) (client *ssh.Client, err error) {
	if strings.Index(config.Addr, ":") < 0 {
		// use default ssh port 22
		config.Addr = net.JoinHostPort(config.Addr, "22")
	}
	// connect to the bastion host
	authMethod, err := config.GetAuth()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", config.Addr, time.Second*10)
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		conn.Close()
		if client != nil {
			client.Close()
		}
	}()
	c, chans, reqs, err := ssh.NewClientConn(conn, config.Addr, &ssh.ClientConfig{
		User:            config.User,
		Auth:            authMethod,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		BannerCallback:  ssh.BannerDisplayStderr(),
		Timeout:         time.Second * 10,
	})
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func JumpTo(ctx context.Context, bClient *ssh.Client, to SshConfig) (client *ssh.Client, err error) {
	if strings.Index(to.Addr, ":") < 0 {
		// use default ssh port 22
		to.Addr = net.JoinHostPort(to.Addr, "22")
	}

	var authMethod []ssh.AuthMethod
	authMethod, err = to.GetAuth()
	if err != nil {
		return nil, err
	}
	// Dial a connection to the service host, from the bastion
	var conn net.Conn
	conn, err = bClient.Dial("tcp", to.Addr)
	if err != nil {
		return
	}
	go func() {
		<-ctx.Done()
		conn.Close()
		if client != nil {
			client.Close()
		}
		bClient.Close()
	}()
	defer func() {
		if err != nil {
			if client != nil {
				client.Close()
			}
			if conn != nil {
				conn.Close()
			}
		}
	}()
	var ncc ssh.Conn
	var chans <-chan ssh.NewChannel
	var reqs <-chan *ssh.Request
	ncc, chans, reqs, err = ssh.NewClientConn(conn, to.Addr, &ssh.ClientConfig{
		User:            to.User,
		Auth:            authMethod,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		BannerCallback:  ssh.BannerDisplayStderr(),
		Timeout:         time.Second * 10,
	})
	if err != nil {
		return
	}

	client = ssh.NewClient(ncc, chans, reqs)
	return
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
	var sshClient *ssh.Client

	var getRemoteConnFunc = func() (net.Conn, error) {
		lock.Lock()
		defer lock.Unlock()

		if sshClient != nil {
			ctx1, cancelFunc := context.WithTimeout(ctx, time.Second*10)
			defer cancelFunc()
			remoteConn, err := sshClient.DialContext(ctx1, "tcp", remote.String())
			if err == nil {
				return remoteConn, nil
			}
			sshClient.Close()
			sshClient = nil
		}
		sshClient, err = DialSshRemote(ctx, conf)
		if err != nil {
			log.Errorf("failed to dial remote ssh server: %v", err)
			return nil, err
		}
		return sshClient.Dial("tcp", remote.String())
	}

	go func() {
		defer localListen.Close()
		go func() {
			<-ctx.Done()
			localListen.Close()
			if sshClient != nil {
				sshClient.Close()
			}
		}()

		for ctx.Err() == nil {
			localConn, err := localListen.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					log.Errorf("failed to accept conn: %v", err)
				}
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
				copyStream(ctx, localConn, remoteConn)
			}()
		}
	}()
	return nil
}
func SshJump(ctx context.Context, conf *SshConfig, flags *pflag.FlagSet, print bool) (path string, err error) {
	if conf.Addr == "" && conf.ConfigAlias == "" {
		if flags != nil {
			lookup := flags.Lookup("kubeconfig")
			if lookup != nil {
				if lookup.Value != nil && lookup.Value.String() != "" {
					path = lookup.Value.String()
				} else if lookup.DefValue != "" {
					path = lookup.DefValue
				} else {
					path = lookup.NoOptDefVal
				}
			}
		}
		return
	}
	defer func() {
		if er := recover(); er != nil {
			err = er.(error)
		}
	}()

	// pre-check network ip connect
	var cli *ssh.Client
	cli, err = DialSshRemote(ctx, conf)
	if err != nil {
		return
	}
	defer cli.Close()

	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()

	if conf.RemoteKubeconfig != "" || (flags != nil && flags.Changed("remote-kubeconfig")) {
		var stdout []byte
		var stderr []byte
		if len(conf.RemoteKubeconfig) != 0 && conf.RemoteKubeconfig[0] == '~' {
			conf.RemoteKubeconfig = filepath.Join("/home", conf.User, conf.RemoteKubeconfig[1:])
		}
		if conf.RemoteKubeconfig == "" {
			// if `--remote-kubeconfig` is parsed then Entrypoint is reset
			conf.RemoteKubeconfig = filepath.Join("/home", conf.User, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)
		}
		stdout, stderr, err = RemoteRun(cli,
			fmt.Sprintf("sh -c 'kubectl config view --flatten --raw --kubeconfig %s || minikube kubectl -- config view --flatten --raw --kubeconfig %s'",
				conf.RemoteKubeconfig,
				conf.RemoteKubeconfig),
			map[string]string{clientcmd.RecommendedConfigPathEnvVar: conf.RemoteKubeconfig},
		)
		if err != nil {
			err = errors.Wrap(err, string(stderr))
			return
		}
		if len(stdout) == 0 {
			err = errors.Errorf("can not get kubeconfig %s from remote ssh server: %s", conf.RemoteKubeconfig, string(stderr))
			return
		}

		var temp *os.File
		if temp, err = os.CreateTemp("", "*.kubeconfig"); err != nil {
			return
		}
		if err = temp.Close(); err != nil {
			return
		}
		if err = os.WriteFile(temp.Name(), stdout, 0644); err != nil {
			return
		}
		if err = os.Chmod(temp.Name(), 0644); err != nil {
			return
		}
		configFlags.KubeConfig = pointer.String(temp.Name())
	} else {
		if flags != nil {
			lookup := flags.Lookup("kubeconfig")
			if lookup != nil {
				if lookup.Value != nil && lookup.Value.String() != "" {
					configFlags.KubeConfig = pointer.String(lookup.Value.String())
				} else if lookup.DefValue != "" {
					configFlags.KubeConfig = pointer.String(lookup.DefValue)
				}
			}
		}
	}
	matchVersionFlags := util.NewMatchVersionFlags(configFlags)
	var rawConfig api.Config
	rawConfig, err = matchVersionFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return
	}
	if err = api.FlattenConfig(&rawConfig); err != nil {
		return
	}
	if rawConfig.Contexts == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	cluster := rawConfig.Clusters[kubeContext.Cluster]
	if cluster == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	var u *url.URL
	u, err = url.Parse(cluster.Server)
	if err != nil {
		return
	}

	serverHost := u.Hostname()
	serverPort := u.Port()
	if serverPort == "" {
		if u.Scheme == "https" {
			serverPort = "443"
		} else if u.Scheme == "http" {
			serverPort = "80"
		} else {
			// handle other schemes if necessary
			err = errors.New("kubeconfig is invalid: wrong protocol")
			return
		}
	}
	ips, err := net.LookupHost(serverHost)
	if err != nil {
		return
	}

	if len(ips) == 0 {
		// handle error: no IP associated with the hostname
		err = fmt.Errorf("kubeconfig: no IP associated with the hostname %s", serverHost)
		return
	}

	var remote netip.AddrPort
	// Use the first IP address
	remote, err = netip.ParseAddrPort(net.JoinHostPort(ips[0], serverPort))
	if err != nil {
		return
	}
	var port int
	port, err = GetAvailableTCPPortOrDie()
	if err != nil {
		return
	}
	var local netip.AddrPort
	local, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return
	}

	if print {
		log.Infof("wait jump to bastion host...")
	}
	err = PortMapUntil(ctx, conf, remote, local)
	if err != nil {
		log.Errorf("ssh proxy err: %v", err)
		return
	}

	rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].Server = fmt.Sprintf("%s://%s", u.Scheme, local.String())
	rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].TLSServerName = serverHost
	// To Do: add cli option to skip tls verify
	// rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].CertificateAuthorityData = nil
	// rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].InsecureSkipTLSVerify = true
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: latest.Version, Kind: "Config"})

	var convertedObj runtime.Object
	convertedObj, err = latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return
	}
	var marshal []byte
	marshal, err = json.Marshal(convertedObj)
	if err != nil {
		return
	}
	var temp *os.File
	temp, err = os.CreateTemp("", "*.kubeconfig")
	if err != nil {
		return
	}
	if err = temp.Close(); err != nil {
		return
	}
	if err = os.WriteFile(temp.Name(), marshal, 0644); err != nil {
		return
	}
	if err = os.Chmod(temp.Name(), 0644); err != nil {
		return
	}
	if print {
		msg := fmt.Sprintf("| To use: export KUBECONFIG=%s |", temp.Name())
		printLine(msg)
		log.Infof(msg)
		printLine(msg)
	}
	path = temp.Name()
	return
}
func printLine(msg string) {
	line := "+" + strings.Repeat("-", len(msg)-2) + "+"
	log.Infof(line)
}

func SshJumpAndSetEnv(ctx context.Context, conf *SshConfig, flags *pflag.FlagSet, print bool) error {
	if conf.Addr == "" && conf.ConfigAlias == "" {
		return nil
	}
	path, err := SshJump(ctx, conf, flags, print)
	if err != nil {
		return err
	}
	err = os.Setenv(clientcmd.RecommendedConfigPathEnvVar, path)
	if err != nil {
		return err
	}
	return os.Setenv(config.EnvSSHJump, path)
}
