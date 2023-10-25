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
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", "ID", "Priority", "Context", "Status")
	status := "None"
	kubeContext := ""
	if svr.connect != nil {
		status = "Connected"
		kubeContext = svr.connect.GetKubeconfigContext()
	}
	_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", 0, "Main", kubeContext, status)

	for i, options := range svr.secondaryConnect {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", i+1, "Minor", options.GetKubeconfigContext(), "Connected")
	}
	_ = w.Flush()
	return &rpc.StatusResponse{Message: sb.String()}, nil
}
