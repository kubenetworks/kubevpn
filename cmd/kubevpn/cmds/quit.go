package cmds

import (
	"io"
	defaultlog "log"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdQuit(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "quit",
		Short:   i18n.T("Quit to kubernetes cluster network"),
		Long:    templates.LongDesc(i18n.T(`Quit to kubernetes cluster network`)),
		Example: templates.Examples(i18n.T(``)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// startup daemon process and sudo process
			err = startupDaemon(cmd.Context())
			if err != nil {
				return err
			}
			util.InitLogger(config.Debug)
			defaultlog.Default().SetOutput(io.Discard)
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := daemon.GetClient(true).Quit(
				cmd.Context(),
				&rpc.QuitRequest{},
			)
			if err != nil {
				return err
			}
			var resp *rpc.QuitResponse
			for {
				resp, err = client.Recv()
				if err == io.EOF {
					break
				} else if err == nil {
					log.Println(resp.Message)
				} else {
					return err
				}
			}
			return nil
		},
	}
	return cmd
}
