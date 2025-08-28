package cmds

import (
	"fmt"
	"net"
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// CmdRoute
func CmdRoute(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Route table management",
	}
	cmd.AddCommand(CmdRouteAdd(f))
	cmd.AddCommand(CmdRouteDelete(f))
	return cmd
}

func CmdRouteAdd(cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a route",
		Long: templates.LongDesc(i18n.T(`
		Add a route
		`)),
		Example: templates.Examples(i18n.T(`
        # Add a route to current connection tun device
		kubevpn route add 198.19.0.1/32
		# Query current connection tun device
		kubevpn status
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			plog.InitLoggerForClient()
			return daemon.StartupDaemon(cmd.Context())
		},
		Args: cobra.MatchAll(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			_, cidr, err := net.ParseCIDR(args[0])
			if err != nil {
				return err
			}
			resp, err := cli.Route(cmd.Context(), &rpc.RouteRequest{Cidr: cidr.String(), Type: rpc.RouteType_ROUTE_ADD})
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(os.Stdout, resp.Message)
			return err
		},
	}
	return cmd
}

func CmdRouteDelete(cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a specific route",
		Long: templates.LongDesc(i18n.T(`
		Delete a specific route
		`)),
		Example: templates.Examples(i18n.T(`
		# Delete a specific route
		kubevpn route delete 198.19.0.1/32
		# Query current connection tun device
		kubevpn status
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			plog.InitLoggerForClient()
			return daemon.StartupDaemon(cmd.Context())
		},
		Args: cobra.MatchAll(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			_, cidr, err := net.ParseCIDR(args[0])
			if err != nil {
				return err
			}
			resp, err := cli.Route(cmd.Context(), &rpc.RouteRequest{Cidr: cidr.String(), Type: rpc.RouteType_ROUTE_DELETE})
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(os.Stdout, resp.Message)
			return err
		},
	}
	return cmd
}
