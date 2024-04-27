package cmds

import (
	"fmt"
	"io"
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
)

func CmdDisconnect(f cmdutil.Factory) *cobra.Command {
	var all = false
	var clusterIDs []string
	cmd := &cobra.Command{
		Use:   "disconnect",
		Short: i18n.T("Disconnect from kubernetes cluster network"),
		Long:  templates.LongDesc(i18n.T(`Disconnect from kubernetes cluster network`)),
		Example: templates.Examples(i18n.T(`
		# disconnect from cluster network and restore proxy resource
        kubevpn disconnect
`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
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
			client, err := daemon.GetClient(false).Disconnect(
				cmd.Context(),
				&rpc.DisconnectRequest{
					ID:         ids,
					ClusterIDs: clusterIDs,
					All:        pointer.Bool(all),
				},
			)
			var resp *rpc.DisconnectResponse
			for {
				resp, err = client.Recv()
				if err == io.EOF {
					break
				} else if err == nil {
					fmt.Fprint(os.Stdout, resp.Message)
				} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
					break
				} else {
					return err
				}
			}
			fmt.Fprint(os.Stdout, "disconnect successfully")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", all, "Disconnect all cluster, disconnect from all cluster network")
	cmd.Flags().StringArrayVar(&clusterIDs, "cluster-id", []string{}, "Cluster id, command status -o yaml/json will show cluster-id")
	return cmd
}
