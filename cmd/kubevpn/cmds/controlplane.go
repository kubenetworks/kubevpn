package cmds

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

var (
	watchDirectoryFilename string
	port                   uint = 9002
)

func init() {
	controlPlaneCmd.Flags().StringVarP(&watchDirectoryFilename, "watchDirectoryFilename", "w", "/etc/envoy/envoy-config.yaml", "full path to directory to watch for files")
	controlPlaneCmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	RootCmd.AddCommand(controlPlaneCmd)
}

var controlPlaneCmd = &cobra.Command{
	Use:   "control-plane",
	Short: "Control-plane is a envoy xds server",
	Long:  `Control-plane is a envoy xds server, distribute envoy route configuration`,
	Run: func(cmd *cobra.Command, args []string) {
		util.InitLogger(config.Debug)
		controlplane.Main(watchDirectoryFilename, port, log.StandardLogger())
	},
}
