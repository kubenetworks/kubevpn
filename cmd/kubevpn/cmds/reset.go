package cmds

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/handler"
)

func CmdReset(factory cmdutil.Factory) *cobra.Command {
	var connect = handler.ConnectOptions{}
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset KubeVPN",
		Long:  `Reset KubeVPN if any error occurs`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := connect.InitClient(factory); err != nil {
				log.Fatal(err)
			}
			err := connect.Reset(cmd.Context())
			if err != nil {
				log.Fatal(err)
			}
			log.Infoln("done")
		},
	}
	return cmd
}
