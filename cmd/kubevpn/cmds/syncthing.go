package cmds

import (
	"github.com/spf13/cobra"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/syncthing"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdSyncthing(_ cmdutil.Factory) *cobra.Command {
	var deviceID string
	var detach bool
	cmd := &cobra.Command{
		Use:   "syncthing",
		Short: i18n.T("Syncthing"),
		Long:  templates.LongDesc(i18n.T(`Syncthing`)),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			go util.StartupPProf(0)
			return syncthing.StartServer(cmd.Context(), detach, deviceID)
		},
		Hidden:                true,
		DisableFlagsInUseLine: true,
	}
	cmd.Flags().StringVar(&deviceID, "device-id", config.SyncthingRemoteDeviceID, "syncthing device id")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "Run syncthing in background")
	return cmd
}
