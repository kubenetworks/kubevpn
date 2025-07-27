package cmds

import (
	"bytes"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/printers"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

func CmdConnection(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connection",
		Short: "Connection management",
	}
	cmd.AddCommand(cmdConnectionList(f))
	cmd.AddCommand(cmdConnectionUse(f))
	return cmd
}

func cmdConnectionList(f cmdutil.Factory) *cobra.Command {
	var sshConf = &pkgssh.SshConfig{}
	cmd := &cobra.Command{
		Use:     "list",
		Short:   i18n.T("List all connections"),
		Aliases: []string{"ls"},
		Long:    templates.LongDesc(i18n.T(`List all connections connected to cluster network`)),
		Example: templates.Examples(i18n.T(`
		# list all connections
		kubevpn connection ls
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &rpc.ConnectionListRequest{}
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			resp, err := cli.ConnectionList(cmd.Context(), req)
			if err != nil {
				return err
			}
			var sb = new(bytes.Buffer)
			w := printers.GetNewTabWriter(sb)
			genConnectMsg(w, resp.CurrentConnectionID, resp.List)
			_ = w.Flush()
			_, _ = fmt.Fprint(os.Stdout, sb.String())
			return nil
		},
	}
	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	return cmd
}

func cmdConnectionUse(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "use",
		Short: i18n.T("Use a specific connection"),
		Long: templates.LongDesc(i18n.T(`
		Use a specific connection.
		`)),
		Example: templates.Examples(i18n.T(`
		# use a specific connection, change current connection to special id, leave or unsync will use this connection
		kubevpn connection use xxx
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
		},
		Args: cobra.MatchAll(cobra.OnlyValidArgs, cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &rpc.ConnectionUseRequest{
				ConnectionID: args[0],
			}
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			_, err = cli.ConnectionUse(cmd.Context(), req)
			if err != nil {
				return err
			}
			return nil
		},
	}
	return cmd
}
