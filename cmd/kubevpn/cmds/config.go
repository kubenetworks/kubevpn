package cmds

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdConfig(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use: "config",
	}
	cmd.AddCommand(cmdConfigAdd(f))
	cmd.AddCommand(cmdConfigRemove(f))
	return cmd
}

func cmdConfigAdd(f cmdutil.Factory) *cobra.Command {
	var sshConf = &util.SshConfig{}
	cmd := &cobra.Command{
		Use:   "add",
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

`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, ns, err := util.ConvertToKubeconfigBytes(f)
			if err != nil {
				return err
			}
			req := &rpc.ConfigAddRequest{
				KubeconfigBytes: string(bytes),
				Namespace:       ns,
				SshJump:         sshConf.ToRPC(),
			}
			cli := daemon.GetClient(false)
			resp, err := cli.ConfigAdd(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, resp.ClusterID)
			return nil
		},
	}
	addSshFlags(cmd, sshConf)
	return cmd
}

func cmdConfigRemove(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
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

`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
		},
		Args: cobra.MatchAll(cobra.OnlyValidArgs, cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &rpc.ConfigRemoveRequest{
				ClusterID: args[0],
			}
			cli := daemon.GetClient(false)
			_, err := cli.ConfigRemove(cmd.Context(), req)
			if err != nil {
				return err
			}
			return nil
		},
	}
	return cmd
}
