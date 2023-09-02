package cmds

import (
	"context"
	"fmt"
	"io"
	defaultlog "log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdConnect2(f cmdutil.Factory) *cobra.Command {
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
				log.Fatal(err)
			}
			go util.StartupPProf(config.PProfPort)
			util.InitLogger(config.Debug)
			defaultlog.Default().SetOutput(io.Discard)
			runtime.GOMAXPROCS(0)
			return err
		},
		Run: func(cmd *cobra.Command, args []string) {
			client, err := daemon.GetClient(true).Connect(
				cmd.Context(),
				&rpc.ConnectRequest{
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
				},
			)
			if err != nil {
				log.Fatal(err)
			}
			for {
				resp, err := client.Recv()
				if err == io.EOF {
					break
				} else if err == nil {
					log.Println(resp.Message)
				} else {
					log.Error(err)
				}
			}
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
	if daemon.GetClient(false) == nil {
		err = runDaemon(ctx)
		if err != nil {
			return err
		}
	}
	_, err = daemon.GetClient(false).Status(ctx, &rpc.StatusRequest{})
	if err != nil {
		err = runDaemon(ctx)
		if err != nil {
			return err
		}
	}

	// sudo
	if client := daemon.GetClient(true); client == nil {
		err = runSudoDaemon(ctx)
		if err != nil {
			return err
		}
	}
	_, err = daemon.GetClient(true).Status(ctx, &rpc.StatusRequest{})
	if err != nil {
		err = runSudoDaemon(ctx)
		if err != nil {
			return err
		}
	}
	return err
}

func runSudoDaemon(ctx context.Context) error {
	if !util.IsAdmin() {
		util.RunWithElevated()
		os.Exit(0)
	}
	port := filepath.Join(config.DaemonPortPath, "sudo_daemon")
	cmd := exec.CommandContext(ctx, "sudo", "--preserve-env", "/usr/local/bin/kubevpn", "daemon")
	cmd.SysProcAttr = &unix.SysProcAttr{
		Setpgid: true,
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	os.Remove(port)
	err := cmd.Start()
	if err != nil {
		return err
	}
	go func() {
		cmd.Wait()
	}()
	for ctx.Err() == nil {
		time.Sleep(time.Millisecond * 50)
		_, err = os.Stat(port)
		if err == nil {
			break
		}
	}
	return err
}

func runDaemon(ctx context.Context) error {
	port := filepath.Join(config.DaemonPortPath, "daemon")
	os.Remove(port)
	cmd := exec.CommandContext(ctx, "kubevpn", "daemon")
	cmd.SysProcAttr = &unix.SysProcAttr{
		Setpgid: true,
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Start()
	if err != nil {
		return err
	}
	go func() {
		cmd.Wait()
	}()
	for ctx.Err() == nil {
		time.Sleep(time.Millisecond * 50)
		_, err = os.Stat(port)
		if err == nil {
			break
		}
	}
	return err
}
