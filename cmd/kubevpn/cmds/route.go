package cmds

import (
	"bytes"
	"fmt"
	"net"
	"os"

	"github.com/libp2p/go-netroute"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/printers"
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
	cmd.AddCommand(CmdRouteSearch(f))
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
			_, err = cli.Route(cmd.Context(), &rpc.RouteRequest{Cidr: cidr.String(), Type: rpc.RouteType_ROUTE_ADD})
			if err != nil {
				return err
			}
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
			_, err = cli.Route(cmd.Context(), &rpc.RouteRequest{Cidr: cidr.String(), Type: rpc.RouteType_ROUTE_DELETE})
			if err != nil {
				return err
			}
			return err
		},
	}
	return cmd
}

func CmdRouteSearch(cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search a specific route",
		Long: templates.LongDesc(i18n.T(`
		Search a specific route
		`)),
		Example: templates.Examples(i18n.T(`
		# Search a specific route
		kubevpn route search 198.19.0.1
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			plog.InitLoggerForClient()
			return daemon.StartupDaemon(cmd.Context())
		},
		Args: cobra.MatchAll(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst := net.ParseIP(args[0])
			if dst == nil {
				return fmt.Errorf("invalid ip: %s", args[0])
			}
			r, err := netroute.New()
			if err != nil {
				return err
			}
			iface, gateway, src, err := r.Route(dst)
			if err != nil {
				return err
			}
			var sb = new(bytes.Buffer)
			w := printers.GetNewTabWriter(sb)
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", "DESTINATION", "GATEWAY", "RT_IFA", "MTU", "NETIF")
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", dst, gateway.String(), src.String(), iface.MTU, iface.Name)
			_ = w.Flush()
			_, _ = fmt.Fprint(os.Stdout, sb.String())
			return nil
		},
	}
	return cmd
}
