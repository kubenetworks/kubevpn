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
	utilcomp "k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdProxy(f cmdutil.Factory) *cobra.Command {
	var connect = handler.ConnectOptions{}
	var extraRoute = &handler.ExtraRouteInfo{}
	var sshConf = &util.SshConfig{}
	var transferImage, foreground bool
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: i18n.T("Proxy kubernetes workloads inbound traffic into local PC"),
		Long:  templates.LongDesc(i18n.T(`Proxy kubernetes workloads inbound traffic into local PC`)),
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

		# Reverse proxy with mesh, traffic with header a=1, will hit local PC, otherwise no effect
		kubevpn proxy service/productpage --headers a=1
		
		# Reverse proxy with mesh, traffic with header a=1 and b=2, will hit local PC, otherwise no effect
		kubevpn proxy service/productpage --headers a=1 --headers b=2

		# Connect to api-server behind of bastion host or ssh jump host and proxy kubernetes resource traffic into local PC
		kubevpn proxy deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem --headers a=1

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn proxy service/productpage --ssh-alias <alias> --headers a=1

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
			if err = daemon.StartupDaemon(cmd.Context()); err != nil {
				return err
			}
			// not support temporally
			if connect.Engine == config.EngineGvisor {
				return fmt.Errorf(`not support type engine: %s, support ("%s"|"%s")`, config.EngineGvisor, config.EngineMix, config.EngineRaw)
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
			// todo 将 doConnect 方法封装？内部使用 client 发送到daemon？
			cli := daemon.GetClient(false)
			logLevel := log.ErrorLevel
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
					Level:                int32(logLevel),
					OriginKubeconfigPath: util.GetKubeConfigPath(f),
				},
			)
			if err != nil {
				return err
			}
			var resp *rpc.ConnectResponse
			for {
				resp, err = client.Recv()
				if err == io.EOF {
					break
				} else if err == nil {
					fmt.Fprint(os.Stdout, resp.Message)
				} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
					return nil
				} else {
					return err
				}
			}
			util.Print(os.Stdout, "Now you can access resources in the kubernetes cluster, enjoy it :)")
			// hangup
			if foreground {
				// leave from cluster resources
				<-cmd.Context().Done()

				stream, err := cli.Leave(context.Background(), &rpc.LeaveRequest{
					Workloads: args,
				})
				var resp *rpc.LeaveResponse
				for {
					resp, err = stream.Recv()
					if err == io.EOF {
						return nil
					} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
						return nil
					} else if err != nil {
						return err
					}
					fmt.Fprint(os.Stdout, resp.Message)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringToStringVarP(&connect.Headers, "headers", "H", map[string]string{}, "Traffic with special headers (use `and` to match all headers) with reverse it to local PC, If not special, redirect all traffic to local PC. eg: --headers a=1 --headers b=2")
	cmd.Flags().StringArrayVar(&connect.PortMap, "portmap", []string{}, "Port map, map container port to local port, format: [tcp/udp]/containerPort:localPort, If not special, localPort will use containerPort. eg: tcp/80:8080 or udp/5000:5001 or 80 or 80:8080")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "Use this image to startup container")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
	cmd.Flags().StringVar((*string)(&connect.Engine), "engine", string(config.EngineRaw), fmt.Sprintf(`transport engine ("%s"|"%s") %s: use gvisor and raw both (both performance and stable), %s: use raw mode (best stable)`, config.EngineMix, config.EngineRaw, config.EngineMix, config.EngineRaw))
	cmd.Flags().BoolVar(&foreground, "foreground", false, "foreground hang up")

	addExtraRoute(cmd, extraRoute)
	addSshFlags(cmd, sshConf)
	cmd.ValidArgsFunction = utilcomp.ResourceTypeAndNameCompletionFunc(f)
	return cmd
}

func addSshFlags(cmd *cobra.Command, sshConf *util.SshConfig) {
	// for ssh jumper host
	cmd.Flags().StringVar(&sshConf.Addr, "ssh-addr", "", "Optional ssh jump server address to dial as <hostname>:<port>, eg: 127.0.0.1:22")
	cmd.Flags().StringVar(&sshConf.User, "ssh-username", "", "Optional username for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Password, "ssh-password", "", "Optional password for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Keyfile, "ssh-keyfile", "", "Optional file with private key for SSH authentication")
	cmd.Flags().StringVar(&sshConf.ConfigAlias, "ssh-alias", "", "Optional config alias with ~/.ssh/config for SSH authentication")
	cmd.Flags().StringVar(&sshConf.GSSAPIPassword, "gssapi-password", "", "GSSAPI password")
	cmd.Flags().StringVar(&sshConf.GSSAPIKeytabConf, "gssapi-keytab", "", "GSSAPI keytab file path")
	cmd.Flags().StringVar(&sshConf.GSSAPICacheFile, "gssapi-cache", "", "GSSAPI cache file path, use command `kinit -c /path/to/cache USERNAME@RELAM` to generate")
	cmd.Flags().StringVar(&sshConf.RemoteKubeconfig, "remote-kubeconfig", "", "Remote kubeconfig abstract path of ssh server, default is /home/$USERNAME/.kube/config")
	lookup := cmd.Flags().Lookup("remote-kubeconfig")
	lookup.NoOptDefVal = "~/.kube/config"
}

func addExtraRoute(cmd *cobra.Command, route *handler.ExtraRouteInfo) {
	cmd.Flags().StringArrayVar(&route.ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, add those cidr network to route table, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	cmd.Flags().StringArrayVar(&route.ExtraDomain, "extra-domain", []string{}, "Extra domain string, the resolved ip will add to route table, eg: --extra-domain test.abc.com --extra-domain foo.test.com")
	cmd.Flags().BoolVar(&route.ExtraNodeIP, "extra-node-ip", false, "Extra node ip, add cluster node ip to route table.")
}
