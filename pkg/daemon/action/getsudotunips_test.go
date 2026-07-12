package action

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// blockingStatusClient simulates a wedged sudo daemon whose Status never returns until the
// caller's context is cancelled (e.g. its own read path is blocked on an unreachable cluster).
type blockingStatusClient struct {
	rpc.DaemonClient
}

func (b *blockingStatusClient) Status(ctx context.Context, _ *rpc.StatusRequest, _ ...grpc.CallOption) (*rpc.StatusResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestGetSudoTunIPs_BoundedByTimeout verifies the user daemon's cross-daemon Status hop is
// bounded by config.SudoStatusTimeout, so a wedged sudo daemon cannot stall an otherwise
// local command (the reported ~4.6s `kubevpn status` symptom). On timeout it returns nil.
func TestGetSudoTunIPs_BoundedByTimeout(t *testing.T) {
	orig := config.SudoStatusTimeout
	config.SudoStatusTimeout = 150 * time.Millisecond
	defer func() { config.SudoStatusTimeout = orig }()

	svr := &Server{
		GetClient: func(isSudo bool) (rpc.DaemonClient, error) { return &blockingStatusClient{}, nil },
	}

	done := make(chan map[string]tunIP, 1)
	start := time.Now()
	go func() { done <- svr.getSudoTunIPs(context.Background()) }()

	select {
	case m := <-done:
		if m != nil {
			t.Fatalf("expected nil map on timeout, got %v", m)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("getSudoTunIPs not bounded by SudoStatusTimeout: took %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("getSudoTunIPs did not return within 2s — the timeout bound is not applied")
	}
}
