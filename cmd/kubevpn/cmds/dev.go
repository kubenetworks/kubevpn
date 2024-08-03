package cmds

import (
	"fmt"
	"os"

	"github.com/containerd/containerd/platforms"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dev"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdDev(f cmdutil.Factory) *cobra.Command {
	var options = &dev.Options{
		NoProxy:        false,
		ExtraRouteInfo: handler.ExtraRouteInfo{},
	}
	var sshConf = &pkgssh.SshConfig{}
	var transferImage bool
	cmd := &cobra.Command{
		Use:   "dev TYPE/NAME [-c CONTAINER] [flags] -- [args...]",
		Short: i18n.T("Startup your kubernetes workloads in local Docker container"),
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
		kubevpn dev deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn dev deployment/productpage --ssh-alias <alias>

		# Switch to terminal mode; send stdin to 'bash' and sends stdout/stderror from 'bash' back to the client
        kubevpn dev deployment/authors -n default --kubeconfig ~/.kube/config --ssh-alias dev --entrypoint /bin/bash
		  or
        kubevpn dev deployment/authors -n default --kubeconfig ~/.kube/config --ssh-alias dev --entrypoint /bin/bash

		# Support ssh auth GSSAPI
        kubevpn dev deployment/authors -n default --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab --entrypoint /bin/bash
        kubevpn dev deployment/authors -n default --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache --entrypoint /bin/bash
        kubevpn dev deployment/authors -n default --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD> --entrypoint /bin/bash
		`)),
		ValidArgsFunction:     completion.ResourceTypeAndNameCompletionFunc(f),
		Args:                  cobra.MatchAll(cobra.OnlyValidArgs),
		DisableFlagsInUseLine: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				_, _ = fmt.Fprintf(os.Stdout, "You must specify the type of resource to proxy. %s\n\n", cmdutil.SuggestAPIResources("kubevpn"))
				fullCmdName := cmd.Parent().CommandPath()
				usageString := "Required resource not specified."
				if len(fullCmdName) > 0 && cmdutil.IsSiblingCommandExists(cmd, "explain") {
					usageString = fmt.Sprintf("%s\nUse \"%s explain <resource>\" for a detailed description of that resource (e.g. %[2]s explain pods).", usageString, fullCmdName)
				}
				return cmdutil.UsageErrorf(cmd, usageString)
			}
			err := cmd.Flags().Parse(args[1:])
			if err != nil {
				return err
			}
			util.InitLoggerForClient(config.Debug)
			// not support temporally
			if options.Engine == config.EngineGvisor {
				return fmt.Errorf(`not support type engine: %s, support ("%s"|"%s")`, config.EngineGvisor, config.EngineMix, config.EngineRaw)
			}

			if p := options.RunOptions.Platform; p != "" {
				if _, err = platforms.Parse(p); err != nil {
					return fmt.Errorf("error parsing specified platform: %v", err)
				}
			}
			if err = validatePullOpt(options.RunOptions.Pull); err != nil {
				return err
			}

			err = daemon.StartupDaemon(cmd.Context())
			if err != nil {
				return err
			}
			return pkgssh.SshJumpAndSetEnv(cmd.Context(), sshConf, cmd.Flags(), false)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			options.Workload = args[0]
			for i, arg := range args {
				if arg == "--" && i != len(args)-1 {
					options.ContainerOptions.Args = args[i+1:]
					break
				}
			}

			defer func() {
				for _, function := range options.GetRollbackFuncList() {
					if function != nil {
						if er := function(); er != nil {
							log.Errorf("Rollback failed, error: %s", er.Error())
						}
					}
				}
			}()

			if err := options.InitClient(f); err != nil {
				return err
			}

			err := options.Main(cmd.Context(), sshConf, cmd.Flags(), transferImage)
			return err
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringToStringVarP(&options.Headers, "headers", "H", map[string]string{}, "Traffic with special headers with reverse it to local PC, you should startup your service after reverse workloads successfully, If not special, redirect all traffic to local PC, format is k=v, like: k1=v1,k2=v2")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	cmd.Flags().BoolVar(&options.NoProxy, "no-proxy", false, "Whether proxy remote workloads traffic into local or not, true: just startup container on local without inject containers to intercept traffic, false: intercept traffic and forward to local")
	cmdutil.AddContainerVarFlags(cmd, &options.ContainerName, options.ContainerName)
	cmdutil.CheckErr(cmd.RegisterFlagCompletionFunc("container", completion.ContainerCompletionFunc(f)))
	cmd.Flags().StringVar((*string)(&options.ConnectMode), "connect-mode", string(dev.ConnectModeHost), "Connect to kubernetes network in container or in host, eg: ["+string(dev.ConnectModeContainer)+"|"+string(dev.ConnectModeHost)+"]")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
	cmd.Flags().StringVar((*string)(&options.Engine), "engine", string(config.EngineRaw), fmt.Sprintf(`transport engine ("%s"|"%s") %s: use gvisor and raw both (both performance and stable), %s: use raw mode (best stable)`, config.EngineMix, config.EngineRaw, config.EngineMix, config.EngineRaw))

	// diy docker options
	cmd.Flags().StringVar(&options.DevImage, "dev-image", "", "Use to startup docker container, Default is pod image")
	// origin docker options
	dev.AddDockerFlags(options, cmd.Flags())

	handler.AddExtraRoute(cmd.Flags(), &options.ExtraRouteInfo)
	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	return cmd
}

func validatePullOpt(val string) error {
	switch val {
	case dev.PullImageAlways, dev.PullImageMissing, dev.PullImageNever, "":
		// valid option, but nothing to do yet
		return nil
	default:
		return fmt.Errorf(
			"invalid pull option: '%s': must be one of %q, %q or %q",
			val,
			dev.PullImageAlways,
			dev.PullImageMissing,
			dev.PullImageNever,
		)
	}
}
