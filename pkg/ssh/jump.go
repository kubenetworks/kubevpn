package ssh

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"

	gossh "golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgutil "github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

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
	// Empty path → ConvertToTempKubeconfigFile uses os.CreateTemp for an
	// atomically-unique name, so concurrent SSH jumps never collide on a
	// deterministic filename.
	path, err = pkgutil.ConvertToTempKubeconfigFile(newKubeconfigBytes, "")
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
