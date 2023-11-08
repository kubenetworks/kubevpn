package cmds

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

func CmdGet(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "get",
		Hidden: true,
		Short:  i18n.T("Get cluster resources which connected"),
		Long:   templates.LongDesc(i18n.T(`Get cluster resources which connected`)),
		Example: templates.Examples(i18n.T(`
		# Get resource to k8s cluster network
		kubevpn get pods

		# Get api-server behind of bastion host or ssh jump host
		kubevpn get deployment --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn get service --ssh-alias <alias>
`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, _, err := f.ToRawKubeConfigLoader().Namespace()
			if err != nil {
				err = errors.Wrap(err, "f.ToRawKubeConfigLoader().Namespace(): ")
				return err
			}
			client, err := daemon.GetClient(false).Get(
				cmd.Context(),
				&rpc.GetRequest{
					Namespace: namespace,
					Resource:  args[0],
				},
			)
			if err != nil {
				return err
			}
			marshal, err := yaml.Marshal(client.Metadata)
			if err != nil {
				err = errors.Wrap(err, "yaml.Marshal(client.Metadata): ")
				return err
			}
			fmt.Fprint(os.Stdout, string(marshal))
			return nil
		},
	}
	return cmd
}
