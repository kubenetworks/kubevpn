package action

import (
	"bytes"
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	StatusOk     = "Connected"
	StatusFailed = "Disconnected"
)

func (svr *Server) Status(ctx context.Context, request *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	var sb = new(bytes.Buffer)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", "ID", "Mode", "Cluster", "Kubeconfig", "Namespace", "Status", "Netif")
	if svr.connect != nil {
		cluster := util.GetKubeconfigCluster(svr.connect.GetFactory())
		namespace := svr.connect.Namespace
		kubeconfig := svr.connect.OriginKubeconfigPath
		name, _ := svr.connect.GetTunDeviceName()
		status := StatusOk
		if name == "" {
			status = StatusFailed
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			0, "full", cluster, kubeconfig, namespace, status, name)
	}

	for i, options := range svr.secondaryConnect {
		cluster := util.GetKubeconfigCluster(options.GetFactory())
		name, _ := options.GetTunDeviceName()
		status := StatusOk
		if name == "" {
			status = StatusFailed
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			i+1, "lite", cluster, options.OriginKubeconfigPath, options.Namespace, status, name)
	}
	_ = w.Flush()
	return &rpc.StatusResponse{Message: sb.String()}, nil
}
