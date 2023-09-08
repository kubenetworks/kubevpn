package cmds

import (
	"fmt"
	"os"

	dockercli "github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	dockercomp "github.com/docker/cli/cli/command/completion"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/dev"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdDev(f cmdutil.Factory) *cobra.Command {
	cli, dockerCli, err := util.GetClient()
	if err != nil {
		panic(err)
	}
	var devOptions = &dev.Options{
		Factory:   f,
		NoProxy:   false,
		ExtraCIDR: []string{},
		Cli:       cli,
		DockerCli: dockerCli,
	}
	var sshConf = &util.SshConfig{}
	var transferImage bool
	cmd := &cobra.Command{
		Use:   "dev [OPTIONS] RESOURCE [COMMAND] [ARG...]",
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
		kubevpn dev --headers a=1 service/productpage

        # Develop workloads without proxy traffic
		kubevpn dev --no-proxy service/productpage

		# Develop workloads which api-server behind of bastion host or ssh jump host
		kubevpn dev --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem deployment/productpage

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn dev --ssh-alias <alias> deployment/productpage

`)),
		Args:                  dockercli.RequiresMinArgs(1),
		DisableFlagsInUseLine: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			err = daemon.StartupDaemon(cmd.Context())
			if err != nil {
				return err
			}
			// not support temporally
			if devOptions.Engine == config.EngineGvisor {
				return fmt.Errorf(`not support type engine: %s, support ("%s"|"%s")`, config.EngineGvisor, config.EngineMix, config.EngineRaw)
			}
			return handler.SshJump(sshConf, cmd.Flags())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			devOptions.Workload = args[0]
			if len(args) > 1 {
				devOptions.Copts.Args = args[1:]
			}
			return dev.DoDev(cmd.Context(), devOptions, cmd.Flags(), f, transferImage)
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringToStringVarP(&devOptions.Headers, "headers", "H", map[string]string{}, "Traffic with special headers with reverse it to local PC, you should startup your service after reverse workloads successfully, If not special, redirect all traffic to local PC, format is k=v, like: k1=v1,k2=v2")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	cmd.Flags().BoolVar(&devOptions.NoProxy, "no-proxy", false, "Whether proxy remote workloads traffic into local or not, true: just startup container on local without inject containers to intercept traffic, false: intercept traffic and forward to local")
	cmdutil.AddContainerVarFlags(cmd, &devOptions.ContainerName, devOptions.ContainerName)
	cmdutil.CheckErr(cmd.RegisterFlagCompletionFunc("container", completion.ContainerCompletionFunc(f)))
	cmd.Flags().StringArrayVar(&devOptions.ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	cmd.Flags().StringArrayVar(&devOptions.ExtraDomain, "extra-domain", []string{}, "Extra domain string, the resolved ip will add to route table, eg: --extra-domain test.abc.com --extra-domain foo.test.com")
	cmd.Flags().StringVar((*string)(&devOptions.ConnectMode), "connect-mode", string(dev.ConnectModeHost), "Connect to kubernetes network in container or in host, eg: ["+string(dev.ConnectModeContainer)+"|"+string(dev.ConnectModeHost)+"]")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
	cmd.Flags().StringVar((*string)(&devOptions.Engine), "engine", string(config.EngineRaw), fmt.Sprintf(`transport engine ("%s"|"%s") %s: use gvisor and raw both (both performance and stable), %s: use raw mode (best stable)`, config.EngineMix, config.EngineRaw, config.EngineMix, config.EngineRaw))

	// diy docker options
	cmd.Flags().StringVar(&devOptions.DockerImage, "docker-image", "", "Overwrite the default K8s pod of the image")
	// origin docker options
	flags := cmd.Flags()
	flags.SetInterspersed(false)

	// These are flags not stored in Config/HostConfig
	flags.BoolVarP(&devOptions.Options.Detach, "detach", "d", false, "Run container in background and print container ID")
	flags.StringVar(&devOptions.Options.Name, "name", "", "Assign a name to the container")
	flags.StringVar(&devOptions.Options.Pull, "pull", dev.PullImageMissing, `Pull image before running ("`+dev.PullImageAlways+`"|"`+dev.PullImageMissing+`"|"`+dev.PullImageNever+`")`)
	flags.BoolVarP(&devOptions.Options.Quiet, "quiet", "q", false, "Suppress the pull output")

	// Add an explicit help that doesn't have a `-h` to prevent the conflict
	// with hostname
	flags.Bool("help", false, "Print usage")

	command.AddPlatformFlag(flags, &devOptions.Options.Platform)
	command.AddTrustVerificationFlags(flags, &devOptions.Options.Untrusted, dockerCli.ContentTrustEnabled())
	devOptions.Copts = dev.AddFlags(flags)

	_ = cmd.RegisterFlagCompletionFunc(
		"env",
		func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return os.Environ(), cobra.ShellCompDirectiveNoFileComp
		},
	)
	_ = cmd.RegisterFlagCompletionFunc(
		"env-file",
		func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveDefault
		},
	)
	_ = cmd.RegisterFlagCompletionFunc(
		"network",
		dockercomp.NetworkNames(nil),
	)

	addSshFlags(cmd, sshConf)
	return cmd
}
