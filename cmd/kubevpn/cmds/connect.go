package cmds

import (
	"context"
	"fmt"
	"io"
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
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
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdConnect(f cmdutil.Factory) *cobra.Command {
	var connect = &handler.ConnectOptions{}
	var extraRoute = &handler.ExtraRouteInfo{}
	var sshConf = &util.SshConfig{}
	var transferImage, foreground, lite bool
	cmd := &cobra.Command{
		Use:   "connect",
		Short: i18n.T("Connect to kubernetes cluster network"),
		Long: templates.LongDesc(i18n.T(`
		Connect to kubernetes cluster network
		
		After connect to kubernetes cluster network, you can ping PodIP or
		curl ServiceIP in local PC, it also support k8s dns resolve. 
		Like: curl authors/authors.default/authors.default.svc/authors.default.svc.cluster.local.
		So you can startup your application in local PC. depends on anything in
		k8s cluster is ok, connect to them just like in k8s cluster.
		`)),
		Example: templates.Examples(i18n.T(`
		# Connect to k8s cluster network
		kubevpn connect

		# Connect to api-server behind of bastion host or ssh jump host
		kubevpn connect --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also support ProxyJump, like
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
			// startup daemon process and sudo process
			err := daemon.StartupDaemon(cmd.Context())
			if err != nil {
				return err
			}
			// not support temporally
			if connect.Engine == config.EngineGvisor {
				return fmt.Errorf(`not support type engine: %s, support ("%s"|"%s")`, config.EngineGvisor, config.EngineMix, config.EngineRaw)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, ns, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			logLevel := log.ErrorLevel
			if config.Debug {
				logLevel = log.DebugLevel
			}
			req := &rpc.ConnectRequest{
				KubeconfigBytes:      string(bytes),
				Namespace:            ns,
				ExtraRoute:           extraRoute.ToRPC(),
				Engine:               string(connect.Engine),
				OriginKubeconfigPath: util.GetKubeConfigPath(f),

				SshJump:       sshConf.ToRPC(),
				TransferImage: transferImage,
				Image:         config.Image,
				Level:         int32(logLevel),
			}
			// if is foreground, send to sudo daemon server
			cli := daemon.GetClient(false)
			if lite {
				resp, err := cli.ConnectFork(cmd.Context(), req)
				if err != nil {
					return err
				}
				for {
					recv, err := resp.Recv()
					if err == io.EOF {
						break
					} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
						return nil
					} else if err != nil {
						return err
					}
					fmt.Fprint(os.Stdout, recv.GetMessage())
				}
			} else {
				resp, err := cli.Connect(cmd.Context(), req)
				if err != nil {
					return err
				}
				for {
					recv, err := resp.Recv()
					if err == io.EOF {
						break
					} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
						return nil
					} else if err != nil {
						return err
					}
					fmt.Fprint(os.Stdout, recv.GetMessage())
				}
			}
			if !foreground {
				util.Print(os.Stdout, "Now you can access resources in the kubernetes cluster, enjoy it :)")
			} else {
				<-cmd.Context().Done()
				disconnect, err := cli.Disconnect(context.Background(), &rpc.DisconnectRequest{
					KubeconfigBytes: ptr.To(string(bytes)),
					Namespace:       ptr.To(ns),
					SshJump:         sshConf.ToRPC(),
				})
				if err != nil {
					log.Errorf("disconnect error: %v", err)
					return err
				}
				for {
					recv, err := disconnect.Recv()
					if err == io.EOF {
						break
					} else if err != nil {
						log.Errorf("receive disconnect error: %v", err)
						return err
					}
					log.Info(recv.Message)
				}
				log.Info("disconnect successfully")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
	cmd.Flags().StringVar((*string)(&connect.Engine), "engine", string(config.EngineRaw), fmt.Sprintf(`transport engine ("%s"|"%s") %s: use gvisor and raw both (both performance and stable), %s: use raw mode (best stable)`, config.EngineMix, config.EngineRaw, config.EngineMix, config.EngineRaw))
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Hang up")
	cmd.Flags().BoolVar(&lite, "lite", false, "connect to multiple cluster in lite mode, you needs to special this options")

	handler.AddExtraRoute(cmd.Flags(), extraRoute)
	util.AddSshFlags(cmd.Flags(), sshConf)
	return cmd
}
