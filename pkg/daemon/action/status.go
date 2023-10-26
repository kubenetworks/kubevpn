package action

import (
	"bytes"
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Status(ctx context.Context, request *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	var sb = new(bytes.Buffer)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", "ID", "Priority", "Cluster", "Kubeconfig", "Namespace", "Status")
	if svr.connect != nil {
		status := "Connected"
		cluster := svr.connect.GetKubeconfigCluster()
		namespace := svr.connect.Namespace
		kubeconfig := svr.connect.OriginKubeconfigPath
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", 0, "Main", cluster, kubeconfig, namespace, status)
	}

	for i, options := range svr.secondaryConnect {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", i+1, "Minor", options.GetKubeconfigCluster(), options.OriginKubeconfigPath, options.Namespace, "Connected")
	}
	_ = w.Flush()
	return &rpc.StatusResponse{Message: sb.String()}, nil
}
