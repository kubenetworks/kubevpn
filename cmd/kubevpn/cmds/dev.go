package cmds

import (
	"context"

	"github.com/docker/cli/cli/command"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dev"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/regctl"
)

func CmdDev(f cmdutil.Factory) *cobra.Command {
	var options = &dev.Options{
		NoProxy:        false,
		ExtraRouteInfo: handler.ExtraRouteInfo{},
	}
	var sshConf = &pkgssh.SshConfig{}
	var transferImage bool
	var imagePullSecretName string
	var managerNamespace string
	cmd := &cobra.Command{
		Use:   "dev TYPE/NAME [-c CONTAINER] [flags] -- [args...]",
		Short: i18n.T("Startup your kubernetes workloads in local Docker container"),
		Long: templates.LongDesc(i18n.T(`
		Startup your kubernetes workloads in local Docker container with same volume、env、and network

		## What did it do:
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

		# Develop workloads with mesh, traffic with header foo=bar, will hit local PC, otherwise no effect
		kubevpn dev service/productpage --headers foo=bar

        # Develop workloads without proxy traffic
		kubevpn dev service/productpage --no-proxy

		# Develop workloads which api-server behind of bastion host or ssh jump host
		kubevpn dev deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also supports ProxyJump, like
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
		Args:                  cobra.MatchAll(cobra.OnlyValidArgs, cobra.MinimumNArgs(1)),
		DisableFlagsInUseLine: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			err := cmd.Flags().Parse(args[1:])
			if err != nil {
				return err
			}
			plog.InitLoggerForClient()
			err = daemon.StartupDaemon(cmd.Context())
			if err != nil {
				return err
			}
			if transferImage {
				err = regctl.TransferImageWithRegctl(cmd.Context(), config.OriginImage, config.Image)
			}
			if err != nil {
				return err
			}
			bytes, _, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			file, err := util.ConvertToTempKubeconfigFile(bytes)
			if err != nil {
				return err
			}
			return pkgssh.SshJumpAndSetEnv(cmd.Context(), sshConf, file, false)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			options.Workload = args[0]
			for i, arg := range args {
				if arg == "--" && i != len(args)-1 {
					options.ContainerOptions.Args = args[i+1:]
					break
				}
			}

			if err := options.InitClient(f); err != nil {
				return err
			}

			conf, hostConfig, err := dev.Parse(cmd.Flags(), options.ContainerOptions)
			if err != nil {
				return err
			}

			defer func() {
				for _, function := range options.GetRollbackFuncList() {
					if function != nil {
						if err := function(); err != nil {
							plog.G(context.Background()).Errorf("Rollback failed, error: %s", err.Error())
						}
					}
				}
			}()

			return options.Main(cmd.Context(), sshConf, conf, hostConfig, imagePullSecretName, managerNamespace)
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringToStringVarP(&options.Headers, "headers", "H", map[string]string{}, "Traffic with special headers with reverse it to local PC, you should startup your service after reverse workloads successfully, If not special, redirect all traffic to local PC, format is k=v, like: k1=v1,k2=v2")
	cmd.Flags().BoolVar(&options.NoProxy, "no-proxy", false, "Whether proxy remote workloads traffic into local or not, true: just startup container on local without inject containers to intercept traffic, false: intercept traffic and forward to local")
	cmdutil.AddContainerVarFlags(cmd, &options.ContainerName, options.ContainerName)
	cmdutil.CheckErr(cmd.RegisterFlagCompletionFunc("container", completion.ContainerCompletionFunc(f)))
	cmd.Flags().StringVar((*string)(&options.ConnectMode), "connect-mode", string(dev.ConnectModeHost), "Connect to kubernetes network in container or in host, eg: ["+string(dev.ConnectModeContainer)+"|"+string(dev.ConnectModeHost)+"]")
	handler.AddCommonFlags(cmd.Flags(), &transferImage, &imagePullSecretName, &options.Engine)
	cmd.Flags().StringVar(&managerNamespace, "manager-namespace", "", "The namespace where the traffic manager is to be found. Only works in cluster mode (install kubevpn server by helm)")

	// diy docker options
	cmd.Flags().StringVar(&options.DevImage, "dev-image", "", "Use to startup docker container, Default is pod image")
	// -- origin docker options -- start
	options.ContainerOptions = dev.AddFlags(cmd.Flags())
	cmd.Flags().StringVar(&options.RunOptions.Pull, "pull", dev.PullImageMissing, `Pull image before running ("`+dev.PullImageAlways+`"|"`+dev.PullImageMissing+`"|"`+dev.PullImageNever+`")`)
	command.AddPlatformFlag(cmd.Flags(), &options.RunOptions.Platform)
	// -- origin docker options -- end
	handler.AddExtraRoute(cmd.Flags(), &options.ExtraRouteInfo)
	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	return cmd
}
