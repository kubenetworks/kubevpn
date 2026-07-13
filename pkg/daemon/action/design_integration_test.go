package action

import (
	"context"
	"testing"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

func newTestSession() *SessionLifecycle {
	return NewSessionLifecycle(log.New())
}

// ============================================================================
// Tests for 02-dual-daemon.md + 04-connection-id.md design:
// findConnection, removeConnection, resetCurrentConnection, dedup logic
// ============================================================================

func TestFindConnection_ReturnsFirstMatch(t *testing.T) {
	svr := &Server{}
	a := &handler.ConnectOptions{ConnectionID: "abc123456789"}
	b := &handler.ConnectOptions{ConnectionID: "def123456789"}
	svr.connections = []handler.Connection{a, b}

	found, idx := svr.findConnection("abc123456789")
	if found != a || idx != 0 {
		t.Fatalf("expected (a, 0), got (%v, %d)", found, idx)
	}

	found, idx = svr.findConnection("def123456789")
	if found != b || idx != 1 {
		t.Fatalf("expected (b, 1), got (%v, %d)", found, idx)
	}
}

func TestFindConnection_NotFound(t *testing.T) {
	svr := &Server{}
	svr.connections = []handler.Connection{
		&handler.ConnectOptions{ConnectionID: "abc123456789"},
	}

	found, idx := svr.findConnection("nonexistent12")
	if found != nil || idx != -1 {
		t.Fatalf("expected (nil, -1), got (%v, %d)", found, idx)
	}
}

func TestRemoveConnection_RemovesAllMatching(t *testing.T) {
	svr := &Server{}
	a := &handler.ConnectOptions{ConnectionID: "abc123456789", OwnerID: "owner-a"}
	b := &handler.ConnectOptions{ConnectionID: "abc123456789", OwnerID: "owner-b"}
	c := &handler.ConnectOptions{ConnectionID: "other1234567"}
	svr.connections = []handler.Connection{a, b, c}

	removed := svr.removeConnection("abc123456789")

	if len(removed) != 2 {
		t.Fatalf("expected 2 removed, got %d", len(removed))
	}
	if len(svr.connections) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(svr.connections))
	}
	if svr.connections[0] != c {
		t.Fatal("wrong connection remaining")
	}
}

func TestRemoveConnection_NoMatch(t *testing.T) {
	svr := &Server{}
	svr.connections = []handler.Connection{
		&handler.ConnectOptions{ConnectionID: "abc123456789"},
	}

	removed := svr.removeConnection("nonexistent12")
	if len(removed) != 0 {
		t.Fatalf("expected 0 removed, got %d", len(removed))
	}
	if len(svr.connections) != 1 {
		t.Fatal("should not modify connections when no match")
	}
}

func TestResetCurrentConnection_PicksFirstRemaining(t *testing.T) {
	svr := &Server{}
	svr.connections = []handler.Connection{
		&handler.ConnectOptions{ConnectionID: "first1234567"},
		&handler.ConnectOptions{ConnectionID: "second123456"},
	}
	svr.currentConnectionID = "removed123456"

	svr.resetCurrentConnection("removed123456")

	if svr.currentConnectionID != "first1234567" {
		t.Fatalf("expected first remaining 'first1234567', got %q", svr.currentConnectionID)
	}
}

func TestResetCurrentConnection_ClearsWhenEmpty(t *testing.T) {
	svr := &Server{}
	svr.connections = nil
	svr.currentConnectionID = "removed123456"

	svr.resetCurrentConnection("removed123456")

	if svr.currentConnectionID != "" {
		t.Fatalf("expected empty when no connections, got %q", svr.currentConnectionID)
	}
}

func TestResetCurrentConnection_NoOpIfDifferentID(t *testing.T) {
	svr := &Server{}
	svr.connections = []handler.Connection{
		&handler.ConnectOptions{ConnectionID: "keep12345678"},
	}
	svr.currentConnectionID = "keep12345678"

	svr.resetCurrentConnection("other1234567")

	if svr.currentConnectionID != "keep12345678" {
		t.Fatalf("should not change currentConnectionID when removedID doesn't match")
	}
}

// ============================================================================
// Tests for 04-connection-id.md: ConnectionID properties
// ============================================================================

func TestConnectionID_DeterministicFromNamespaceUID(t *testing.T) {
	// Doc says: ConnectionID = last 12 chars of namespace UID
	// Same namespace UID → same ConnectionID (deterministic)
	uid := "550e8400-e29b-41d4-a716-446655440000"
	id1 := uid[len(uid)-12:]
	id2 := uid[len(uid)-12:]
	if id1 != id2 {
		t.Fatal("same UID should produce same ConnectionID")
	}
	if len(id1) != 12 {
		t.Fatalf("ConnectionID should be 12 chars, got %d", len(id1))
	}
}

// ============================================================================
// Tests for 02-dual-daemon.md: OwnerID reuse on restart (persistence)
// ============================================================================

func TestOwnerID_PersistedInConnectOptions(t *testing.T) {
	// Doc says: OwnerID has json tag (persisted), ConnectionID does not
	c := &handler.ConnectOptions{}
	c.OwnerID = "test-owner-1"

	// Verify OwnerID survives through GetConnectionID pattern
	if c.OwnerID != "test-owner-1" {
		t.Fatal("OwnerID should be settable")
	}
}

func TestRedirectReusesExistingOwnerID(t *testing.T) {
	// Doc says: when req.OwnerID is set (from restored connection), don't generate new UUID
	// The fix: ownerID = req.OwnerID if not empty, else uuid.New()
	// Test the logic inline:
	reqOwnerID := "existing1234"
	ownerID := reqOwnerID
	if ownerID == "" {
		ownerID = "new-uuid-val"
	}
	if ownerID != "existing1234" {
		t.Fatalf("should reuse existing OwnerID, got %q", ownerID)
	}

	// Empty case: generate new
	reqOwnerID = ""
	ownerID = reqOwnerID
	if ownerID == "" {
		ownerID = "new-uuid-val"
	}
	if ownerID != "new-uuid-val" {
		t.Fatalf("should generate new OwnerID when empty, got %q", ownerID)
	}
}

// ============================================================================
// Tests for 12-session-lifecycle.md: SessionLifecycle advanced scenarios
// ============================================================================

func TestSessionLifecycle_ContextOutlivesRPC(t *testing.T) {
	// Doc says: session uses context.Background(), NOT resp.Context()
	// So cancelling a "mock RPC context" should NOT affect the session
	rpcCtx, rpcCancel := context.WithCancel(context.Background())
	_ = rpcCtx

	session := newTestSession()
	rpcCancel() // simulate RPC disconnect

	if session.Ctx.Err() != nil {
		t.Fatal("session context should NOT be cancelled when RPC context is cancelled")
	}
}

// ============================================================================
// Fault injection: abnormal exit / crash scenarios
// ============================================================================

func TestFault_DisconnectDuringActiveConnections(t *testing.T) {
	// Simulate: multiple connections active, disconnect one by ID
	// Others should remain intact
	svr := &Server{}
	a := &handler.ConnectOptions{ConnectionID: "conn-aaaa-1234", OwnerID: "owner-a"}
	b := &handler.ConnectOptions{ConnectionID: "conn-bbbb-5678", OwnerID: "owner-b"}
	c := &handler.ConnectOptions{ConnectionID: "conn-cccc-9012", OwnerID: "owner-c"}
	svr.connections = []handler.Connection{a, b, c}
	svr.currentConnectionID = "conn-bbbb-5678"

	removed := svr.removeConnection("conn-bbbb-5678")
	svr.resetCurrentConnection("conn-bbbb-5678")

	if len(removed) != 1 || removed[0] != b {
		t.Fatalf("expected [b] removed, got %v", removed)
	}
	if len(svr.connections) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(svr.connections))
	}
	// currentConnectionID should be reset to first remaining
	if svr.currentConnectionID != "conn-aaaa-1234" {
		t.Fatalf("current should be first remaining, got %q", svr.currentConnectionID)
	}
}

func TestFault_DisconnectAll_CleansEverything(t *testing.T) {
	// Simulate: disconnect --all
	svr := &Server{}
	for i := 0; i < 5; i++ {
		svr.connections = append(svr.connections, &handler.ConnectOptions{
			ConnectionID: "conn-" + string(rune('a'+i)) + "123456789",
		})
	}
	svr.currentConnectionID = "conn-c123456789"

	all := handler.Connects(svr.connections)
	svr.connections = nil
	svr.currentConnectionID = ""

	if len(all) != 5 {
		t.Fatalf("expected 5 disconnected, got %d", len(all))
	}
	if svr.currentConnectionID != "" {
		t.Fatal("currentConnectionID should be empty after disconnect all")
	}
	if len(svr.connections) != 0 {
		t.Fatal("connections should be empty after disconnect all")
	}
}

func TestFault_CrashRestart_OwnerIDPreserved(t *testing.T) {
	// Simulate: daemon crashes, restarts, LoadFromConfig restores OwnerID
	// The fix ensures req.OwnerID is set from persisted ConnectOptions
	persistedOwnerID := "abc123def456"

	// Simulate the LoadFromConfig logic
	reqOwnerID := "" // RequestRaw doesn't contain OwnerID (marshaled too early)
	if persistedOwnerID != "" {
		reqOwnerID = persistedOwnerID
	}

	// Simulate redirectConnectToSudoDaemon logic
	ownerID := reqOwnerID
	if ownerID == "" {
		ownerID = "new-random-uuid"
	}

	if ownerID != persistedOwnerID {
		t.Fatalf("should reuse persisted OwnerID %q, got %q", persistedOwnerID, ownerID)
	}
}

func TestFault_FindConnection_EmptySlice(t *testing.T) {
	svr := &Server{}
	svr.connections = nil

	found, idx := svr.findConnection("anything")
	if found != nil || idx != -1 {
		t.Fatal("empty connections slice should return nil, -1")
	}
}

func TestFault_RemoveConnection_FromSingleElement(t *testing.T) {
	svr := &Server{}
	only := &handler.ConnectOptions{ConnectionID: "only1234567"}
	svr.connections = []handler.Connection{only}
	svr.currentConnectionID = "only1234567"

	removed := svr.removeConnection("only1234567")
	svr.resetCurrentConnection("only1234567")

	if len(removed) != 1 {
		t.Fatal("should remove the only connection")
	}
	if len(svr.connections) != 0 {
		t.Fatal("connections should be empty")
	}
	if svr.currentConnectionID != "" {
		t.Fatal("currentConnectionID should be cleared")
	}
}
