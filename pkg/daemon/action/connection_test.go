package action

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// TestCleanupConnection_NilConn verifies that calling cleanupConnection
// with a nil *ConnectOptions does not panic.
func TestCleanupConnection_NilConn(t *testing.T) {
	// Should not panic.
	cleanupConnection(context.Background(), nil)
}

// TestCleanupConnection_NoSync verifies that Cleanup is called on
// a ConnectOptions whose Sync field is nil.
func TestCleanupConnection_NoSync(t *testing.T) {
	var cleanupRan atomic.Bool
	conn := &handler.ConnectOptions{}
	conn.AddRollbackFunc(func() error {
		cleanupRan.Store(true)
		return nil
	})

	cleanupConnection(context.Background(), conn)

	if !cleanupRan.Load() {
		t.Fatal("expected ConnectOptions.Cleanup to run (rollback func was not called)")
	}
}

// TestCleanupConnection_WithSync verifies that both Sync.Cleanup and
// ConnectOptions.Cleanup are called when Sync is non-nil.
func TestCleanupConnection_WithSync(t *testing.T) {
	var connCleanupRan atomic.Bool
	var syncCleanupRan atomic.Bool

	syncOpts := &handler.SyncOptions{}
	syncOpts.AddRollbackFunc(func() error {
		syncCleanupRan.Store(true)
		return nil
	})

	conn := &handler.ConnectOptions{
		Sync: syncOpts,
	}
	conn.AddRollbackFunc(func() error {
		connCleanupRan.Store(true)
		return nil
	})

	cleanupConnection(context.Background(), conn)

	if !syncCleanupRan.Load() {
		t.Fatal("expected SyncOptions.Cleanup to run (sync rollback func was not called)")
	}
	if !connCleanupRan.Load() {
		t.Fatal("expected ConnectOptions.Cleanup to run (conn rollback func was not called)")
	}
}
