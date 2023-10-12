package action

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func (svr *Server) ConnectFork(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectForkServer) error {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	out := io.MultiWriter(newWarp(resp), svr.LogFile)
	log.SetOutput(out)
	log.SetLevel(log.InfoLevel)
	if !svr.IsSudo {
		return fmt.Errorf("connect-fork should not send to sudo daemon server")
	}

	ctx := resp.Context()
	return fork(ctx, req, out)
}

func fork(ctx context.Context, req *rpc.ConnectRequest, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable error: %s", err.Error())
	}
	var args = []string{"connect-fork"}
	if req.SshJump != nil {
		if req.SshJump.Addr != "" {
			args = append(args, "--ssh-addr", req.SshJump.Addr)
		}
		if req.SshJump.User != "" {
			args = append(args, "--ssh-username", req.SshJump.User)
		}
		if req.SshJump.Password != "" {
			args = append(args, "--ssh-password", req.SshJump.Password)
		}
		if req.SshJump.Keyfile != "" {
			args = append(args, "--ssh-keyfile", req.SshJump.Keyfile)
		}
		if req.SshJump.ConfigAlias != "" { // alias in ~/.ssh/config
			args = append(args, "--ssh-alias", req.SshJump.ConfigAlias)
		}
		if req.SshJump.RemoteKubeconfig != "" { // remote path in ssh server
			args = append(args, "--remote-kubeconfig", req.SshJump.RemoteKubeconfig)
		}
	}
	if req.KubeconfigBytes != "" {
		var path string
		path, err = util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
		if err != nil {
			return err
		}
		args = append(args, "--kubeconfig", path)
	}
	if req.Namespace != "" {
		args = append(args, "-n", req.Namespace)
	}
	if req.Image != "" {
		args = append(args, "--image", req.Image)
	}
	if req.TransferImage {
		args = append(args, "--transfer-image")
	}
	for _, v := range req.ExtraCIDR {
		args = append(args, "--extra-cidr", v)
	}
	for _, v := range req.ExtraDomain {
		args = append(args, "--extra-domain", v)
	}

	env := os.Environ()
	envKeys := sets.New[string](config.EnvInboundPodTunIPv4, config.EnvInboundPodTunIPv6, config.EnvTunNameOrLUID)
	for i := 0; i < len(env); i++ {
		index := strings.Index(env[i], "=")
		envKey := env[i][:index]
		if envKeys.HasAny(envKey) {
			env = append(env[:i], env[i+1:]...)
			i--
			continue
		}
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("fork to exec connect error: %s", err.Error())
	}
	return nil
}
