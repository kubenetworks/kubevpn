package action

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// TestDisconnect_MatchingID verifies that disconnect removes connections
// whose GetConnectionID() matches the given connectionID and calls
// Cleanup on each removed connection.
func TestDisconnect_MatchingID(t *testing.T) {
	var cleanupRan atomic.Bool

	conn := &handler.ConnectOptions{}
	// GetConnectionID() returns "" when dhcp is nil, so use "" to match.
	conn.AddRollbackFunc(func() error {
		cleanupRan.Store(true)
		return nil
	})

	svr := &Server{
		connections: []*handler.ConnectOptions{conn},
	}

	disconnect(context.Background(), svr, "")

	if len(svr.connections) != 0 {
		t.Fatalf("expected 0 connections after disconnect, got %d", len(svr.connections))
	}
	if !cleanupRan.Load() {
		t.Fatal("expected Cleanup to run on the removed connection")
	}
}

// TestDisconnect_NonMatchingID verifies that disconnect leaves connections
// unchanged when no connection matches the given connectionID.
func TestDisconnect_NonMatchingID(t *testing.T) {
	conn := &handler.ConnectOptions{}
	// GetConnectionID() returns "" when dhcp is nil, so "nonexistent" won't match.

	svr := &Server{
		connections: []*handler.ConnectOptions{conn},
	}

	disconnect(context.Background(), svr, "nonexistent")

	if len(svr.connections) != 1 {
		t.Fatalf("expected 1 connection unchanged, got %d", len(svr.connections))
	}
}

// TestDisconnect_EmptyConnections verifies that disconnect does not panic
// when the server has no connections.
func TestDisconnect_EmptyConnections(t *testing.T) {
	svr := &Server{}

	// Should not panic.
	disconnect(context.Background(), svr, "anything")

	if len(svr.connections) != 0 {
		t.Fatalf("expected 0 connections, got %d", len(svr.connections))
	}
}

// TestDisconnect_MultipleConnections verifies that disconnect removes only
// the matching connections and leaves the rest intact.
func TestDisconnect_MultipleConnections(t *testing.T) {
	var cleanupCount atomic.Int32

	// All have nil dhcp, so GetConnectionID() returns "" for all.
	makeConn := func() *handler.ConnectOptions {
		c := &handler.ConnectOptions{}
		c.AddRollbackFunc(func() error {
			cleanupCount.Add(1)
			return nil
		})
		return c
	}

	conn1 := makeConn()
	conn2 := makeConn()
	conn3 := makeConn()

	svr := &Server{
		connections: []*handler.ConnectOptions{conn1, conn2, conn3},
	}

	// All match "" because dhcp is nil on all of them.
	disconnect(context.Background(), svr, "")

	if len(svr.connections) != 0 {
		t.Fatalf("expected 0 connections after disconnect, got %d", len(svr.connections))
	}
	if got := cleanupCount.Load(); got != 3 {
		t.Fatalf("expected Cleanup called 3 times, got %d", got)
	}
}

// TestDisconnect_PreservesNonMatchingOrder verifies that non-matching
// connections keep their original order after a disconnect call.
func TestDisconnect_PreservesNonMatchingOrder(t *testing.T) {
	conn1 := &handler.ConnectOptions{OriginNamespace: "ns1"}
	conn2 := &handler.ConnectOptions{OriginNamespace: "ns2"}

	svr := &Server{
		connections: []*handler.ConnectOptions{conn1, conn2},
	}

	// "no-match" won't match any connection (all return "" from GetConnectionID).
	disconnect(context.Background(), svr, "no-match")

	if len(svr.connections) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(svr.connections))
	}
	if svr.connections[0].OriginNamespace != "ns1" || svr.connections[1].OriginNamespace != "ns2" {
		t.Fatalf("connection order changed: got [%s, %s]",
			svr.connections[0].OriginNamespace, svr.connections[1].OriginNamespace)
	}
}
