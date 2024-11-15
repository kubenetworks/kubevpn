package cmds

import (
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/syncthing"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdSyncthing(_ cmdutil.Factory) *cobra.Command {
	var detach bool
	var dir string
	cmd := &cobra.Command{
		Use:   "syncthing",
		Short: i18n.T("Syncthing"),
		Long:  templates.LongDesc(i18n.T(`Syncthing`)),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			go util.StartupPProfForServer(0)
			return syncthing.StartServer(cmd.Context(), detach, dir)
		},
		Hidden:                true,
		DisableFlagsInUseLine: true,
	}
	cmd.Flags().StringVar(&dir, "dir", "", "dir")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "Run syncthing in background")
	return cmd
}
