package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	gossh "golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgutil "github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// sshOpTimeout bounds SSH dial, handshake, and per-operation context timeouts.
const sshOpTimeout = 10 * time.Second

// DialSshRemote establishes an SSH client connection, resolving aliases and jump hosts recursively.
func DialSshRemote(ctx context.Context, conf *SshConfig, stopChan <-chan struct{}) (client *gossh.Client, err error) {
	defer func() {
		if err != nil && client != nil {
			client.Close()
		}
	}()

	if conf.ConfigAlias != "" {
		return conf.AliasRecursion(ctx, stopChan)
	}
	if conf.Jump != "" {
		return conf.JumpRecursion(ctx, stopChan)
	}
	return conf.Dial(ctx, stopChan)
}

// RemoteRun executes a command on the remote SSH server and returns stdout and stderr output.
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

// PortMapUntil sets up local TCP port forwarding through an SSH tunnel, forwarding local connections to the remote address.
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

		clientMap := &sync.Map{}
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
			go func() {
				defer localConn.Close()

				remoteConn, err := getRemoteConn(ctx1, clientMap, conf, remote)
				if err != nil {
					var openChannelError *gossh.OpenChannelError
					// if ssh server not permitted ssh port-forward, do nothing until exit
					if errors.As(err, &openChannelError) && openChannelError.Reason == gossh.Prohibited {
						plog.G(ctx).Errorf("Prohibited to open ssh port-forward to %s: %v", remote.String(), err)
						cancelFunc1()
						return
					}
					plog.G(ctx).Debugf("Failed to dial remote from %s<=>%s -> %s: %v", localConn.LocalAddr().String(), localConn.RemoteAddr().String(), remote.String(), err)
					return
				}
				plog.G(ctx).Debugf("Opened ssh port-forward to %s<=>%s -> %s", localConn.LocalAddr().String(), localConn.RemoteAddr().String(), remote.String())

				defer remoteConn.Close()
				copyStream(ctx, localConn, remoteConn)
			}()
		}
	}()
	return nil
}

// SshJumpBytes establishes an SSH tunnel to the Kubernetes API server and returns
// the rewritten kubeconfig bytes pointing at the local tunnel endpoint, WITHOUT
// materializing a temp file. Callers that only need an in-process kubectl Factory
// should consume these bytes directly (via util.InitFactoryByBytes); use SshJump
// when a real file is required (child process, container mount, or KUBECONFIG env
// var). The tunnel stays up for the lifetime of ctx.
func SshJumpBytes(ctx context.Context, conf *SshConfig, kubeconfigBytes []byte, print bool) (newKubeconfigBytes []byte, err error) {
	if len(conf.RemoteKubeconfig) != 0 {
		var stdout []byte
		var stderr []byte
		// pre-check network IP connect
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
			err = fmt.Errorf("%s: %w: %w", string(stderr), err, config.ErrSSHRemoteCommand)
			return
		}
		if len(bytes.TrimSpace(stdout)) == 0 {
			err = fmt.Errorf("cannot get kubeconfig %s from remote ssh server: %s: %w", conf.RemoteKubeconfig, string(stderr), config.ErrSSHRemoteCommand)
			return
		}
		kubeconfigBytes = bytes.TrimSpace(stdout)
	}
	var port int
	port, err = pkgutil.GetAvailableTCPPort()
	if err != nil {
		return
	}
	var local netip.AddrPort
	local, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return
	}
	var oldAPIServer netip.AddrPort
	newKubeconfigBytes, oldAPIServer, err = pkgutil.ModifyAPIServer(ctx, kubeconfigBytes, local)
	if err != nil {
		return
	}

	if print {
		plog.G(ctx).Infof("Waiting jump to bastion host...")
		plog.G(ctx).Infof("Jump ssh bastion host to apiserver: %s", oldAPIServer.String())
	} else {
		plog.G(ctx).Debugf("Waiting jump to bastion host...")
		plog.G(ctx).Debugf("Jump ssh bastion host to apiserver: %s", oldAPIServer.String())
	}

	err = PortMapUntil(ctx, conf, oldAPIServer, local)
	if err != nil {
		plog.G(ctx).Errorf("SSH port map error: %v", err)
		return
	}
	return newKubeconfigBytes, nil
}

// SshJump establishes an SSH tunnel to the Kubernetes API server and returns the
// path to a rewritten kubeconfig file pointing at the local tunnel endpoint. The
// file is removed when ctx is done. Prefer SshJumpBytes unless a real file is
// required (child process, container mount, or KUBECONFIG env var).
func SshJump(ctx context.Context, conf *SshConfig, kubeconfigBytes []byte, print bool) (path string, err error) {
	newKubeconfigBytes, err := SshJumpBytes(ctx, conf, kubeconfigBytes, print)
	if err != nil {
		return "", err
	}
	path, err = pkgutil.ConvertToTempKubeconfigFile(newKubeconfigBytes, GenKubeconfigTempPath(conf, kubeconfigBytes))
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

// SshJumpAndSetEnv performs SshJump and sets the KUBECONFIG and SSH jump environment variables to the resulting path.
func SshJumpAndSetEnv(ctx context.Context, sshConf *SshConfig, kubeconfigBytes []byte, print bool) error {
	path, err := SshJump(ctx, sshConf, kubeconfigBytes, print)
	if err != nil {
		return err
	}
	if err = os.Setenv(clientcmd.RecommendedConfigPathEnvVar, path); err != nil {
		return err
	}
	return os.Setenv(config.EnvSSHJump, path)
}

// JumpTo dials through an existing SSH client (bastion) to establish a new SSH connection to the target host.
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
		err = fmt.Errorf("dial %s via bastion: %w: %w", to.Addr, err, config.ErrSSHConnect)
		return
	}
	go func() {
		if stopChan != nil {
			<-stopChan
			// Closing conn/bClient tears down both an in-progress handshake and an
			// established client. Avoid reading the named return `client` here — it
			// races with the function's return assignment.
			_ = conn.Close()
			_ = bClient.Close()
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
		Timeout: sshOpTimeout,
	})
	if err != nil {
		err = wrapDialError(err)
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
		ctx1, cancelFunc1 := context.WithTimeout(ctx, sshOpTimeout)
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
	key := uuid.NewString()
	wrap := newSshClientWrap(client, cancelFunc1)
	clientMap.Store(key, wrap)
	go func() {
		<-ctx.Done()
		if val, loaded := clientMap.LoadAndDelete(key); loaded {
			val.(*sshClientWrap).Close()
		}
	}()
	plog.G(ctx1).Debug("Connected to remote ssh server")

	ctx2, cancelFunc2 := context.WithTimeout(ctx, sshOpTimeout)
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
	done := make(chan struct{}, 2)

	copy := func(dst, src net.Conn, direction string) {
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])
		_, err := io.CopyBuffer(dst, src, buf)
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			plog.G(ctx).Debugf("Copy %s error: %s", direction, err)
		}
		done <- struct{}{}
	}

	go copy(local, remote, "remote->local")
	go copy(remote, local, "local->remote")

	// Wait for either direction to finish or context cancel, then close both
	// connections to unblock the other goroutine.
	select {
	case <-done:
	case <-ctx.Done():
	}
	local.Close()
	remote.Close()
	// Wait for the second goroutine to finish
	<-done
}

// GenKubeconfigTempPath generates a temp file path for kubeconfig based on the SSH config identifier or kubeconfig content.
func GenKubeconfigTempPath(conf *SshConfig, kubeconfigBytes []byte) string {
	if conf != nil && conf.RemoteKubeconfig != "" {
		return filepath.Join(config.GetTempPath(), fmt.Sprintf("%s_%d", conf.KubeconfigIdentifier(), time.Now().Unix()))
	}

	return pkgutil.GenKubeconfigTempPath(kubeconfigBytes)
}
