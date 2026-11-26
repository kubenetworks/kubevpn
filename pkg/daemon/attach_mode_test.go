package daemon

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

type attachModeTestStream struct {
	rpc.Daemon_ProxyClient
	sent *rpc.ProxyRequest
}

func (s *attachModeTestStream) Send(req *rpc.ProxyRequest) error {
	s.sent = req
	return nil
}

func TestAttachModeProxyClient_MarksOnlyLegacyCommand(t *testing.T) {
	stream := &attachModeTestStream{}
	client := &attachModeProxyClient{Daemon_ProxyClient: stream}
	t.Setenv(LegacyProxyEnv, "true")
	if err := client.Send(&rpc.ProxyRequest{}); err != nil {
		t.Fatal(err)
	}
	if stream.sent.AttachMode != config.AttachModeLegacySidecar {
		t.Fatalf("attach mode = %q, want %q", stream.sent.AttachMode, config.AttachModeLegacySidecar)
	}
}
