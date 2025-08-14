package ssh

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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	gossh "golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgutil "github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// DialSshRemote https://github.com/golang/go/issues/21478
func DialSshRemote(ctx context.Context, conf *SshConfig, stopChan <-chan struct{}) (remote *gossh.Client, err error) {
	defer func() {
		if err != nil {
			if remote != nil {
				remote.Close()
			}
		}
	}()

	if conf.ConfigAlias != "" {
		remote, err = conf.AliasRecursion(ctx, stopChan)
	} else if conf.Jump != "" {
		remote, err = conf.JumpRecursion(ctx, stopChan)
	} else {
		remote, err = conf.Dial(ctx, stopChan)
	}

	// ref: https://github.com/golang/go/issues/21478
	if err == nil {
		//go func() {
		//	err2 := keepAlive(remote, conn, ctx.Done())
		//	if err2 != nil {
		//		plog.G(ctx).Debugf("Failed to send keep-alive request: %v", err2)
		//	}
		//}()
	}
	return remote, err
}

func RemoteRun(client *gossh.Client, cmd string, env map[string]string) (output []byte, errOut []byte, err error) {
	var session *gossh.Session
	session, err = client.NewSession()
	if err != nil {
		return
	}
	defer session.Close()
	for k, v := range env {
		// /etc/ssh/sshd_config
		// AcceptEnv DEBIAN_FRONTEND
		_ = session.Setenv(k, v)
	}
	var out bytes.Buffer
	var er bytes.Buffer
	session.Stdout = &out
	session.Stderr = &er
	err = session.Run(cmd)
	return out.Bytes(), er.Bytes(), err
}

func PortMapUntil(ctx context.Context, conf *SshConfig, remote, local netip.AddrPort) error {
	// Listen on remote server port
	var lc net.ListenConfig
	localListen, e := lc.Listen(ctx, "tcp", local.String())
	if e != nil {
		plog.G(ctx).Errorf("failed to listen %s: %v", local.String(), e)
		return e
	}
	plog.G(ctx).Debugf("SSH listening on local %s forward to %s", local.String(), remote.String())

	go func() {
		<-ctx.Done()
		localListen.Close()
	}()

	go func() {
		defer localListen.Close()

		var clientMap = &sync.Map{}
		ctx1, cancelFunc1 := context.WithCancel(ctx)
		defer cancelFunc1()

		for ctx1.Err() == nil {
			localConn, err1 := localListen.Accept()
			if err1 != nil {
				if errors.Is(err1, net.ErrClosed) {
					return
				}
				plog.G(ctx).Debugf("Failed to accept ssh conn: %v", err1)
				continue
			}
			plog.G(ctx).Debugf("Accepted ssh conn from %s", localConn.RemoteAddr().String())
			go func() {
				defer localConn.Close()

				remoteConn, err := getRemoteConn(ctx1, clientMap, conf, remote)
				if err != nil {
					var openChannelError *gossh.OpenChannelError
					// if ssh server not permitted ssh port-forward, do nothing until exit
					if errors.As(err, &openChannelError) && openChannelError.Reason == gossh.Prohibited {
						plog.G(ctx).Debugf("Failed to open ssh port-forward to %s: %v", remote.String(), err)
						plog.G(ctx).Errorf("Failed to open ssh port-forward to %s: %v", remote.String(), err)
						cancelFunc1()
					}
					plog.G(ctx).Debugf("Failed to dial into remote %s: %v", remote.String(), err)
					return
				}
				plog.G(ctx).Debugf("Opened ssh port-forward to %s", remote.String())

				defer remoteConn.Close()
				copyStream(ctx, localConn, remoteConn)
			}()
		}
	}()
	return nil
}

func SshJump(ctx context.Context, conf *SshConfig, kubeconfigBytes []byte, print bool) (path string, err error) {
	if len(conf.RemoteKubeconfig) != 0 {
		var stdout []byte
		var stderr []byte
		// pre-check network ip connect
		var cli *gossh.Client
		cli, err = DialSshRemote(ctx, conf, ctx.Done())
		if err != nil {
			return
		}
		defer cli.Close()
		stdout, stderr, err = RemoteRun(cli,
			fmt.Sprintf("sh -c 'kubectl config view --flatten --raw --kubeconfig %s || minikube kubectl -- config view --flatten --raw --kubeconfig %s || cat %s'",
				conf.RemoteKubeconfig,
				conf.RemoteKubeconfig,
				conf.RemoteKubeconfig,
			),
			map[string]string{clientcmd.RecommendedConfigPathEnvVar: conf.RemoteKubeconfig},
		)
		if err != nil {
			err = errors.Wrap(err, string(stderr))
			return
		}
		if len(bytes.TrimSpace(stdout)) == 0 {
			err = errors.Errorf("can not get kubeconfig %s from remote ssh server: %s", conf.RemoteKubeconfig, string(stderr))
			return
		}
		kubeconfigBytes = bytes.TrimSpace(stdout)
	}
	var clientConfig clientcmd.ClientConfig
	clientConfig, err = clientcmd.NewClientConfigFromBytes(kubeconfigBytes)
	if err != nil {
		return
	}
	var rawConfig api.Config
	rawConfig, err = clientConfig.RawConfig()
	if err != nil {
		plog.G(ctx).WithError(err).Errorf("failed to build config: %v", err)
		return
	}
	if err = api.FlattenConfig(&rawConfig); err != nil {
		plog.G(ctx).Errorf("failed to flatten config: %v", err)
		return
	}
	if rawConfig.Contexts == nil {
		err = errors.New("kubeconfig is invalid")
		plog.G(ctx).Error("can not get contexts")
		return
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		err = errors.New("kubeconfig is invalid")
		plog.G(ctx).Errorf("can not find kubeconfig context %s", rawConfig.CurrentContext)
		return
	}
	cluster := rawConfig.Clusters[kubeContext.Cluster]
	if cluster == nil {
		err = errors.New("kubeconfig is invalid")
		plog.G(ctx).Errorf("can not find cluster %s", kubeContext.Cluster)
		return
	}
	var u *url.URL
	u, err = url.Parse(cluster.Server)
	if err != nil {
		plog.G(ctx).Errorf("failed to parse cluster url: %v", err)
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
			plog.G(ctx).Error(err)
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
		plog.G(ctx).Error(err)
		return
	}

	var remote netip.AddrPort
	// Use the first IP address
	remote, err = netip.ParseAddrPort(net.JoinHostPort(ips[0], serverPort))
	if err != nil {
		return
	}
	var port int
	port, err = pkgutil.GetAvailableTCPPortOrDie()
	if err != nil {
		return
	}
	var local netip.AddrPort
	local, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return
	}

	if print {
		plog.G(ctx).Infof("Waiting jump to bastion host...")
		plog.G(ctx).Infof("Jump ssh bastion host to apiserver: %s", cluster.Server)
	} else {
		plog.G(ctx).Debugf("Waiting jump to bastion host...")
		plog.G(ctx).Debugf("Jump ssh bastion host to apiserver: %s", cluster.Server)
	}
	err = PortMapUntil(ctx, conf, remote, local)
	if err != nil {
		plog.G(ctx).Errorf("SSH port map error: %v", err)
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
		plog.G(ctx).Errorf("failed to build config: %v", err)
		return
	}
	var marshal []byte
	marshal, err = json.Marshal(convertedObj)
	if err != nil {
		plog.G(ctx).Errorf("failed to marshal config: %v", err)
		return
	}
	path, err = pkgutil.ConvertToTempKubeconfigFile(marshal, GenKubeconfigTempPath(conf, kubeconfigBytes))
	if err != nil {
		plog.G(ctx).Errorf("failed to write kubeconfig: %v", err)
		return
	}
	go func() {
		<-ctx.Done()
		_ = os.Remove(path)
	}()
	if print {
		plog.G(ctx).Infof("Use temp kubeconfig: %s", path)
	} else {
		plog.G(ctx).Debugf("Use temp kubeconfig: %s", path)
	}
	return
}

func SshJumpAndSetEnv(ctx context.Context, sshConf *SshConfig, kubeconfigBytes []byte, print bool) error {
	path, err := SshJump(ctx, sshConf, kubeconfigBytes, print)
	if err != nil {
		return err
	}
	err = os.Setenv(clientcmd.RecommendedConfigPathEnvVar, path)
	if err != nil {
		return err
	}
	return os.Setenv(config.EnvSSHJump, path)
}

func JumpTo(ctx context.Context, bClient *gossh.Client, to SshConfig, stopChan <-chan struct{}) (client *gossh.Client, err error) {
	if _, _, err = net.SplitHostPort(to.Addr); err != nil {
		// use default ssh port 22
		to.Addr = net.JoinHostPort(to.Addr, "22")
		err = nil
	}

	var authMethod []gossh.AuthMethod
	authMethod, err = to.GetAuth()
	if err != nil {
		return nil, err
	}
	// Dial a connection to the service host, from the bastion
	var conn net.Conn
	conn, err = bClient.DialContext(ctx, "tcp", to.Addr)
	if err != nil {
		return
	}
	go func() {
		if stopChan != nil {
			<-stopChan
			conn.Close()
			if client != nil {
				client.Close()
			}
			bClient.Close()
		}
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
	var ncc gossh.Conn
	var chans <-chan gossh.NewChannel
	var reqs <-chan *gossh.Request
	ncc, chans, reqs, err = gossh.NewClientConn(conn, to.Addr, &gossh.ClientConfig{
		User:            to.User,
		Auth:            authMethod,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		//BannerCallback:  ssh.BannerDisplayStderr(),
		Timeout: time.Second * 10,
	})
	if err != nil {
		return
	}

	client = gossh.NewClient(ncc, chans, reqs)
	return
}

func getRemoteConn(ctx context.Context, clientMap *sync.Map, conf *SshConfig, remote netip.AddrPort) (net.Conn, error) {
	var conn net.Conn
	var err error
	clientMap.Range(func(key, value any) bool {
		cli := value.(*sshClientWrap)
		ctx1, cancelFunc1 := context.WithTimeout(ctx, time.Second*10)
		conn, err = cli.DialContext(ctx1, "tcp", remote.String())
		cancelFunc1()
		if err != nil {
			plog.G(ctx).Debugf("Failed to dial remote address %s: %v", remote.String(), err)
			clientMap.Delete(key)
			plog.G(ctx).Error("Delete invalid ssh client from map")
			_ = cli.Close()
			return true
		}
		return false
	})
	if conn != nil {
		return conn, nil
	}

	ctx1, cancelFunc1 := context.WithCancel(ctx)
	var client *gossh.Client
	client, err = DialSshRemote(ctx1, conf, ctx1.Done())
	if err != nil {
		plog.G(ctx).Debugf("Failed to dial remote ssh server: %v", err)
		cancelFunc1()
		return nil, err
	}
	clientMap.Store(uuid.NewString(), newSshClientWrap(client, cancelFunc1))
	plog.G(ctx1).Debug("Connected to remote ssh server")

	ctx2, cancelFunc2 := context.WithTimeout(ctx, time.Second*10)
	defer cancelFunc2()
	conn, err = client.DialContext(ctx2, "tcp", remote.String())
	if err != nil {
		plog.G(ctx).Debugf("Failed to dial remote addr %s: %v", remote.String(), err)
		return nil, err
	}
	plog.G(ctx).Debugf("Connected to remote addr %s", remote.String())
	return conn, nil
}

func copyStream(ctx context.Context, local net.Conn, remote net.Conn) {
	chDone := make(chan bool, 2)

	// start remote -> local data transfer
	go func() {
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])
		_, err := io.CopyBuffer(local, remote, buf)
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			plog.G(ctx).Errorf("Failed to copy remote -> local: %s", err)
		}
		chDone <- true
	}()

	// start local -> remote data transfer
	go func() {
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])
		_, err := io.CopyBuffer(remote, local, buf)
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			plog.G(ctx).Errorf("Failed to copy local -> remote: %s", err)
		}
		chDone <- true
	}()

	select {
	case <-chDone:
		return
	case <-ctx.Done():
		return
	}
}

func GenKubeconfigTempPath(conf *SshConfig, kubeconfigBytes []byte) string {
	if conf != nil && conf.RemoteKubeconfig != "" {
		return filepath.Join(config.GetTempPath(), fmt.Sprintf("%s_%d", conf.GenKubeconfigIdentify(), time.Now().Unix()))
	}

	return pkgutil.GenKubeconfigTempPath(kubeconfigBytes)
}
