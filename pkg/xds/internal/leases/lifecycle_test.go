package leases

import (
	"context"
	"net"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func TestIntegration_CloseJoinsAllocationCallbackAndReaper(t *testing.T) {
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	exited := make(chan struct{})
	s, err := New(context.Background(), fake.NewSimpleClientset(), "test-ns", Callbacks{
		Sync: func(ctx context.Context, owner string, v4, v6 *net.IPNet) {
			if owner != "alice" || v4 == nil || v6 == nil {
				t.Error("callback lost committed allocation values")
			}
			entered <- ctx
			<-ctx.Done()
			<-release
			close(exited)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer close(release)
	s.StartLeaseReaper(context.Background())
	if _, err := s.GetTunIP(context.Background(), &Request{OwnerID: "alice"}); err != nil {
		t.Fatal(err)
	}
	var callbackCtx context.Context
	select {
	case callbackCtx = <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback did not start")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-callbackCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel callback")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before callback completed")
	default:
	}
	// Release via a separate channel owner so the deferred close remains single-use.
	release <- struct{}{}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join callback and reaper")
	}
	select {
	case <-exited:
	default:
		t.Fatal("callback still running after Close")
	}
}
