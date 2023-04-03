package cmds

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/pkg/util"
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
		Run: func(cmd *cobra.Command, args []string) {
			util.InitLogger(config.Debug)
			go util.StartupPProf(0)
			controlplane.Main(watchDirectoryFilename, port, log.StandardLogger())
		},
	}
	cmd.Flags().StringVarP(&watchDirectoryFilename, "watchDirectoryFilename", "w", "/etc/envoy/envoy-config.yaml", "full path to directory to watch for files")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	return cmd
}
