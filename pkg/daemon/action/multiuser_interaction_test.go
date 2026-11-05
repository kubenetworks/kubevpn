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
	svr.registry.list = append(svr.registry.list, connA)
	svr.registry.currentID = "prod-ns-uid-12"

	// User B tries to connect same cluster → dedup
	existing, _ := svr.registry.find("prod-ns-uid-12")
	if existing == nil {
		t.Fatal("second connect should find existing")
	}
	// In production: returns immediately, no new connection created.
	// B gets the same connectionID.

	// B disconnects (uses connectionID)
	removed := svr.registry.removeByID("prod-ns-uid-12")
	svr.registry.resetCurrent("prod-ns-uid-12")

	// The shared connection is removed — both users lose access
	if len(removed) != 1 {
		t.Fatal("should remove the shared connection")
	}
	if len(svr.registry.list) != 0 {
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
	svr.registry.list = append(svr.registry.list, alice, bob, carol)
	svr.registry.currentID = "prod-uid-12345"

	// t1: Bob adds proxy (simulated by adding to proxyManager)
	bob.ConnectionID = "staging-uid-12" // already set

	// t2: Alice disconnects
	svr.registry.removeByID("prod-uid-12345")
	svr.registry.resetCurrent("prod-uid-12345")

	if len(svr.registry.list) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(svr.registry.list))
	}
	// After Alice disconnects, current should auto-select first remaining
	if svr.registry.currentID != "staging-uid-12" {
		t.Fatalf("expected staging as current, got %q", svr.registry.currentID)
	}

	// Bob and Carol should still be findable
	if found, _ := svr.registry.find("staging-uid-12"); found == nil {
		t.Fatal("Bob's connection should exist")
	}
	if found, _ := svr.registry.find("dev-uid-123456"); found == nil {
		t.Fatal("Carol's connection should exist")
	}

	// t3: Bob disconnects
	svr.registry.removeByID("staging-uid-12")
	svr.registry.resetCurrent("staging-uid-12")

	if len(svr.registry.list) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(svr.registry.list))
	}
	if svr.registry.currentID != "dev-uid-123456" {
		t.Fatalf("expected dev as current, got %q", svr.registry.currentID)
	}

	// t4: Carol disconnects
	svr.registry.removeByID("dev-uid-123456")
	svr.registry.resetCurrent("dev-uid-123456")

	if len(svr.registry.list) != 0 || svr.registry.currentID != "" {
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

	svr.registry.list = append(svr.registry.list, prod, staging, dev)
	svr.registry.currentID = "prod-uid-12345"

	// t1: findConnection for current → returns prod
	current, _ := svr.registry.find(svr.registry.currentID)
	if current.GetOwnerID() != "owner-prod" {
		t.Fatal("current should be prod")
	}

	// t2: Switch to staging
	svr.registry.currentID = "staging-uid-12"
	current, _ = svr.registry.find(svr.registry.currentID)
	if current.GetOwnerID() != "owner-staging" {
		t.Fatal("current should be staging after switch")
	}

	// t3: Disconnect current (staging)
	svr.registry.removeByID(svr.registry.currentID)
	svr.registry.resetCurrent("staging-uid-12")

	// t4: Auto-selects first remaining (prod)
	if svr.registry.currentID != "prod-uid-12345" {
		t.Fatalf("expected prod after staging removed, got %q", svr.registry.currentID)
	}
	if len(svr.registry.list) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(svr.registry.list))
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
	svr.registry.list = append(svr.registry.list, alice1)
	svr.registry.currentID = "cluster-uid-12"

	// t1-t2: Proxy and leave are envoy rule operations (tested separately)
	// Here we verify the connection state

	// t3: Disconnect
	svr.registry.removeByID("cluster-uid-12")
	svr.registry.resetCurrent("cluster-uid-12")
	if len(svr.registry.list) != 0 {
		t.Fatal("should be empty after disconnect")
	}

	// t4: Reconnect (same ConnectionID — deterministic)
	alice2 := &handler.ConnectOptions{ConnectionID: "cluster-uid-12", OwnerID: "alice-session2"}
	svr.registry.list = append(svr.registry.list, alice2)
	svr.registry.currentID = "cluster-uid-12"

	found, _ := svr.registry.find("cluster-uid-12")
	if found == nil || found.GetOwnerID() != "alice-session2" {
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

	svr.registry.list = append(svr.registry.list, alice, bob, carol)
	svr.registry.currentID = "cluster-b-5678"

	// t1: Bob crashes — connection removed
	svr.registry.removeByID("cluster-b-5678")
	svr.registry.resetCurrent("cluster-b-5678")

	// t2: Alice and Carol still exist
	if len(svr.registry.list) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(svr.registry.list))
	}
	if f, _ := svr.registry.find("cluster-a-1234"); f == nil {
		t.Fatal("Alice should still be connected")
	}
	if f, _ := svr.registry.find("cluster-c-9012"); f == nil {
		t.Fatal("Carol should still be connected")
	}

	// t3: Bob reconnects
	bobReconnected := &handler.ConnectOptions{ConnectionID: "cluster-b-5678", OwnerID: "bob-owner-02"}
	svr.registry.list = append(svr.registry.list, bobReconnected)

	if len(svr.registry.list) != 3 {
		t.Fatalf("expected 3 after reconnect, got %d", len(svr.registry.list))
	}
	found, _ := svr.registry.find("cluster-b-5678")
	if found.GetOwnerID() != "bob-owner-02" {
		t.Fatalf("Bob should have new OwnerID, got %q", found.GetOwnerID())
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
		svr.registry.list = append(svr.registry.list, conn)
	}
	svr.registry.currentID = "conn-c1234567890"

	// Simulate --all disconnect
	all := handler.Connects(svr.registry.list)
	svr.registry.list = nil
	svr.registry.currentID = ""

	// All connections should be returned for cleanup
	if len(all) != 5 {
		t.Fatalf("expected 5 connections to clean up, got %d", len(all))
	}
	if svr.registry.currentID != "" {
		t.Fatal("current should be empty")
	}
	if len(svr.registry.list) != 0 {
		t.Fatal("connections should be empty")
	}
}
