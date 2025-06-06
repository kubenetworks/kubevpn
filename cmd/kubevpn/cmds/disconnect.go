package cmds

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdDisconnect(f cmdutil.Factory) *cobra.Command {
	var all = false
	var clusterIDs []string
	cmd := &cobra.Command{
		Use:   "disconnect",
		Short: i18n.T("Disconnect from kubernetes cluster network"),
		Long: templates.LongDesc(i18n.T(`
		Disconnect from kubernetes cluster network
		
		This command is to disconnect from cluster. after use command 'kubevpn connect',
		you can use this command to disconnect from a specific cluster. 
		before disconnect, it will leave proxy resource and clone resource if resource depends on this cluster 
		after disconnect it will also cleanup DNS and host
		`)),
		Example: templates.Examples(i18n.T(`
		# disconnect from cluster network and restore proxy resource
        kubevpn disconnect
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			plog.InitLoggerForClient()
			err = daemon.StartupDaemon(cmd.Context())
			return err
		},
		Args: cobra.MatchAll(cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && all {
				return fmt.Errorf("either specify --all or ID, not both")
			}
			if len(clusterIDs) > 0 && all {
				return fmt.Errorf("either specify --all or cluster-id, not both")
			}
			if len(args) == 0 && !all && len(clusterIDs) == 0 {
				return fmt.Errorf("either specify --all or ID or cluster-id")
			}
			var ids *int32
			if len(args) > 0 {
				integer, err := strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("invalid ID: %s: %v", args[0], err)
				}
				ids = pointer.Int32(int32(integer))
			}
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			req := &rpc.DisconnectRequest{
				ID:         ids,
				ClusterIDs: clusterIDs,
				All:        pointer.Bool(all),
			}
			resp, err := cli.Disconnect(context.Background())
			if err != nil {
				return err
			}
			err = resp.Send(req)
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.DisconnectResponse](cmd.Context(), resp)
			if err != nil {
				if status.Code(err) == codes.Canceled {
					return nil
				}
				return err
			}
			_, _ = fmt.Fprint(os.Stdout, "Disconnect completed")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", all, "Disconnect all cluster, disconnect from all cluster network")
	cmd.Flags().StringArrayVar(&clusterIDs, "cluster-id", []string{}, "Cluster id, command status -o yaml/json will show cluster-id")
	return cmd
}
