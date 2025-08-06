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

// CmdSync multiple cluster operate, can start up one deployment to another cluster
// kubectl exec POD_NAME -c CONTAINER_NAME /sbin/killall5 or ephemeralcontainers
func CmdSync(f cmdutil.Factory) *cobra.Command {
	var options = handler.SyncOptions{}
	var sshConf = &pkgssh.SshConfig{}
	var extraRoute = &handler.ExtraRouteInfo{}
	var transferImage bool
	var syncDir string
	var imagePullSecretName string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: i18n.T("Sync workloads run in current namespace with same volume、env、and network"),
		Long: templates.LongDesc(i18n.T(`
		Sync local dir to workloads which run in current namespace with same volume、env、and network

		In this way, you can startup another deployment in current namespace, but with different image version,
		it also supports service mesh proxy. only traffic with special header will hit to sync resource.
		`)),
		Example: templates.Examples(i18n.T(`
		# sync
		- sync deployment run in current namespace with sync ~/code to /code/app
		  kubevpn sync deployment/productpage --sync ~/code:/code/app

		# sync with mesh, traffic with header foo=bar, will hit sync workloads, otherwise hit origin workloads
		kubevpn sync deployment/productpage --sync ~/code:/code/app --headers foo=bar

		# sync workloads which api-server behind of bastion host or ssh jump host
		kubevpn sync deployment/productpage --sync ~/code:/code/app --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem --headers foo=bar

		# It also supports ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn sync deployment/productpage --sync ~/code:/code/app --ssh-alias <alias> --headers foo=bar

		# Support ssh auth GSSAPI
        kubevpn sync deployment/productpage --sync ~/code:/code/app --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn sync deployment/productpage --sync ~/code:/code/app --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn sync deployment/productpage --sync ~/code:/code/app --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>
		`)),
		Args: cobra.MatchAll(cobra.OnlyValidArgs, cobra.MinimumNArgs(1)),
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
			req := &rpc.SyncRequest{
				KubeconfigBytes:      string(bytes),
				Namespace:            ns,
				Headers:              options.Headers,
				Workloads:            args,
				ExtraRoute:           extraRoute.ToRPC(),
				OriginKubeconfigPath: util.GetKubeConfigPath(f),
				SshJump:              sshConf.ToRPC(),
				TargetContainer:      options.TargetContainer,
				TargetImage:          options.TargetImage,
				TransferImage:        transferImage,
				Image:                config.Image,
				ImagePullSecretName:  imagePullSecretName,
				Level:                int32(util.If(config.Debug, log.DebugLevel, log.InfoLevel)),
				LocalDir:             options.LocalDir,
				RemoteDir:            options.RemoteDir,
			}
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			resp, err := cli.Sync(context.Background())
			if err != nil {
				return err
			}
			err = resp.Send(req)
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.SyncResponse](cmd.Context(), resp)
			if err != nil {
				if status.Code(err) == codes.Canceled {
					return nil
				}
				return err
			}
			_, _ = fmt.Fprintln(os.Stdout, config.Slogan)
			return nil
		},
	}
	cmd.Flags().StringToStringVarP(&options.Headers, "headers", "H", map[string]string{}, "Traffic with special headers (use `and` to match all headers) with reverse it to target cluster sync workloads, If not special, redirect all traffic to target cluster sync workloads. eg: --headers foo=bar --headers env=dev")
	handler.AddCommonFlags(cmd.Flags(), &transferImage, &imagePullSecretName)

	cmdutil.AddContainerVarFlags(cmd, &options.TargetContainer, options.TargetContainer)
	cmd.Flags().StringVar(&options.TargetImage, "target-image", "", "Sync container use this image to startup container, if not special, use origin image")
	cmd.Flags().StringVar(&syncDir, "sync", "", "Sync local dir to remote pod dir. format: LOCAL_DIR:REMOTE_DIR, eg: ~/code:/app/code")

	handler.AddExtraRoute(cmd.Flags(), extraRoute)
	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	cmd.ValidArgsFunction = utilcomp.ResourceTypeAndNameCompletionFunc(f)
	return cmd
}
