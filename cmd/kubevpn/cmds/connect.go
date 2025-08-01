package cmds

import (
	"context"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/regctl"
)

func CmdConnect(f cmdutil.Factory) *cobra.Command {
	var extraRoute = &handler.ExtraRouteInfo{}
	var sshConf = &pkgssh.SshConfig{}
	var transferImage, foreground bool
	var imagePullSecretName string
	var managerNamespace string
	cmd := &cobra.Command{
		Use:   "connect",
		Short: i18n.T("Connect to kubernetes cluster network"),
		Long: templates.LongDesc(i18n.T(`
		Connect to kubernetes cluster network
		
		After connect to kubernetes cluster network, you can ping PodIP or
		curl ServiceIP in local PC, it also supports k8s DNS resolve. 
		Like: curl authors/authors.default/authors.default.svc/authors.default.svc.cluster.local.
		So you can start up your application in local PC. depends on anything in
		k8s cluster is ok, connect to them just like in k8s cluster.
		`)),
		Example: templates.Examples(i18n.T(`
		# Connect to k8s cluster network
		kubevpn connect

		# Connect to api-server behind of bastion host or ssh jump host
		kubevpn connect --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also supports ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn connect --ssh-alias <alias>

		# Support ssh auth GSSAPI
        kubevpn connect --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn connect --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn connect --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>

		# Support ssh jump inline
		kubevpn connect --ssh-jump "--ssh-addr jump.naison.org --ssh-username naison --gssapi-password xxx" --ssh-username root --ssh-addr 127.0.0.1:22 --ssh-keyfile ~/.ssh/dst.pem
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			plog.InitLoggerForClient()
			// startup daemon process and sudo process
			err := daemon.StartupDaemon(cmd.Context())
			if err != nil {
				return err
			}
			if transferImage {
				err = regctl.TransferImageWithRegctl(cmd.Context(), config.OriginImage, config.Image)
			}
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, ns, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			if !sshConf.IsEmpty() {
				if ip := util.GetAPIServerFromKubeConfigBytes(bytes); ip != nil {
					extraRoute.ExtraCIDR = append(extraRoute.ExtraCIDR, ip.String())
				}
			}
			req := &rpc.ConnectRequest{
				KubeconfigBytes:      string(bytes),
				Namespace:            ns,
				ExtraRoute:           extraRoute.ToRPC(),
				OriginKubeconfigPath: util.GetKubeConfigPath(f),

				SshJump:             sshConf.ToRPC(),
				TransferImage:       transferImage,
				Image:               config.Image,
				ImagePullSecretName: imagePullSecretName,
				Level:               int32(util.If(config.Debug, log.DebugLevel, log.InfoLevel)),
				ManagerNamespace:    managerNamespace,
			}
			// if is foreground, send to sudo daemon server
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			var resp grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse]
			resp, err = cli.Connect(context.Background())
			if err != nil {
				return err
			}
			err = resp.Send(req)
			if err != nil {
				return err
			}
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.ConnectResponse](cmd.Context(), resp)
			if err != nil {
				if status.Code(err) == codes.Canceled {
					return nil
				}
				return err
			}
			if !foreground {
				util.Print(os.Stdout, config.Slogan)
			} else {
				<-cmd.Context().Done()
				err = disconnect(cli, bytes, ns, sshConf)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprint(os.Stdout, "Disconnect completed")
			}
			return nil
		},
	}
	handler.AddCommonFlags(cmd.Flags(), &transferImage, &imagePullSecretName)
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Hang up")
	cmd.Flags().StringVar(&managerNamespace, "manager-namespace", "", "The namespace where the traffic manager is to be found. Only works in cluster mode (install kubevpn server by helm)")

	handler.AddExtraRoute(cmd.Flags(), extraRoute)
	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	return cmd
}

func disconnect(cli rpc.DaemonClient, bytes []byte, ns string, sshConf *pkgssh.SshConfig) error {
	resp, err := cli.Disconnect(context.Background())
	if err != nil {
		plog.G(context.Background()).Errorf("Disconnect error: %v", err)
		return err
	}
	err = resp.Send(&rpc.DisconnectRequest{
		KubeconfigBytes: ptr.To(string(bytes)),
		Namespace:       ptr.To(ns),
		SshJump:         sshConf.ToRPC(),
	})
	if err != nil {
		plog.G(context.Background()).Errorf("Disconnect error: %v", err)
		return err
	}
	err = util.PrintGRPCStream[rpc.DisconnectResponse](nil, resp)
	if err != nil {
		if status.Code(err) == codes.Canceled {
			return nil
		}
		return err
	}
	return nil
}
