package cmds

import (
	"os"

	"github.com/docker/cli/cli"
	"github.com/docker/cli/opts"
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
		NoProxy:    false,
		ExtraCIDR:  []string{},
	}
	var sshConf = &util.SshConfig{}
	var transferImage bool
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
			if transferImage {
				if err := dev.TransferImage(cmd.Context(), sshConf); err != nil {
					return err
				}
			}
			return handler.SshJump(sshConf, cmd.Flags())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return dev.DoDev(devOptions, args, f)
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
	cmd.Flags().StringVar((*string)(&devOptions.ConnectMode), "connect-mode", string(dev.ConnectModeHost), "Connect to kubernetes network in container or in host, eg: ["+string(dev.ConnectModeContainer)+"|"+string(dev.ConnectModeHost)+"]")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to "+config.Image)

	// docker options
	cmd.Flags().Var(&devOptions.ExtraHosts, "add-host", "Add a custom host-to-IP mapping (host:ip)")
	// We allow for both "--net" and "--network", although the latter is the recommended way.
	cmd.Flags().Var(&devOptions.NetMode, "net", "Connect a container to a network, eg: [default|bridge|host|none|container:$CONTAINER_ID]")
	cmd.Flags().Var(&devOptions.NetMode, "network", "Connect a container to a network")
	cmd.Flags().MarkHidden("net")
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

	addSshFlags(cmd, sshConf)
	return cmd
}
