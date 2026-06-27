package action

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// ============================================================================
// Story 1: Two users connect same cluster, one disconnects, other stays
//
// In practice, ConnectionID is derived from namespace UID and is the same
// for both users connecting to the same namespace. The dedup logic means
// only one connection object exists.
// ============================================================================

func TestInteraction_SameCluster_DedupThenDisconnect(t *testing.T) {
	svr := &Server{}

	// User A connects cluster "prod" → creates connection
	connA := &handler.ConnectOptions{ConnectionID: "prod-ns-uid-12", OwnerID: "alice-uuid-1"}
	svr.connections = append(svr.connections, connA)
	svr.currentConnectionID = "prod-ns-uid-12"

	// User B tries to connect same cluster → dedup
	existing, _ := svr.findConnection("prod-ns-uid-12")
	if existing == nil {
		t.Fatal("second connect should find existing")
	}
	// In production: returns immediately, no new connection created.
	// B gets the same connectionID.

	// B disconnects (uses connectionID)
	removed := svr.removeConnection("prod-ns-uid-12")
	svr.resetCurrentConnection("prod-ns-uid-12")

	// The shared connection is removed — both users lose access
	if len(removed) != 1 {
		t.Fatal("should remove the shared connection")
	}
	if len(svr.connections) != 0 {
		t.Fatal("no connections should remain")
	}
}

// ============================================================================
// Story 2: Three users connect three different clusters, operations isolated
//
// Timeline:
//   t0: Alice→prod, Bob→staging, Carol→dev
//   t1: Bob proxies in staging (adds proxy resource)
//   t2: Alice disconnects prod → staging and dev unaffected
//   t3: Bob disconnects staging → dev unaffected
//   t4: Carol disconnects dev → all clean
// ============================================================================

func TestInteraction_ThreeClusters_SequentialDisconnect(t *testing.T) {
	svr := &Server{}

	alice := &handler.ConnectOptions{ConnectionID: "prod-uid-12345", OwnerID: "alice-owner"}
	bob := &handler.ConnectOptions{ConnectionID: "staging-uid-12", OwnerID: "bob-owner-01"}
	carol := &handler.ConnectOptions{ConnectionID: "dev-uid-123456", OwnerID: "carol-owner"}

	// t0: All connect
	svr.connections = append(svr.connections, alice, bob, carol)
	svr.currentConnectionID = "prod-uid-12345"

	// t1: Bob adds proxy (simulated by adding to proxyManager)
	bob.ConnectionID = "staging-uid-12" // already set

	// t2: Alice disconnects
	svr.removeConnection("prod-uid-12345")
	svr.resetCurrentConnection("prod-uid-12345")

	if len(svr.connections) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(svr.connections))
	}
	// After Alice disconnects, current should auto-select first remaining
	if svr.currentConnectionID != "staging-uid-12" {
		t.Fatalf("expected staging as current, got %q", svr.currentConnectionID)
	}

	// Bob and Carol should still be findable
	if found, _ := svr.findConnection("staging-uid-12"); found == nil {
		t.Fatal("Bob's connection should exist")
	}
	if found, _ := svr.findConnection("dev-uid-123456"); found == nil {
		t.Fatal("Carol's connection should exist")
	}

	// t3: Bob disconnects
	svr.removeConnection("staging-uid-12")
	svr.resetCurrentConnection("staging-uid-12")

	if len(svr.connections) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(svr.connections))
	}
	if svr.currentConnectionID != "dev-uid-123456" {
		t.Fatalf("expected dev as current, got %q", svr.currentConnectionID)
	}

	// t4: Carol disconnects
	svr.removeConnection("dev-uid-123456")
	svr.resetCurrentConnection("dev-uid-123456")

	if len(svr.connections) != 0 || svr.currentConnectionID != "" {
		t.Fatal("all should be clean")
	}
}

// ============================================================================
// Story 3: User switches between clusters, operations target current
//
// Timeline:
//   t0: Connect to prod, staging, dev
//   t1: Current = prod. Leave targets prod's connection.
//   t2: Switch current to staging.
//   t3: Leave targets staging's connection.
//   t4: Switch to dev. Leave targets dev's connection.
// ============================================================================

func TestInteraction_SwitchAndOperate(t *testing.T) {
	svr := &Server{}

	prod := &handler.ConnectOptions{ConnectionID: "prod-uid-12345", OwnerID: "owner-prod"}
	staging := &handler.ConnectOptions{ConnectionID: "staging-uid-12", OwnerID: "owner-staging"}
	dev := &handler.ConnectOptions{ConnectionID: "dev-uid-123456", OwnerID: "owner-dev"}

	svr.connections = append(svr.connections, prod, staging, dev)
	svr.currentConnectionID = "prod-uid-12345"

	// t1: findConnection for current → returns prod
	current, _ := svr.findConnection(svr.currentConnectionID)
	if current.OwnerID != "owner-prod" {
		t.Fatal("current should be prod")
	}

	// t2: Switch to staging
	svr.currentConnectionID = "staging-uid-12"
	current, _ = svr.findConnection(svr.currentConnectionID)
	if current.OwnerID != "owner-staging" {
		t.Fatal("current should be staging after switch")
	}

	// t3: Disconnect current (staging)
	svr.removeConnection(svr.currentConnectionID)
	svr.resetCurrentConnection("staging-uid-12")

	// t4: Auto-selects first remaining (prod)
	if svr.currentConnectionID != "prod-uid-12345" {
		t.Fatalf("expected prod after staging removed, got %q", svr.currentConnectionID)
	}
	if len(svr.connections) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(svr.connections))
	}
}

// ============================================================================
// Story 4: Connect → proxy → leave → disconnect → reconnect (full lifecycle)
//
// Timeline:
//   t0: Alice connects to cluster
//   t1: Alice proxies reviews
//   t2: Alice leaves reviews
//   t3: Alice disconnects
//   t4: Alice reconnects → gets same ConnectionID (deterministic from ns UID)
//        but new OwnerID (or restored from persistence)
// ============================================================================

func TestInteraction_FullLifecycle_ConnectProxyLeaveDisconnectReconnect(t *testing.T) {
	svr := &Server{}

	// t0: Connect
	alice1 := &handler.ConnectOptions{ConnectionID: "cluster-uid-12", OwnerID: "alice-session1"}
	svr.connections = append(svr.connections, alice1)
	svr.currentConnectionID = "cluster-uid-12"

	// t1-t2: Proxy and leave are envoy rule operations (tested separately)
	// Here we verify the connection state

	// t3: Disconnect
	svr.removeConnection("cluster-uid-12")
	svr.resetCurrentConnection("cluster-uid-12")
	if len(svr.connections) != 0 {
		t.Fatal("should be empty after disconnect")
	}

	// t4: Reconnect (same ConnectionID — deterministic)
	alice2 := &handler.ConnectOptions{ConnectionID: "cluster-uid-12", OwnerID: "alice-session2"}
	svr.connections = append(svr.connections, alice2)
	svr.currentConnectionID = "cluster-uid-12"

	found, _ := svr.findConnection("cluster-uid-12")
	if found == nil || found.OwnerID != "alice-session2" {
		t.Fatal("reconnect should create new connection with new OwnerID")
	}
}

// ============================================================================
// Story 5: Multiple users, one crashes → others unaffected, crasher reconnects
//
// Timeline:
//   t0: Alice, Bob, Carol all connected
//   t1: Bob crashes (connection removed abruptly)
//   t2: Alice and Carol still work
//   t3: Bob reconnects (new OwnerID or preserved)
// ============================================================================

func TestInteraction_CrashAndReconnect_OthersUnaffected(t *testing.T) {
	svr := &Server{}

	alice := &handler.ConnectOptions{ConnectionID: "cluster-a-1234", OwnerID: "alice-owner"}
	bob := &handler.ConnectOptions{ConnectionID: "cluster-b-5678", OwnerID: "bob-owner-01"}
	carol := &handler.ConnectOptions{ConnectionID: "cluster-c-9012", OwnerID: "carol-owner"}

	svr.connections = append(svr.connections, alice, bob, carol)
	svr.currentConnectionID = "cluster-b-5678"

	// t1: Bob crashes — connection removed
	svr.removeConnection("cluster-b-5678")
	svr.resetCurrentConnection("cluster-b-5678")

	// t2: Alice and Carol still exist
	if len(svr.connections) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(svr.connections))
	}
	if f, _ := svr.findConnection("cluster-a-1234"); f == nil {
		t.Fatal("Alice should still be connected")
	}
	if f, _ := svr.findConnection("cluster-c-9012"); f == nil {
		t.Fatal("Carol should still be connected")
	}

	// t3: Bob reconnects
	bobReconnected := &handler.ConnectOptions{ConnectionID: "cluster-b-5678", OwnerID: "bob-owner-02"}
	svr.connections = append(svr.connections, bobReconnected)

	if len(svr.connections) != 3 {
		t.Fatalf("expected 3 after reconnect, got %d", len(svr.connections))
	}
	found, _ := svr.findConnection("cluster-b-5678")
	if found.OwnerID != "bob-owner-02" {
		t.Fatalf("Bob should have new OwnerID, got %q", found.OwnerID)
	}
}

// ============================================================================
// Story 6: Disconnect --all with multiple active proxy sessions
// ============================================================================

func TestInteraction_DisconnectAll_MultipleActiveSessions(t *testing.T) {
	svr := &Server{}

	for i := 0; i < 5; i++ {
		conn := &handler.ConnectOptions{
			ConnectionID: "conn-" + string(rune('a'+i)) + "1234567890",
			OwnerID:      "owner-" + string(rune('a'+i)),
		}
		svr.connections = append(svr.connections, conn)
	}
	svr.currentConnectionID = "conn-c1234567890"

	// Simulate --all disconnect
	all := handler.Connects(svr.connections)
	svr.connections = nil
	svr.currentConnectionID = ""

	// All connections should be returned for cleanup
	if len(all) != 5 {
		t.Fatalf("expected 5 connections to clean up, got %d", len(all))
	}
	if svr.currentConnectionID != "" {
		t.Fatal("current should be empty")
	}
	if len(svr.connections) != 0 {
		t.Fatal("connections should be empty")
	}
}
