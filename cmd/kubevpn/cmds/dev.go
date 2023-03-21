package cmds

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"github.com/docker/cli/cli"
	"github.com/docker/cli/opts"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/dev"
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
		Aliases:    opts.NewListOpts(nil),
		NoProxy:    false,
		ExtraCIDR:  []string{},
	}
	var sshConf = &util.SshConfig{}
	cmd := &cobra.Command{
		Use:   "dev",
		Short: i18n.T("Startup your kubernetes workloads in local Docker container with same volume、env、and network"),
		Long: templates.LongDesc(i18n.T(`
Startup your kubernetes workloads in local Docker container with same volume、env、and network

## What did i do:
- Download volume which MountPath point to, mount to docker container
- Connect to cluster network, set network to docker container
- Get all environment with command (env), set env to docker container
`)),
		Example: templates.Examples(i18n.T(`
        # Develop workloads
		- develop deployment
		  kubevpn dev deployment/productpage

		- develop service
		  kubevpn dev service/productpage

		# Develop workloads with mesh, traffic with header a=1, will hit local PC, otherwise no effect
		kubevpn dev service/productpage --headers a=1

        # Develop workloads without proxy traffic
		kubevpn dev service/productpage --no-proxy

		# Develop workloads which api-server behind of bastion host or ssh jump host
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
			go util.StartupPProf(config.PProfPort)
			util.InitLogger(config.Debug)
			return handler.SshJump(sshConf, cmd.Flags())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			connect := handler.ConnectOptions{
				Headers:     devOptions.Headers,
				Workloads:   args,
				ExtraCIDR:   devOptions.ExtraCIDR,
				ExtraDomain: devOptions.ExtraDomain,
			}

			mode := container.NetworkMode(devOptions.NetMode.NetworkMode())
			if mode.IsContainer() {
				client, _, err := dev.GetClient()
				if err != nil {
					return err
				}
				var inspect types.ContainerJSON
				inspect, err = client.ContainerInspect(context.Background(), mode.ConnectedContainer())
				if err != nil {
					return err
				}
				if inspect.State == nil {
					return fmt.Errorf("can not get container status, please make contianer name is valid")
				}
				if !inspect.State.Running {
					return fmt.Errorf("container %s status is %s, expect is running, please make sure your outer docker name is correct", mode.ConnectedContainer(), inspect.State.Status)
				}
			}

			if err := connect.InitClient(f); err != nil {
				return err
			}
			err := connect.PreCheckResource()
			if err != nil {
				return err
			}

			if len(connect.Workloads) > 1 {
				return fmt.Errorf("can only dev one workloads at same time, workloads: %v", connect.Workloads)
			}
			if len(connect.Workloads) < 1 {
				return fmt.Errorf("you must provide resource to dev, workloads : %v is invaild", connect.Workloads)
			}

			devOptions.Workload = connect.Workloads[0]
			// if no-proxy is true, not needs to intercept traffic
			if devOptions.NoProxy {
				if len(connect.Headers) != 0 {
					return fmt.Errorf("not needs to provide headers if is no-proxy mode")
				}
				connect.Workloads = []string{}
			}
			defer func() {
				handler.Cleanup(syscall.SIGQUIT)
				select {}
			}()
			if err = connect.DoConnect(); err != nil {
				log.Errorln(err)
				return err
			}
			devOptions.Namespace = connect.Namespace
			err = devOptions.Main(context.Background())
			if err != nil {
				log.Errorln(err)
			}
			return err
		},
	}
	cmd.Flags().StringToStringVarP(&devOptions.Headers, "headers", "H", map[string]string{}, "Traffic with special headers with reverse it to local PC, you should startup your service after reverse workloads successfully, If not special, redirect all traffic to local PC, format is k=v, like: k1=v1,k2=v2")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	cmd.Flags().BoolVar(&devOptions.NoProxy, "no-proxy", false, "Whether proxy remote workloads traffic into local or not, true: just startup container on local without inject containers to intercept traffic, false: intercept traffic and forward to local")
	cmdutil.AddContainerVarFlags(cmd, &devOptions.ContainerName, devOptions.ContainerName)
	cmdutil.CheckErr(cmd.RegisterFlagCompletionFunc("container", completion.ContainerCompletionFunc(f)))
	cmd.Flags().StringArrayVar(&devOptions.ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	cmd.Flags().StringArrayVar(&devOptions.ExtraDomain, "extra-domain", []string{}, "Extra domain string, the resolved ip will add to route table, eg: --extra-domain test.abc.com --extra-domain foo.test.com")

	// docker options
	cmd.Flags().Var(&devOptions.ExtraHosts, "add-host", "Add a custom host-to-IP mapping (host:ip)")
	// We allow for both "--net" and "--network", although the latter is the recommended way.
	cmd.Flags().Var(&devOptions.NetMode, "net", "Connect a container to a network, eg: [default|bridge|host|none|container:$CONTAINER_ID]")
	cmd.Flags().Var(&devOptions.NetMode, "network", "Connect a container to a network")
	cmd.Flags().MarkHidden("net")
	// We allow for both "--net-alias" and "--network-alias", although the latter is the recommended way.
	cmd.Flags().Var(&devOptions.Aliases, "net-alias", "Add network-scoped alias for the container")
	cmd.Flags().Var(&devOptions.Aliases, "network-alias", "Add network-scoped alias for the container")
	cmd.Flags().MarkHidden("net-alias")
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

	addSshFlag(cmd, sshConf)
	return cmd
}
