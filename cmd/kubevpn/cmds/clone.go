package cmds

import (
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

// CmdClone multiple cluster operate, can start up one deployment to another cluster
// kubectl exec POD_NAME -c CONTAINER_NAME /sbin/killall5 or ephemeralcontainers
func CmdClone(f cmdutil.Factory) *cobra.Command {
	var options = handler.CloneOptions{}
	var sshConf = &util.SshConfig{}
	var transferImage bool
	cmd := &cobra.Command{
		Use:   "clone",
		Short: i18n.T("Clone workloads to target-kubeconfig cluster with same volume、env、and network"),
		Long:  templates.LongDesc(i18n.T(`Clone workloads to target-kubeconfig cluster with same volume、env、and network`)),
		Example: templates.Examples(i18n.T(`
		# clone
		- clone deployment in current cluster and current namespace
		  kubevpn clone deployment/productpage

		- clone deployment in current cluster with different namespace
		  kubevpn clone deployment/productpage -n test
        
		- clone deployment to another cluster
		  kubevpn clone deployment/productpage --target-kubeconfig ~/.kube/other-kubeconfig

        - clone multiple workloads
          kubevpn clone deployment/authors deployment/productpage
          or 
          kubevpn clone deployment authors productpage

		# clone with mesh, traffic with header a=1, will hit cloned workloads, otherwise hit origin workloads
		kubevpn clone deployment/productpage --headers a=1

		# clone workloads which api-server behind of bastion host or ssh jump host
		kubevpn clone deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem --headers a=1

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn clone service/productpage --ssh-alias <alias> --headers a=1

		# Support ssh auth GSSAPI
        kubevpn clone service/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn clone service/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn clone service/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>
`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// not support temporally
			if options.Engine == config.EngineGvisor {
				return fmt.Errorf(`not support type engine: %s, support ("%s"|"%s")`, config.EngineGvisor, config.EngineMix, config.EngineRaw)
			}
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
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

			bytes, ns, err := util.ConvertToKubeconfigBytes(f)
			if err != nil {
				return err
			}
			logLevel := log.ErrorLevel
			if config.Debug {
				logLevel = log.DebugLevel
			}
			req := &rpc.CloneRequest{
				KubeconfigBytes:        string(bytes),
				Namespace:              ns,
				Headers:                options.Headers,
				Workloads:              args,
				ExtraCIDR:              options.ExtraCIDR,
				ExtraDomain:            options.ExtraDomain,
				UseLocalDNS:            options.UseLocalDNS,
				OriginKubeconfigPath:   util.GetKubeconfigPath(f),
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
				Level:                  int32(logLevel),
			}
			cli := daemon.GetClient(false)
			resp, err := cli.Clone(cmd.Context(), req)
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
			util.Print(os.Stdout, "Now clone workloads running successfully on other cluster, enjoy it :)")
			return nil
		},
	}
	cmd.Flags().StringToStringVarP(&options.Headers, "headers", "H", map[string]string{}, "Traffic with special headers (use `and` to match all headers) with reverse it to local PC, If not special, redirect all traffic to local PC. eg: --headers a=1 --headers b=2")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "Use this image to startup container")
	cmd.Flags().StringArrayVar(&options.ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	cmd.Flags().StringArrayVar(&options.ExtraDomain, "extra-domain", []string{}, "Extra domain string, the resolved ip will add to route table, eg: --extra-domain test.abc.com --extra-domain foo.test.com")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
	cmd.Flags().StringVar((*string)(&options.Engine), "engine", string(config.EngineRaw), fmt.Sprintf(`transport engine ("%s"|"%s") %s: use gvisor and raw both (both performance and stable), %s: use raw mode (best stable)`, config.EngineMix, config.EngineRaw, config.EngineMix, config.EngineRaw))
	cmd.Flags().BoolVar(&options.UseLocalDNS, "use-localdns", false, "if use-lcoaldns is true, kubevpn will start coredns listen at 53 to forward your dns queries. only support on linux now")

	cmd.Flags().StringVar(&options.TargetImage, "target-image", "", "Clone container use this image to startup container, if not special, use origin image")
	cmd.Flags().StringVar(&options.TargetContainer, "target-container", "", "Clone container use special image to startup this container, if not special, use origin image")
	cmd.Flags().StringVar(&options.TargetNamespace, "target-namespace", "", "Clone workloads in this namespace, if not special, use origin namespace")
	cmd.Flags().StringVar(&options.TargetKubeconfig, "target-kubeconfig", "", "Clone workloads will create in this cluster, if not special, use origin cluster")
	cmd.Flags().StringVar(&options.TargetRegistry, "target-registry", "", "Clone workloads will create this registry domain to replace origin registry, if not special, use origin registry")

	addSshFlags(cmd, sshConf)
	cmd.ValidArgsFunction = utilcomp.ResourceTypeAndNameCompletionFunc(f)
	return cmd
}
