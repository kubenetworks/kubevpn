package action

import (
	"context"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func TestIdentify_ReturnsID(t *testing.T) {
	svr := &Server{ID: "daemon-abc123"}
	resp, err := svr.Identify(context.Background(), &rpc.IdentifyRequest{})
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if resp.ID != "daemon-abc123" {
		t.Errorf("expected ID 'daemon-abc123', got %q", resp.ID)
	}
}

func TestIdentify_EmptyID(t *testing.T) {
	svr := &Server{}
	resp, err := svr.Identify(context.Background(), &rpc.IdentifyRequest{})
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if resp.ID != "" {
		t.Errorf("expected empty ID, got %q", resp.ID)
	}
}
