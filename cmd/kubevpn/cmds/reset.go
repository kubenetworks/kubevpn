package cmds

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdReset(factory cmdutil.Factory) *cobra.Command {
	var connect = handler.ConnectOptions{}
	var sshConf = util.SshConfig{}
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset KubeVPN",
		Long:  `Reset KubeVPN if any error occurs`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := connect.InitClient(factory, cmd.Flags(), sshConf); err != nil {
				log.Fatal(err)
			}
			err := connect.Reset(cmd.Context())
			if err != nil {
				log.Fatal(err)
			}
			log.Infoln("done")
		},
	}

	// for ssh jumper host
	cmd.Flags().StringVar(&sshConf.Addr, "ssh-addr", "", "ssh connection string address to dial as <hostname>:<port>, eg: 127.0.0.1:22")
	cmd.Flags().StringVar(&sshConf.User, "ssh-username", "", "username for ssh")
	cmd.Flags().StringVar(&sshConf.Password, "ssh-password", "", "password for ssh")
	cmd.Flags().StringVar(&sshConf.Keyfile, "ssh-keyfile", "", "file with private key for SSH authentication")
	return cmd
}
