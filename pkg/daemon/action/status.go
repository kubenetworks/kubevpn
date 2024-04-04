package action

import (
	"bytes"
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Status(ctx context.Context, request *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	var sb = new(bytes.Buffer)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", "ID", "Mode", "Cluster", "Kubeconfig", "Namespace", "Status")
	if svr.connect != nil {
		status := "Connected"
		cluster := util.GetKubeconfigCluster(svr.connect.GetFactory())
		namespace := svr.connect.Namespace
		kubeconfig := svr.connect.OriginKubeconfigPath
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", 0, "full", cluster, kubeconfig, namespace, status)
	}

	for i, options := range svr.secondaryConnect {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", i+1, "lite", util.GetKubeconfigCluster(options.GetFactory()), options.OriginKubeconfigPath, options.Namespace, "Connected")
	}
	_ = w.Flush()
	return &rpc.StatusResponse{Message: sb.String()}, nil
}
