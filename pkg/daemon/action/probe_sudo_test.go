package action

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// statusErrClient is a DaemonClient whose Status always fails with a fixed error.
type statusErrClient struct {
	rpc.DaemonClient
	err error
}

func (c *statusErrClient) Status(context.Context, *rpc.StatusRequest, ...grpc.CallOption) (*rpc.StatusResponse, error) {
	return nil, c.err
}

// statusOKClient is a DaemonClient whose Status returns a fixed response.
type statusOKClient struct {
	rpc.DaemonClient
	resp *rpc.StatusResponse
}

func (c *statusOKClient) Status(context.Context, *rpc.StatusRequest, ...grpc.CallOption) (*rpc.StatusResponse, error) {
	return c.resp, nil
}

// TestProbeSudo_DaemonNotRunning_SurfacesError locks B3: when the sudo daemon is genuinely
// gone (GetClient fails with ErrDaemonNotRunning), probeSudo surfaces that error instead of
// swallowing it to nil, so callers can report "disconnected" rather than a silent blank.
func TestProbeSudo_DaemonNotRunning_SurfacesError(t *testing.T) {
	wantErr := fmt.Errorf("stat: %w", config.ErrDaemonNotRunning)
	svr := &Server{GetClient: func(bool) (rpc.DaemonClient, error) { return nil, wantErr }}

	m, err := svr.probeSudo(context.Background())
	if m != nil {
		t.Fatalf("expected nil map, got %v", m)
	}
	if !errors.Is(err, config.ErrDaemonNotRunning) {
		t.Fatalf("expected error to wrap ErrDaemonNotRunning, got %v", err)
	}
}

// TestProbeSudo_Unavailable_InvalidatesClient locks B2: a connection-level failure
// (codes.Unavailable, e.g. the sudo daemon crashed/restarted) drops the cached client so the
// next probe redials and re-runs the health/version gate.
func TestProbeSudo_Unavailable_InvalidatesClient(t *testing.T) {
	var invalidatedSudo bool
	svr := &Server{
		GetClient: func(bool) (rpc.DaemonClient, error) {
			return &statusErrClient{err: status.Error(codes.Unavailable, "connection refused")}, nil
		},
		InvalidateClient: func(isSudo bool) {
			if isSudo {
				invalidatedSudo = true
			}
		},
	}

	if _, err := svr.probeSudo(context.Background()); err == nil {
		t.Fatal("expected an error on Unavailable")
	}
	if !invalidatedSudo {
		t.Fatal("expected InvalidateClient(true) to be called on codes.Unavailable")
	}
}

// TestProbeSudo_NonUnavailable_KeepsClient ensures a transient non-connection error (e.g. a
// timeout / DeadlineExceeded from a slow-but-alive daemon) does NOT drop the cached client —
// only connection-level failures do.
func TestProbeSudo_NonUnavailable_KeepsClient(t *testing.T) {
	var invalidated bool
	svr := &Server{
		GetClient: func(bool) (rpc.DaemonClient, error) {
			return &statusErrClient{err: status.Error(codes.DeadlineExceeded, "slow")}, nil
		},
		InvalidateClient: func(bool) { invalidated = true },
	}

	if _, err := svr.probeSudo(context.Background()); err == nil {
		t.Fatal("expected an error on DeadlineExceeded")
	}
	if invalidated {
		t.Fatal("must not invalidate the cached client on a non-Unavailable error")
	}
}

// TestProbeSudo_Success_ReturnsStateMap verifies the happy path maps ConnectionID → data-plane
// state (TUN IPs + status verdict) the sudo daemon computed.
func TestProbeSudo_Success_ReturnsStateMap(t *testing.T) {
	resp := &rpc.StatusResponse{List: []*rpc.Status{
		{ConnectionID: "c1", IPv4: "198.19.0.1", IPv6: "2001:2::1", Status: StatusOk},
	}}
	svr := &Server{GetClient: func(bool) (rpc.DaemonClient, error) { return &statusOKClient{resp: resp}, nil }}

	m, err := svr.probeSudo(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := m["c1"]
	if !ok {
		t.Fatalf("expected connection c1 in map, got %+v", m)
	}
	if got.v4 != "198.19.0.1" || got.v6 != "2001:2::1" || got.status != StatusOk {
		t.Fatalf("unexpected state for c1: %+v", got)
	}
}
