package cmds

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdControlPlane(_ cmdutil.Factory) *cobra.Command {
	var (
		watchDirectoryFilename string
		port                   uint = 9002
	)
	cmd := &cobra.Command{
		Use:    "control-plane",
		Hidden: true,
		Short:  "Control-plane is a envoy xds server",
		Long:   `Control-plane is a envoy xds server, distribute envoy route configuration`,
		RunE: func(cmd *cobra.Command, args []string) error {
			util.InitLoggerForServer(config.Debug)
			go util.StartupPProf(0)
			err := controlplane.Main(cmd.Context(), watchDirectoryFilename, port, log.StandardLogger())
			return err
		},
	}
	cmd.Flags().StringVarP(&watchDirectoryFilename, "watchDirectoryFilename", "w", "/etc/envoy/envoy-config.yaml", "full path to directory to watch for files")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	return cmd
}
