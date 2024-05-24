package cmds

import (
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/upgrade"
)

func CmdUpgrade(_ cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: i18n.T("Upgrade kubevpn client to latest version"),
		Long: templates.LongDesc(i18n.T(`
		Upgrade kubevpn client to latest version, automatically download and install latest kubevpn from GitHub.
		disconnect all from k8s cluster, leave all resources, remove all clone resource, and then,  
		upgrade local daemon grpc server to latest version.
		`)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return upgrade.Main(cmd.Context())
		},
	}
	return cmd
}
