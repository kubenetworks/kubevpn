package cmds

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func CmdDisconnect(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "disconnect",
		Short:   i18n.T("Disconnect from kubernetes cluster network"),
		Long:    templates.LongDesc(i18n.T(`Disconnect from kubernetes cluster network`)),
		Example: templates.Examples(i18n.T(``)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			t := time.Now()
			err = daemon.StartupDaemon(cmd.Context())
			fmt.Printf("exec prerun use %s\n", time.Now().Sub(t).String())
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			now := time.Now()
			client, err := daemon.GetClient(false).Disconnect(
				cmd.Context(),
				&rpc.DisconnectRequest{},
			)
			fmt.Printf("call api disconnect use %s\n", time.Now().Sub(now).String())
			if err != nil {
				return err
			}
			var resp *rpc.DisconnectResponse
			for {
				resp, err = client.Recv()
				if err == io.EOF {
					return nil
				} else if err == nil {
					fmt.Fprint(os.Stdout, resp.Message)
				} else {
					return err
				}
			}
		},
	}
	return cmd
}
