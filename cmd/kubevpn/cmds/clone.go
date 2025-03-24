package cmds

import (
	"context"
	"fmt"
	"os"

	pkgerr "github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	utilcomp "k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/regctl"
)

// CmdClone multiple cluster operate, can start up one deployment to another cluster
// kubectl exec POD_NAME -c CONTAINER_NAME /sbin/killall5 or ephemeralcontainers
func CmdClone(f cmdutil.Factory) *cobra.Command {
	var options = handler.CloneOptions{}
	var sshConf = &pkgssh.SshConfig{}
	var extraRoute = &handler.ExtraRouteInfo{}
	var transferImage bool
	var syncDir string
	var imagePullSecretName string
	cmd := &cobra.Command{
		Use:   "clone",
		Short: i18n.T("Clone workloads to run in target-kubeconfig cluster with same volume、env、and network"),
		Long: templates.LongDesc(i18n.T(`
		Clone workloads to run into target-kubeconfig cluster with same volume、env、and network

		In this way, you can startup another deployment in same cluster or not, but with different image version,
		it also supports service mesh proxy. only traffic with special header will hit to cloned_resource.
		`)),
		Example: templates.Examples(i18n.T(`
		# clone
		- clone deployment run into current cluster and current namespace
		  kubevpn clone deployment/productpage

		- clone deployment run into current cluster with different namespace
		  kubevpn clone deployment/productpage -n test
        
		- clone deployment run into another cluster
		  kubevpn clone deployment/productpage --target-kubeconfig ~/.kube/other-kubeconfig

        - clone multiple workloads run into current cluster and current namespace
          kubevpn clone deployment/authors deployment/productpage
          or 
          kubevpn clone deployment authors productpage

		# clone with mesh, traffic with header foo=bar, will hit cloned workloads, otherwise hit origin workloads
		kubevpn clone deployment/productpage --headers foo=bar

		# clone workloads which api-server behind of bastion host or ssh jump host
		kubevpn clone deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem --headers foo=bar

		# It also supports ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn clone service/productpage --ssh-alias <alias> --headers foo=bar

		# Support ssh auth GSSAPI
        kubevpn clone service/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn clone service/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn clone service/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			plog.InitLoggerForClient()
			// startup daemon process and sudo process
			err = daemon.StartupDaemon(cmd.Context())
			if err != nil {
				return err
			}
			if transferImage {
				err = regctl.TransferImageWithRegctl(cmd.Context(), config.OriginImage, config.Image)
			}
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				_, _ = fmt.Fprintf(os.Stdout, "You must specify the type of resource to proxy. %s\n\n", cmdutil.SuggestAPIResources("kubevpn"))
				fullCmdName := cmd.Parent().CommandPath()
				usageString := "Required resource not specified."
				if len(fullCmdName) > 0 && cmdutil.IsSiblingCommandExists(cmd, "explain") {
					usageString = fmt.Sprintf("%s\nUse \"%s explain <resource>\" for a detailed description of that resource (e.g. %[2]s explain pods).", usageString, fullCmdName)
				}
				return cmdutil.UsageErrorf(cmd, usageString)
			}
			// special empty string, eg: --target-registry ""
			options.IsChangeTargetRegistry = cmd.Flags().Changed("target-registry")

			if syncDir != "" {
				local, remote, err := util.ParseDirMapping(syncDir)
				if err != nil {
					return pkgerr.Wrapf(err, "options 'sync' is invalid, %s", syncDir)
				}
				options.LocalDir = local
				options.RemoteDir = remote
			} else {
				options.RemoteDir = config.DefaultRemoteDir
			}

			bytes, ns, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			if !sshConf.IsEmpty() {
				if ip := util.GetAPIServerFromKubeConfigBytes(bytes); ip != nil {
					extraRoute.ExtraCIDR = append(extraRoute.ExtraCIDR, ip.String())
				}
			}
			req := &rpc.CloneRequest{
				KubeconfigBytes:        string(bytes),
				Namespace:              ns,
				Headers:                options.Headers,
				Workloads:              args,
				ExtraRoute:             extraRoute.ToRPC(),
				OriginKubeconfigPath:   util.GetKubeConfigPath(f),
				Engine:                 string(options.Engine),
				SshJump:                sshConf.ToRPC(),
				TargetKubeconfig:       options.TargetKubeconfig,
				TargetNamespace:        options.TargetNamespace,
				TargetContainer:        options.TargetContainer,
				TargetImage:            options.TargetImage,
				TargetRegistry:         options.TargetRegistry,
				IsChangeTargetRegistry: options.IsChangeTargetRegistry,
				TransferImage:          transferImage,
				Image:                  config.Image,
				ImagePullSecretName:    imagePullSecretName,
				Level:                  int32(util.If(config.Debug, log.DebugLevel, log.InfoLevel)),
				LocalDir:               options.LocalDir,
				RemoteDir:              options.RemoteDir,
			}
			cli := daemon.GetClient(false)
			resp, err := cli.Clone(cmd.Context(), req)
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.CloneResponse](resp)
			if err != nil {
				if status.Code(err) == codes.Canceled {
					err = remove(cli, args)
					return err
				}
				return err
			}
			util.Print(os.Stdout, config.Slogan)
			return nil
		},
	}
	cmd.Flags().StringToStringVarP(&options.Headers, "headers", "H", map[string]string{}, "Traffic with special headers (use `and` to match all headers) with reverse it to target cluster cloned workloads, If not special, redirect all traffic to target cluster cloned workloads. eg: --headers foo=bar --headers env=dev")
	handler.AddCommonFlags(cmd.Flags(), &transferImage, &imagePullSecretName, &options.Engine)

	cmdutil.AddContainerVarFlags(cmd, &options.TargetContainer, options.TargetContainer)
	cmd.Flags().StringVar(&options.TargetImage, "target-image", "", "Clone container use this image to startup container, if not special, use origin image")
	cmd.Flags().StringVar(&options.TargetNamespace, "target-namespace", "", "Clone workloads in this namespace, if not special, use origin namespace")
	cmd.Flags().StringVar(&options.TargetKubeconfig, "target-kubeconfig", "", "Clone workloads will create in this cluster, if not special, use origin cluster")
	cmd.Flags().StringVar(&options.TargetRegistry, "target-registry", "", "Clone workloads will create this registry domain to replace origin registry, if not special, use origin registry")
	cmd.Flags().StringVar(&syncDir, "sync", "", "Sync local dir to remote pod dir. format: LOCAL_DIR:REMOTE_DIR, eg: ~/code:/app/code")

	handler.AddExtraRoute(cmd.Flags(), extraRoute)
	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	cmd.ValidArgsFunction = utilcomp.ResourceTypeAndNameCompletionFunc(f)
	return cmd
}

func remove(cli rpc.DaemonClient, args []string) error {
	resp, err := cli.Remove(context.Background(), &rpc.RemoveRequest{
		Workloads: args,
	})
	if err != nil {
		return err
	}
	err = util.PrintGRPCStream[rpc.DisconnectResponse](resp)
	if err != nil {
		if status.Code(err) == codes.Canceled {
			return nil
		}
		return err
	}
	return nil
}
