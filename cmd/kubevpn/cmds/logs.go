package cmds

import (
	"io"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func CmdLogs(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "logs",
		Short:   i18n.T("Logs to kubernetes cluster network"),
		Long:    templates.LongDesc(i18n.T(`Logs to kubernetes cluster network`)),
		Example: templates.Examples(i18n.T(``)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// startup daemon process and sudo process
			return startupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := daemon.GetClient(true).Logs(
				cmd.Context(),
				&rpc.LogRequest{},
			)
			if err != nil {
				return err
			}
			var resp *rpc.LogResponse
			for {
				resp, err = client.Recv()
				if err == io.EOF {
					break
				} else if err == nil {
					log.Print(resp.Message)
				} else {
					return err
				}
			}
			return nil
		},
	}
	return cmd
}
