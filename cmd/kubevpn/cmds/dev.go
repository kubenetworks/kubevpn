package cmds

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/docker/cli/cli"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/dev"
	"github.com/wencaiwulue/kubevpn/pkg/driver"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdDev(f cmdutil.Factory) *cobra.Command {
	var devOptions = dev.Options{
		Factory:    f,
		Entrypoint: "",
		Publish:    opts.NewListOpts(nil),
		Expose:     opts.NewListOpts(nil),
		Env:        opts.NewListOpts(nil),
		Volumes:    opts.NewListOpts(nil),
		ExtraHosts: opts.NewListOpts(nil),
	}
	var sshConf = util.SshConfig{}
	cmd := &cobra.Command{
		Use:   "dev",
		Short: i18n.T("Proxy kubernetes workloads inbound traffic into local PC and dev in docker container"),
		Long:  templates.LongDesc(i18n.T(`Proxy kubernetes workloads inbound traffic into local PC`)),
		Example: templates.Examples(i18n.T(`
        # Dev reverse proxy
		- reverse deployment
		  kubevpn dev deployment/productpage
		- reverse service
		  kubevpn dev service/productpage

		# Reverse proxy with mesh, traffic with header a=1, will hit local PC, otherwise no effect
		kubevpn dev service/productpage --headers a=1

		# Dev reverse proxy api-server behind of bastion host or ssh jump host
		kubevpn dev deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile /Users/naison/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn dev deployment/productpage --ssh-alias <alias>

`)),
		Args: cli.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if !util.IsAdmin() {
				util.RunWithElevated()
				os.Exit(0)
			}
			go http.ListenAndServe("localhost:6060", nil)
			util.InitLogger(config.Debug)
			if util.IsWindows() {
				driver.InstallWireGuardTunDriver()
			}
			return handler.SshJump(sshConf, cmd.Flags())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			devOptions.Workload = args[0]

			connect := handler.ConnectOptions{
				Headers:   devOptions.Headers,
				Workloads: []string{devOptions.Workload},
			}

			if devOptions.ParentContainer != "" {
				client, _, err := dev.GetClient()
				if err != nil {
					return err
				}
				var inspect types.ContainerJSON
				inspect, err = client.ContainerInspect(context.Background(), devOptions.ParentContainer)
				if err != nil {
					return err
				}
				if inspect.State == nil {
					return fmt.Errorf("can not get container status, please make contianer name is valid")
				}
				if !inspect.State.Running {
					return fmt.Errorf("container %s status is %s, expect is running, please make sure your outer docker name is correct", devOptions.ParentContainer, inspect.State.Status)
				}
			}

			if err := connect.InitClient(f); err != nil {
				return err
			}
			connect.PreCheckResource()
			defer func() {
				handler.Cleanup(syscall.SIGQUIT)
				select {}
			}()
			if err := connect.DoConnect(); err != nil {
				log.Errorln(err)
				return err
			}
			devOptions.Namespace = connect.Namespace
			err := devOptions.Main(context.Background())
			if err != nil {
				log.Errorln(err)
			}
			return err
		},
		PostRun: func(*cobra.Command, []string) {
			if util.IsWindows() {
				err := retry.OnError(retry.DefaultRetry, func(err error) bool {
					return err != nil
				}, func() error {
					return driver.UninstallWireGuardTunDriver()
				})
				if err != nil {
					var wd string
					wd, err = os.Getwd()
					if err != nil {
						return
					}
					filename := filepath.Join(wd, "wintun.dll")
					var temp *os.File
					if temp, err = os.CreateTemp("", ""); err != nil {
						return
					}
					if err = temp.Close(); err != nil {
						return
					}
					if err = os.Rename(filename, temp.Name()); err != nil {
						log.Debugln(err)
					}
				}
			}
		},
	}
	cmd.Flags().StringToStringVarP(&devOptions.Headers, "headers", "H", map[string]string{}, "Traffic with special headers with reverse it to local PC, you should startup your service after reverse workloads successfully, If not special, redirect all traffic to local PC, format is k=v, like: k1=v1,k2=v2")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	cmdutil.AddContainerVarFlags(cmd, &devOptions.ContainerName, devOptions.ContainerName)
	cmdutil.CheckErr(cmd.RegisterFlagCompletionFunc("container", completion.ContainerCompletionFunc(f)))

	// docker options
	cmd.Flags().Var(&devOptions.ExtraHosts, "add-host", "Add a custom host-to-IP mapping (host:ip)")
	cmd.Flags().StringVar(&devOptions.ParentContainer, "parent-container", "", "Parent container name if running in Docker (Docker in Docker)")
	cmd.Flags().VarP(&devOptions.Volumes, "volume", "v", "Bind mount a volume")
	cmd.Flags().Var(&devOptions.Mounts, "mount", "Attach a filesystem mount to the container")
	cmd.Flags().Var(&devOptions.Expose, "expose", "Expose a port or a range of ports")
	cmd.Flags().VarP(&devOptions.Publish, "publish", "p", "Publish a container's port(s) to the host")
	cmd.Flags().BoolVarP(&devOptions.PublishAll, "publish-all", "P", false, "Publish all exposed ports to random ports")
	cmd.Flags().VarP(&devOptions.Env, "env", "e", "Set environment variables")
	cmd.Flags().StringVar(&devOptions.Entrypoint, "entrypoint", "", "Overwrite the default ENTRYPOINT of the image")
	cmd.Flags().StringVar(&devOptions.DockerImage, "docker-image", "", "Overwrite the default K8s pod of the image")
	//cmd.Flags().StringVar(&devOptions.Pull, "pull", container.PullImageMissing, `Pull image before creating ("`+container.PullImageAlways+`"|"`+container.PullImageMissing+`"|"`+container.PullImageNever+`")`)
	cmd.Flags().StringVar(&devOptions.Platform, "platform", os.Getenv("DOCKER_DEFAULT_PLATFORM"), "Set platform if server is multi-platform capable")
	cmd.Flags().StringVar(&devOptions.VolumeDriver, "volume-driver", "", "Optional volume driver for the container")
	_ = cmd.Flags().SetAnnotation("platform", "version", []string{"1.32"})

	// for ssh jumper host
	cmd.Flags().StringVar(&sshConf.Addr, "ssh-addr", "", "Optional ssh jump server address to dial as <hostname>:<port>, eg: 127.0.0.1:22")
	cmd.Flags().StringVar(&sshConf.User, "ssh-username", "", "Optional username for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Password, "ssh-password", "", "Optional password for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Keyfile, "ssh-keyfile", "", "Optional file with private key for SSH authentication")
	cmd.Flags().StringVar(&sshConf.ConfigAlias, "ssh-alias", "", "Optional config alias with ~/.ssh/config for SSH authentication")
	return cmd
}
