package cmds

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func init() {
	resetCmd.Flags().StringVar(&connect.KubeconfigPath, "kubeconfig", "", "kubeconfig")
	resetCmd.Flags().StringVarP(&connect.Namespace, "namespace", "n", "", "namespace")
	RootCmd.AddCommand(resetCmd)
}

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset KubeVPN",
	Long:  `Reset KubeVPN if any error occurs`,
	Run: func(cmd *cobra.Command, args []string) {
		if err := connect.InitClient(); err != nil {
			log.Fatal(err)
		}
		err := connect.Reset(cmd.Context())
		if err != nil {
			log.Fatal(err)
		}
		log.Infoln("done")
	},
}
