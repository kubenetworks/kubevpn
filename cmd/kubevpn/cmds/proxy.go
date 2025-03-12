package cmds

import (
	"context"
	"fmt"
	"os"

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
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/regctl"
)

func CmdProxy(f cmdutil.Factory) *cobra.Command {
	var connect = handler.ConnectOptions{}
	var extraRoute = &handler.ExtraRouteInfo{}
	var sshConf = &pkgssh.SshConfig{}
	var transferImage, foreground bool
	var imagePullSecretName string
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: i18n.T("Proxy kubernetes workloads inbound traffic into local PC"),
		Long: templates.LongDesc(i18n.T(`
		Proxy kubernetes workloads inbound traffic into local PC

		Proxy k8s workloads inbound traffic into local PC with/without service mesh. 
		Without service mesh, it will proxy all inbound traffic into local PC, even traffic protocol is layer 4(Transport layer).
		With service mesh, it will proxy traffic which has special header to local PC, support protocol HTTP,GRPC,THRIFT, WebSocket...
		After proxy resource, it also connected to cluster network automatically. so just startup your app in local PC
		and waiting for inbound traffic, make debug more easier.
		
		`)),
		Example: templates.Examples(i18n.T(`
		# Reverse proxy
		- proxy deployment
		  kubevpn proxy deployment/productpage

		- proxy service
		  kubevpn proxy service/productpage

        - proxy multiple workloads
          kubevpn proxy deployment/authors deployment/productpage
          or 
          kubevpn proxy deployment authors productpage

		# Reverse proxy with mesh, traffic with header foo=bar, will hit local PC, otherwise no effect
		kubevpn proxy service/productpage --headers foo=bar
		
		# Reverse proxy with mesh, traffic with header foo=bar and env=dev, will hit local PC, otherwise no effect
		kubevpn proxy service/productpage --headers foo=bar --headers env=dev

		# Connect to api-server behind of bastion host or ssh jump host and proxy kubernetes resource traffic into local PC
		kubevpn proxy deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem --headers foo=bar

		# It also supports ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn proxy service/productpage --ssh-alias <alias> --headers foo=bar

		# Support ssh auth GSSAPI
        kubevpn proxy service/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn proxy service/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn proxy service/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>

		# Support port map, you can proxy container port to local port by command:
		kubevpn proxy deployment/productpage --portmap 80:8080
		
		# Proxy container port 9080 to local port 8080 of TCP protocol
		kubevpn proxy deployment/productpage --portmap 9080:8080

		# Proxy container port 9080 to local port 5000 of UDP protocol
		kubevpn proxy deployment/productpage --portmap udp/9080:5000

		# Auto proxy container port to same local port, and auto detect protocol
		kubevpn proxy deployment/productpage
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			util.InitLoggerForClient(false)
			if err = daemon.StartupDaemon(cmd.Context()); err != nil {
				return err
			}
			if transferImage {
				err = regctl.TransferImageWithRegctl(cmd.Context(), config.OriginImage, config.Image)
			}
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintf(os.Stdout, "You must specify the type of resource to proxy. %s\n\n", cmdutil.SuggestAPIResources("kubevpn"))
				fullCmdName := cmd.Parent().CommandPath()
				usageString := "Required resource not specified."
				if len(fullCmdName) > 0 && cmdutil.IsSiblingCommandExists(cmd, "explain") {
					usageString = fmt.Sprintf("%s\nUse \"%s explain <resource>\" for a detailed description of that resource (e.g. %[2]s explain pods).", usageString, fullCmdName)
				}
				return cmdutil.UsageErrorf(cmd, usageString)
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
			// todo 将 doConnect 方法封装？内部使用 client 发送到daemon？
			cli := daemon.GetClient(false)
			logLevel := log.InfoLevel
			if config.Debug {
				logLevel = log.DebugLevel
			}
			client, err := cli.Proxy(
				cmd.Context(),
				&rpc.ConnectRequest{
					KubeconfigBytes:      string(bytes),
					Namespace:            ns,
					Headers:              connect.Headers,
					PortMap:              connect.PortMap,
					Workloads:            args,
					ExtraRoute:           extraRoute.ToRPC(),
					Engine:               string(connect.Engine),
					SshJump:              sshConf.ToRPC(),
					TransferImage:        transferImage,
					Image:                config.Image,
					ImagePullSecretName:  imagePullSecretName,
					Level:                int32(logLevel),
					OriginKubeconfigPath: util.GetKubeConfigPath(f),
				},
			)
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.ConnectResponse](client)
			if err != nil {
				if status.Code(err) == codes.Canceled {
					err = leave(cli, args)
					return err
				}
				return err
			}
			util.Print(os.Stdout, config.Slogan)
			// hangup
			if foreground {
				// leave from cluster resources
				<-cmd.Context().Done()

				err = leave(cli, args)
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringToStringVarP(&connect.Headers, "headers", "H", map[string]string{}, "Traffic with special headers (use `and` to match all headers) with reverse it to local PC, If not special, redirect all traffic to local PC. format: <KEY>=<VALUE> eg: --headers foo=bar --headers env=dev")
	cmd.Flags().StringArrayVar(&connect.PortMap, "portmap", []string{}, "Port map, map container port to local port, format: [tcp/udp]/containerPort:localPort, If not special, localPort will use containerPort. eg: tcp/80:8080 or udp/5000:5001 or 80 or 80:8080")
	handler.AddCommonFlags(cmd.Flags(), &transferImage, &imagePullSecretName, &connect.Engine)
	cmd.Flags().BoolVar(&foreground, "foreground", false, "foreground hang up")

	handler.AddExtraRoute(cmd.Flags(), extraRoute)
	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	cmd.ValidArgsFunction = utilcomp.ResourceTypeAndNameCompletionFunc(f)
	return cmd
}

func leave(cli rpc.DaemonClient, args []string) error {
	stream, err := cli.Leave(context.Background(), &rpc.LeaveRequest{
		Workloads: args,
	})
	if err != nil {
		return err
	}
	err = util.PrintGRPCStream[rpc.LeaveResponse](stream)
	if err != nil {
		if status.Code(err) == codes.Canceled {
			return nil
		}
		return err
	}
	return nil
}
