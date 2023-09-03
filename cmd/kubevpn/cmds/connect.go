package cmds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	defaultlog "log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdConnect(f cmdutil.Factory) *cobra.Command {
	var connect = &handler.ConnectOptions{}
	var sshConf = &util.SshConfig{}
	var transferImage bool
	cmd := &cobra.Command{
		Use:   "connect",
		Short: i18n.T("Connect to kubernetes cluster network"),
		Long:  templates.LongDesc(i18n.T(`Connect to kubernetes cluster network`)),
		Example: templates.Examples(i18n.T(`
		# Connect to k8s cluster network
		kubevpn connect

		# Connect to api-server behind of bastion host or ssh jump host
		kubevpn connect --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn connect --ssh-alias <alias>

`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// startup daemon process and sudo process
			err = startupDaemon(cmd.Context())
			if err != nil {
				return err
			}
			go util.StartupPProf(config.PProfPort)
			util.InitLogger(config.Debug)
			defaultlog.Default().SetOutput(io.Discard)
			runtime.GOMAXPROCS(0)
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, err2 := ConvertToKubeconfigBytes(f)
			if err2 != nil {
				return err2
			}
			client, err := daemon.GetClient(true).Connect(
				cmd.Context(),
				&rpc.ConnectRequest{
					KubeconfigBytes:  string(bytes),
					Namespace:        connect.Namespace,
					Headers:          connect.Headers,
					Workloads:        connect.Workloads,
					ExtraCIDR:        connect.ExtraCIDR,
					ExtraDomain:      connect.ExtraDomain,
					UseLocalDNS:      connect.UseLocalDNS,
					Engine:           string(connect.Engine),
					Addr:             sshConf.Addr,
					User:             sshConf.User,
					Password:         sshConf.Password,
					Keyfile:          sshConf.Keyfile,
					ConfigAlias:      sshConf.ConfigAlias,
					RemoteKubeconfig: sshConf.RemoteKubeconfig,
					TransferImage:    transferImage,
					Image:            config.Image,
					Level:            int32(log.DebugLevel),
				},
			)
			if err != nil {
				return err
			}
			var resp *rpc.ConnectResponse
			for {
				resp, err = client.Recv()
				if err == io.EOF {
					break
				} else if err == nil {
					log.Println(resp.Message)
				} else {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	cmd.Flags().StringArrayVar(&connect.ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	cmd.Flags().StringArrayVar(&connect.ExtraDomain, "extra-domain", []string{}, "Extra domain string, the resolved ip will add to route table, eg: --extra-domain test.abc.com --extra-domain foo.test.com")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
	cmd.Flags().BoolVar(&connect.UseLocalDNS, "use-localdns", false, "if use-lcoaldns is true, kubevpn will start coredns listen at 53 to forward your dns queries. only support on linux now")
	cmd.Flags().StringVar((*string)(&connect.Engine), "engine", string(config.EngineRaw), fmt.Sprintf(`transport engine ("%s"|"%s") %s: use gvisor and raw both (both performance and stable), %s: use raw mode (best stable)`, config.EngineMix, config.EngineRaw, config.EngineMix, config.EngineRaw))

	addSshFlags(cmd, sshConf)
	return cmd
}

func startupDaemon(ctx context.Context) (err error) {
	// normal
	if daemon.GetClient(false) == nil {
		if err = runDaemon(ctx); err != nil {
			return err
		}
	}

	// sudo
	if client := daemon.GetClient(true); client == nil {
		if err = runSudoDaemon(ctx); err != nil {
			return err
		}
	}
	return nil
}

func runSudoDaemon(ctx context.Context) error {
	if !util.IsAdmin() {
		util.RunWithElevated()
		os.Exit(0)
	}

	port := daemon.GetPortPath(true)
	err := os.Remove(port)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	path := daemon.GetPidPath(true)
	if file, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(string(file)); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				if err = p.Kill(); err != nil && err != os.ErrProcessDone {
					log.Error(err)
				}
			}
		}
	}

	log.Info(os.Args[0])
	cmd := exec.CommandContext(ctx, "sudo", "--preserve-env", os.Args[0], "daemon", "--sudo")
	cmd.SysProcAttr = &unix.SysProcAttr{
		Setpgid: true,
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return err
	}
	err = os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)), os.ModePerm)
	if err != nil {
		return err
	}
	err = os.Chmod(daemon.GetPidPath(true), os.ModePerm)
	if err != nil {
		return err
	}
	go func() {
		cmd.Wait()
	}()
	for ctx.Err() == nil {
		println("wait")
		time.Sleep(time.Millisecond * 50)
		if _, err = os.Stat(port); err == nil {
			break
		}
	}
	return err
}

func runDaemon(ctx context.Context) error {
	path := daemon.GetPortPath(false)
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pidPath := daemon.GetPidPath(false)
	if file, err := os.ReadFile(pidPath); err == nil {
		if pid, err := strconv.Atoi(string(file)); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				if err = p.Kill(); err != nil && err != os.ErrProcessDone {
					log.Error(err)
				}
			}
		}
	}
	fmt.Println(os.Args[0])
	cmd := exec.CommandContext(ctx, os.Args[0], "daemon")
	log.Info(cmd.Args)
	cmd.SysProcAttr = &unix.SysProcAttr{
		Setpgid: true,
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return err
	}
	err = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), os.ModePerm)
	if err != nil {
		return err
	}
	err = os.Chmod(daemon.GetPidPath(false), os.ModePerm)
	if err != nil {
		return err
	}
	go func() {
		cmd.Wait()
	}()

	for ctx.Err() == nil {
		time.Sleep(time.Millisecond * 50)
		if _, err = os.Stat(path); err == nil {
			break
		}
	}
	return err
}

func ConvertToKubeconfigBytes(factory cmdutil.Factory) ([]byte, error) {
	rawConfig, err := factory.ToRawKubeConfigLoader().RawConfig()
	convertedObj, err := latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return nil, err
	}
	return json.Marshal(convertedObj)
}
