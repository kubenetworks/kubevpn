package cmds

import (
	"context"
	"fmt"
	"io"
	defaultlog "log"
	"os"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	utilcomp "k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/dev"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// CmdDuplicate multiple cluster operate, can start up one deployment to another cluster
// kubectl exec POD_NAME -c CONTAINER_NAME /sbin/killall5 or ephemeralcontainers
func CmdDuplicate(f cmdutil.Factory) *cobra.Command {
	var duplicateOptions = handler.DuplicateOptions{}
	var sshConf = &util.SshConfig{}
	var transferImage bool
	cmd := &cobra.Command{
		Use:   "duplicate",
		Short: i18n.T("Duplicate workloads to target-kubeconfig cluster with same volume、env、and network"),
		Long:  templates.LongDesc(i18n.T(`Duplicate workloads to target-kubeconfig cluster with same volume、env、and network`)),
		Example: templates.Examples(i18n.T(`
		# duplicate
		- duplicate deployment in current cluster and current namespace
		  kubevpn duplicate deployment/productpage

		- duplicate deployment in current cluster with different namespace
		  kubevpn duplicate deployment/productpage -n test
        
		- duplicate deployment to another cluster
		  kubevpn duplicate deployment/productpage --target-kubeconfig ~/.kube/other-kubeconfig

        - duplicate multiple workloads
          kubevpn duplicate deployment/authors deployment/productpage
          or 
          kubevpn duplicate deployment authors productpage

		# duplicate with mesh, traffic with header a=1, will hit duplicate workloads, otherwise hit origin workloads
		kubevpn duplicate deployment/productpage --headers a=1

		# duplicate workloads which api-server behind of bastion host or ssh jump host
		kubevpn duplicate deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem --headers a=1

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn duplicate service/productpage --ssh-alias <alias> --headers a=1

`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			if !util.IsAdmin() {
				util.RunWithElevated()
				os.Exit(0)
			}
			go util.StartupPProf(config.PProfPort)
			util.InitLogger(config.Debug)
			defaultlog.Default().SetOutput(io.Discard)
			if transferImage {
				if err = dev.TransferImage(cmd.Context(), sshConf); err != nil {
					return err
				}
			}
			// not support temporally
			if duplicateOptions.Engine == config.EngineGvisor {
				return fmt.Errorf(`not support type engine: %s, support ("%s"|"%s")`, config.EngineGvisor, config.EngineMix, config.EngineRaw)
			}
			return handler.SshJump(sshConf, cmd.Flags())
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
			duplicateOptions.IsChangeTargetRegistry = cmd.Flags().Changed("target-registry")

			connectOptions := handler.ConnectOptions{
				Namespace: duplicateOptions.Namespace,
				Workloads: args,
				ExtraCIDR: duplicateOptions.ExtraCIDR,
				Engine:    duplicateOptions.Engine,
			}
			if err := connectOptions.InitClient(f); err != nil {
				return err
			}
			err := connectOptions.PreCheckResource()
			if err != nil {
				return err
			}
			duplicateOptions.Workloads = connectOptions.Workloads
			connectOptions.Workloads = []string{}
			if err = connectOptions.DoConnect(); err != nil {
				log.Errorln(err)
				handler.Cleanup(syscall.SIGQUIT)
			} else {
				err = duplicateOptions.InitClient(f)
				if err != nil {
					return err
				}
				err = duplicateOptions.DoDuplicate(context.Background())
				if err != nil {
					return err
				}
				util.Print(os.Stdout, "Now duplicate workloads running successfully on other cluster, enjoy it :)")
			}
			select {}
		},
	}
	cmd.Flags().StringToStringVarP(&duplicateOptions.Headers, "headers", "H", map[string]string{}, "Traffic with special headers with reverse it to duplicate workloads, you should startup your service after reverse workloads successfully, If not special, redirect all traffic to duplicate workloads, format is k=v, like: k1=v1,k2=v2")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "Use this image to startup container")
	cmd.Flags().StringArrayVar(&duplicateOptions.ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	cmd.Flags().StringArrayVar(&duplicateOptions.ExtraDomain, "extra-domain", []string{}, "Extra domain string, the resolved ip will add to route table, eg: --extra-domain test.abc.com --extra-domain foo.test.com")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
	cmd.Flags().StringVar((*string)(&duplicateOptions.Engine), "engine", string(config.EngineRaw), fmt.Sprintf(`transport engine ("%s"|"%s") %s: use gvisor and raw both (both performance and stable), %s: use raw mode (best stable)`, config.EngineMix, config.EngineRaw, config.EngineMix, config.EngineRaw))

	cmd.Flags().StringVar(&duplicateOptions.TargetImage, "target-image", "", "Duplicate container use this image to startup container, if not special, use origin origin image")
	cmd.Flags().StringVar(&duplicateOptions.TargetContainer, "target-container", "", "Duplicate container use special image to startup this container, if not special, use origin origin image")
	cmd.Flags().StringVar(&duplicateOptions.TargetNamespace, "target-namespace", "", "Duplicate workloads in this namespace, if not special, use origin namespace")
	cmd.Flags().StringVar(&duplicateOptions.TargetKubeconfig, "target-kubeconfig", "", "Duplicate workloads will create in this cluster, if not special, use origin cluster")
	cmd.Flags().StringVar(&duplicateOptions.TargetRegistry, "target-registry", "", "Duplicate workloads will create this registry domain to replace origin registry, if not special, use origin registry")

	addSshFlags(cmd, sshConf)
	cmd.ValidArgsFunction = utilcomp.ResourceTypeAndNameCompletionFunc(f)
	return cmd
}
