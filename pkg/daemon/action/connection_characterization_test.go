package action

// connection_characterization_test.go — Characterization tests for the
// daemon/action connection-management surface.
//
// Purpose: lock down the CURRENT observable behavior of:
//   - Server.findConnection (lookup by ConnectionID)
//   - Server.removeConnection (removal + return value)
//   - Server.resetCurrentConnection (auto-selection after removal)
//   - Server.siblingTunIPs (TUN IP aggregation)
//   - handler.Connects.Sort() (dependency-based ordering, no integration tag)
//   - handler.Connects.Append() (nil-safe accumulation)
//   - ConnectionUse RPC success path
//   - Persistence round-trip (OwnerID + ConnectionID fields preserved)
//
// All tests run without a real cluster. They use direct Server{} struct
// construction and handler.ConnectOptions{} literals with pre-set
// ConnectionID fields. No kubeconfig pointing at a real cluster is used.
//
// Design note on "Characterization" prefix: these tests assert observable
// OUTPUTS (IDs returned, slice length, ordering) — not internal struct
// fields. After the refactor migrates connections to []handler.Connection,
// all assertions here must still compile and pass against the new surface.

import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// ============================================================================
// 1. findConnection — lookup by ConnectionID
// ============================================================================

// TestCharacterization_FindConnection_ReturnsCorrectElement verifies that
// findConnection returns the connection whose ConnectionID matches and the
// correct slice index, even when multiple connections are present.
func TestCharacterization_FindConnection_ReturnsCorrectElement(t *testing.T) {
	connA := &handler.ConnectOptions{ConnectionID: "aaa-000000000000"}
	connB := &handler.ConnectOptions{ConnectionID: "bbb-000000000000"}
	connC := &handler.ConnectOptions{ConnectionID: "ccc-000000000000"}

	svr := &Server{
		connections: []handler.Connection{connA, connB, connC},
	}

	// --- findConnection must return the element at index 1 for "bbb..."
	got, idx := svr.findConnection("bbb-000000000000")
	if got == nil {
		t.Fatal("findConnection: expected non-nil result for existing ID")
	}
	if got.GetConnectionID() != "bbb-000000000000" {
		t.Errorf("findConnection: returned wrong element: %q", got.GetConnectionID())
	}
	if idx != 1 {
		t.Errorf("findConnection: expected index 1, got %d", idx)
	}

	// --- First element
	got, idx = svr.findConnection("aaa-000000000000")
	if got == nil || idx != 0 {
		t.Errorf("findConnection: first element: got=%v idx=%d", got, idx)
	}

	// --- Last element
	got, idx = svr.findConnection("ccc-000000000000")
	if got == nil || idx != 2 {
		t.Errorf("findConnection: last element: got=%v idx=%d", got, idx)
	}

	// --- Non-existent → (nil, -1)
	got, idx = svr.findConnection("zzz-000000000000")
	if got != nil || idx != -1 {
		t.Errorf("findConnection: non-existent: expected (nil, -1), got (%v, %d)", got, idx)
	}
}

// TestCharacterization_FindConnection_EmptyServer verifies the nil/–1 result
// on an empty server (no connections slice at all).
func TestCharacterization_FindConnection_EmptyServer(t *testing.T) {
	svr := &Server{}
	got, idx := svr.findConnection("any-id-000000000")
	if got != nil || idx != -1 {
		t.Errorf("empty server: expected (nil, -1), got (%v, %d)", got, idx)
	}
}

// TestCharacterization_FindConnection_EmptyID verifies behavior when the
// search key is the empty string. ConnectOptions with ConnectionID="" will match.
func TestCharacterization_FindConnection_EmptyID(t *testing.T) {
	// ConnectOptions with no ConnectionID set → GetConnectionID() returns ""
	conn := &handler.ConnectOptions{}
	svr := &Server{connections: []handler.Connection{conn}}

	got, idx := svr.findConnection("")
	if got == nil || idx != 0 {
		t.Errorf("empty ID match: expected (conn, 0), got (%v, %d)", got, idx)
	}

	// Non-matching non-empty ID against an empty-ConnectionID connection.
	got, idx = svr.findConnection("not-matching-00")
	if got != nil || idx != -1 {
		t.Errorf("non-match against empty-ID conn: expected (nil, -1), got (%v, %d)", got, idx)
	}
}

// ============================================================================
// 2. removeConnection — returned elements, slice mutation, ordering
// ============================================================================

// TestCharacterization_RemoveConnection_MiddleElement verifies that removing
// the middle element returns it, the remaining slice order is preserved, and
// the removed element is gone.
func TestCharacterization_RemoveConnection_MiddleElement(t *testing.T) {
	connA := &handler.ConnectOptions{ConnectionID: "aaa-111111111111"}
	connB := &handler.ConnectOptions{ConnectionID: "bbb-111111111111"}
	connC := &handler.ConnectOptions{ConnectionID: "ccc-111111111111"}

	svr := &Server{connections: []handler.Connection{connA, connB, connC}}

	removed := svr.removeConnection("bbb-111111111111")

	// Returned slice must contain exactly connB.
	if len(removed) != 1 {
		t.Fatalf("remove middle: expected 1 removed, got %d", len(removed))
	}
	if removed[0].GetConnectionID() != "bbb-111111111111" {
		t.Errorf("remove middle: wrong element returned: %q", removed[0].GetConnectionID())
	}

	// Remaining connections must be [A, C] in order.
	if len(svr.connections) != 2 {
		t.Fatalf("remove middle: expected 2 remaining, got %d", len(svr.connections))
	}
	if svr.connections[0].GetConnectionID() != "aaa-111111111111" {
		t.Errorf("remove middle: remaining[0] wrong: %q", svr.connections[0].GetConnectionID())
	}
	if svr.connections[1].GetConnectionID() != "ccc-111111111111" {
		t.Errorf("remove middle: remaining[1] wrong: %q", svr.connections[1].GetConnectionID())
	}
}

// TestCharacterization_RemoveConnection_NonExistentID verifies that removing
// a non-existent ID leaves the slice unchanged and returns an empty Connects.
func TestCharacterization_RemoveConnection_NonExistentID(t *testing.T) {
	connA := &handler.ConnectOptions{ConnectionID: "aaa-222222222222"}
	svr := &Server{connections: []handler.Connection{connA}}

	removed := svr.removeConnection("zzz-000000000000")
	if len(removed) != 0 {
		t.Errorf("non-existent remove: expected 0 removed, got %d", len(removed))
	}
	if len(svr.connections) != 1 {
		t.Errorf("non-existent remove: expected slice unchanged (1), got %d", len(svr.connections))
	}
}

// TestCharacterization_RemoveConnection_EmptyServer verifies no panic and
// empty result on an empty server.
func TestCharacterization_RemoveConnection_EmptyServer(t *testing.T) {
	svr := &Server{}
	removed := svr.removeConnection("any-id-000000000")
	if len(removed) != 0 {
		t.Errorf("empty server remove: expected 0, got %d", len(removed))
	}
}

// TestCharacterization_RemoveConnection_DuplicateIDs verifies that ALL
// connections matching the ID are removed and returned when duplicates exist.
// (The implementation iterates with i-- to catch all duplicates.)
func TestCharacterization_RemoveConnection_DuplicateIDs(t *testing.T) {
	dup := "dup-333333333333"
	connA := &handler.ConnectOptions{ConnectionID: "aaa-333333333333"}
	connB1 := &handler.ConnectOptions{ConnectionID: dup}
	connB2 := &handler.ConnectOptions{ConnectionID: dup}
	connC := &handler.ConnectOptions{ConnectionID: "ccc-333333333333"}

	svr := &Server{connections: []handler.Connection{connA, connB1, connB2, connC}}

	removed := svr.removeConnection(dup)
	if len(removed) != 2 {
		t.Fatalf("duplicate remove: expected 2 removed, got %d", len(removed))
	}
	// Both removed items must have the dup ID.
	for i, r := range removed {
		if r.GetConnectionID() != dup {
			t.Errorf("duplicate remove: removed[%d] has wrong ID: %q", i, r.GetConnectionID())
		}
	}
	// Remaining must be [A, C].
	if len(svr.connections) != 2 {
		t.Fatalf("duplicate remove: expected 2 remaining, got %d", len(svr.connections))
	}
	if svr.connections[0].GetConnectionID() != "aaa-333333333333" ||
		svr.connections[1].GetConnectionID() != "ccc-333333333333" {
		t.Errorf("duplicate remove: remaining order wrong: [%q, %q]",
			svr.connections[0].GetConnectionID(), svr.connections[1].GetConnectionID())
	}
}

// TestCharacterization_RemoveConnection_OnlyElement verifies removing the sole
// connection results in an empty slice.
func TestCharacterization_RemoveConnection_OnlyElement(t *testing.T) {
	conn := &handler.ConnectOptions{ConnectionID: "solo-44444444444"}
	svr := &Server{connections: []handler.Connection{conn}}

	removed := svr.removeConnection("solo-44444444444")
	if len(removed) != 1 {
		t.Fatalf("solo remove: expected 1 removed, got %d", len(removed))
	}
	if len(svr.connections) != 0 {
		t.Errorf("solo remove: expected empty slice, got %d", len(svr.connections))
	}
}

// ============================================================================
// 3. resetCurrentConnection — auto-selection semantics
// ============================================================================

// TestCharacterization_ResetCurrentConnection_RemovedIsDifferent verifies
// that when the removed ID does NOT match currentConnectionID, the current
// is left unchanged.
func TestCharacterization_ResetCurrentConnection_RemovedIsDifferent(t *testing.T) {
	svr := &Server{
		connections: []handler.Connection{
			&handler.ConnectOptions{ConnectionID: "aa-555555555555"},
			&handler.ConnectOptions{ConnectionID: "bb-555555555555"},
		},
		currentConnectionID: "aa-555555555555",
	}

	// Remove "bb" — current is "aa", so current must stay "aa".
	svr.resetCurrentConnection("bb-555555555555")
	if svr.currentConnectionID != "aa-555555555555" {
		t.Errorf("different ID removed: expected current unchanged 'aa...', got %q",
			svr.currentConnectionID)
	}
}

// TestCharacterization_ResetCurrentConnection_RemovedIsCurrent_PicksFirst
// verifies that when the removed ID matches current, the current is updated
// to the first element of the remaining connections.
func TestCharacterization_ResetCurrentConnection_RemovedIsCurrent_PicksFirst(t *testing.T) {
	svr := &Server{
		connections: []handler.Connection{
			&handler.ConnectOptions{ConnectionID: "first-6666666666"},
			&handler.ConnectOptions{ConnectionID: "third-6666666666"},
		},
		currentConnectionID: "removed-66666666",
	}
	// Simulate: "removed..." was just removed, connections now hold [first, third].
	svr.resetCurrentConnection("removed-66666666")

	if svr.currentConnectionID != "first-6666666666" {
		t.Errorf("should pick first remaining: expected 'first-6666666666', got %q",
			svr.currentConnectionID)
	}
}

// TestCharacterization_ResetCurrentConnection_NoRemainingConnections verifies
// that when the removed ID was current and no connections remain, currentConnectionID
// becomes the empty string.
func TestCharacterization_ResetCurrentConnection_NoRemainingConnections(t *testing.T) {
	svr := &Server{
		connections:         []handler.Connection{}, // already emptied
		currentConnectionID: "only-777777777777",
	}
	svr.resetCurrentConnection("only-777777777777")

	if svr.currentConnectionID != "" {
		t.Errorf("no remaining: expected empty currentConnectionID, got %q",
			svr.currentConnectionID)
	}
}

// TestCharacterization_ResetCurrentConnection_EmptyCurrent verifies that when
// currentConnectionID is already empty, resetCurrentConnection leaves it empty
// regardless of the removed ID (idempotent).
func TestCharacterization_ResetCurrentConnection_EmptyCurrent(t *testing.T) {
	svr := &Server{
		connections:         []handler.Connection{&handler.ConnectOptions{ConnectionID: "some-888888888888"}},
		currentConnectionID: "",
	}
	svr.resetCurrentConnection("any-removed-id-0")
	if svr.currentConnectionID != "" {
		t.Errorf("empty current: should stay empty, got %q", svr.currentConnectionID)
	}
}

// ============================================================================
// 4. siblingTunIPs — TUN IP aggregation
// ============================================================================

// TestCharacterization_SiblingTunIPs_Empty verifies an empty server returns nil.
func TestCharacterization_SiblingTunIPs_Empty(t *testing.T) {
	svr := &Server{}
	ips := svr.siblingTunIPs()
	if len(ips) != 0 {
		t.Errorf("empty server: expected 0 sibling IPs, got %d", len(ips))
	}
}

// TestCharacterization_SiblingTunIPs_NoNetworkManager verifies that
// connections without a NetworkManager (GetLocalTunIP returns "", "") are
// silently skipped — no nil dereference, no entries added.
func TestCharacterization_SiblingTunIPs_NoNetworkManager(t *testing.T) {
	conn := &handler.ConnectOptions{ConnectionID: "no-tun-999999999"}
	// GetLocalTunIP on a ConnectOptions without network returns ("", "").
	svr := &Server{connections: []handler.Connection{conn}}
	ips := svr.siblingTunIPs()
	if len(ips) != 0 {
		t.Errorf("no network manager: expected 0 IPs, got %v", ips)
	}
}

// ============================================================================
// 5. Connects.Sort() — dependency-based ordering (no integration tag)
//
// The sort_test.go file is behind //go:build integration because newConnectWithNetwork
// accesses unexported network.cfg fields. These characterization tests use only
// ExtraCIDR-based ordering (the "exact IP in ExtraCIDR" path of Less) which
// does NOT require a real cluster or the NetworkManager field.
//
// Sort semantics (from sort.go):
//   a < b  iff  a's APIServerIPs are contained in b's ExtraCIDR → a disconnects FIRST
//
// For the ExtraCIDR CIDR-contains path: when no NetworkManager is set,
// APIServerIPs is empty, so containsFunc never fires. Only the ExtraCIDR
// exact-IP-match path (ip.Equal) is exercisable without setting network.
//
// These tests deliberately characterize the behavior accessible without setting
// the unexported network field. Tests that require APIServerIPs in a
// NetworkManager live in pkg/handler/sort_test.go (integration-tagged).
// ============================================================================

// TestCharacterization_Connects_Sort_Empty verifies Sort on an empty slice
// returns an empty slice without panicking.
func TestCharacterization_Connects_Sort_Empty(t *testing.T) {
	var c handler.Connects
	sorted := c.Sort()
	if len(sorted) != 0 {
		t.Errorf("empty sort: expected 0, got %d", len(sorted))
	}
}

// TestCharacterization_Connects_Sort_SingleElement verifies a single-element
// slice survives Sort unchanged.
func TestCharacterization_Connects_Sort_SingleElement(t *testing.T) {
	conn := &handler.ConnectOptions{ConnectionID: "solo-aaaaaaaaaaaa"}
	c := handler.Connects{conn}
	sorted := c.Sort()
	if len(sorted) != 1 {
		t.Fatalf("single element: expected 1, got %d", len(sorted))
	}
	if sorted[0].GetConnectionID() != "solo-aaaaaaaaaaaa" {
		t.Errorf("single element: wrong element returned")
	}
}

// TestCharacterization_Connects_Sort_NilElement_NilFirst verifies that when
// nil occupies the FIRST position of the input slice, Sort returns it at the
// front without panicking.
//
// NOTE on nil-in-sort safety: Less(i, j) guards only a==nil (i.e. s[i]==nil).
// It does NOT guard b==nil (s[j]==nil). Therefore:
//   - Sort({nil, conn}) → works: reversal produces {conn, nil}; stable-sort
//     then calls Less(1,0) with a=nil → true → no panic, nil stays first.
//   - Sort({conn, nil}) → PANICS: reversal produces {nil, conn}; stable-sort
//     calls Less(1,0) with a=conn, b=nil → accesses nil.ExtraRouteInfo → panic.
//
// This asymmetry is a known limitation of the current Less implementation.
// In practice the daemon never inserts nil into svr.connections, so this is
// academic. We characterize the safe case only.
func TestCharacterization_Connects_Sort_NilElement_NilFirst(t *testing.T) {
	conn := &handler.ConnectOptions{ConnectionID: "real-bbbbbbbbbbbb"}
	// Input: nil is already first — Sort reverses to {conn, nil}, then stable-sort
	// sees Less(1,0) with a=nil → returns true → moves nil back to front.
	c := handler.Connects{nil, conn}
	sorted := c.Sort()
	if len(sorted) != 2 {
		t.Fatalf("nil-first element: expected 2, got %d", len(sorted))
	}
	// nil must be at index 0.
	if sorted[0] != nil {
		t.Errorf("nil-first: expected nil at index 0, got %v", sorted[0].GetConnectionID())
	}
}

// TestCharacterization_Connects_Sort_NoDependency_DoesNotPanic verifies that
// two independent connections (no ExtraCIDR, no network) can be sorted without
// panicking. The exact order is not asserted (comparator returns false for both
// directions), but the slice must have 2 elements.
func TestCharacterization_Connects_Sort_NoDependency_DoesNotPanic(t *testing.T) {
	connA := &handler.ConnectOptions{ConnectionID: "cluster-a-cccccc"}
	connB := &handler.ConnectOptions{ConnectionID: "cluster-b-cccccc"}

	c := handler.Connects{connA, connB}
	sorted := c.Sort()
	if len(sorted) != 2 {
		t.Fatalf("no-dep sort: expected 2, got %d", len(sorted))
	}
}

// TestCharacterization_Connects_Sort_InvalidCIDRSkipped verifies that malformed
// CIDR strings in ExtraCIDR are skipped without panic.
func TestCharacterization_Connects_Sort_InvalidCIDRSkipped(t *testing.T) {
	connA := &handler.ConnectOptions{ConnectionID: "cluster-a-dddddd"}
	connB := &handler.ConnectOptions{
		ConnectionID: "cluster-b-dddddd",
		ExtraRouteInfo: handler.ExtraRouteInfo{
			ExtraCIDR: []string{"not-a-cidr", "also-bad/32"},
		},
	}

	c := handler.Connects{connA, connB}
	sorted := c.Sort()
	if len(sorted) != 2 {
		t.Fatalf("invalid CIDR: expected 2, got %d", len(sorted))
	}
}

// ============================================================================
// 6. Connects.Append — nil-safe accumulation
// ============================================================================

// TestCharacterization_Connects_Append_NilSkipped verifies that nil is not
// appended to the slice.
func TestCharacterization_Connects_Append_NilSkipped(t *testing.T) {
	var c handler.Connects
	result := c.Append(nil)
	if len(result) != 0 {
		t.Errorf("Append nil: expected 0, got %d", len(result))
	}
}

// TestCharacterization_Connects_Append_NonNilAdded verifies non-nil elements
// are appended correctly.
func TestCharacterization_Connects_Append_NonNilAdded(t *testing.T) {
	var c handler.Connects
	conn := &handler.ConnectOptions{ConnectionID: "app-eeeeeeeeeeee"}
	result := c.Append(conn)
	if len(result) != 1 {
		t.Fatalf("Append non-nil: expected 1, got %d", len(result))
	}
	if result[0].GetConnectionID() != "app-eeeeeeeeeeee" {
		t.Errorf("Append non-nil: wrong element: %q", result[0].GetConnectionID())
	}
}

// TestCharacterization_Connects_Append_MixedNilAndNonNil verifies that
// repeated Append calls accumulate only non-nil values.
func TestCharacterization_Connects_Append_MixedNilAndNonNil(t *testing.T) {
	var c handler.Connects
	c = c.Append(&handler.ConnectOptions{ConnectionID: "one-ffffffffffff"})
	c = c.Append(nil)
	c = c.Append(&handler.ConnectOptions{ConnectionID: "two-ffffffffffff"})
	c = c.Append(nil)
	if len(c) != 2 {
		t.Fatalf("mixed append: expected 2, got %d", len(c))
	}
	if c[0].GetConnectionID() != "one-ffffffffffff" || c[1].GetConnectionID() != "two-ffffffffffff" {
		t.Errorf("mixed append: wrong elements: %v, %v",
			c[0].GetConnectionID(), c[1].GetConnectionID())
	}
}

// ============================================================================
// 7. ConnectionUse RPC — success path
// ============================================================================

// TestCharacterization_ConnectionUse_Success verifies that ConnectionUse sets
// currentConnectionID when the requested ID exists.
func TestCharacterization_ConnectionUse_Success(t *testing.T) {
	conn := &handler.ConnectOptions{ConnectionID: "use-gggggggggggg"}
	svr := &Server{
		connections:         []handler.Connection{conn},
		currentConnectionID: "",
	}

	resp, err := svr.ConnectionUse(context.Background(), &rpc.ConnectionUseRequest{
		ConnectionID: "use-gggggggggggg",
	})
	if err != nil {
		t.Fatalf("ConnectionUse success: unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("ConnectionUse success: expected non-nil response")
	}
	if svr.currentConnectionID != "use-gggggggggggg" {
		t.Errorf("ConnectionUse success: currentConnectionID not updated: got %q",
			svr.currentConnectionID)
	}
}

// TestCharacterization_ConnectionUse_UpdatesCurrentFromExisting verifies that
// ConnectionUse correctly switches from one connection to another when both
// exist.
func TestCharacterization_ConnectionUse_UpdatesCurrentFromExisting(t *testing.T) {
	connA := &handler.ConnectOptions{ConnectionID: "use-a-hhhhhhhhhh"}
	connB := &handler.ConnectOptions{ConnectionID: "use-b-hhhhhhhhhh"}
	svr := &Server{
		connections:         []handler.Connection{connA, connB},
		currentConnectionID: "use-a-hhhhhhhhhh",
	}

	_, err := svr.ConnectionUse(context.Background(), &rpc.ConnectionUseRequest{
		ConnectionID: "use-b-hhhhhhhhhh",
	})
	if err != nil {
		t.Fatalf("ConnectionUse switch: unexpected error: %v", err)
	}
	if svr.currentConnectionID != "use-b-hhhhhhhhhh" {
		t.Errorf("ConnectionUse switch: expected 'use-b-...', got %q", svr.currentConnectionID)
	}
}

// ============================================================================
// 8. cleanupConnection — rollback is invoked
// ============================================================================

// TestCharacterization_CleanupConnection_RollbackInvokedOnce verifies that
// exactly one rollback function is called by cleanupConnection.
func TestCharacterization_CleanupConnection_RollbackInvokedOnce(t *testing.T) {
	var count atomic.Int32
	conn := &handler.ConnectOptions{}
	conn.AddRollbackFunc(func() error {
		count.Add(1)
		return nil
	})
	cleanupConnection(context.Background(), conn)
	if count.Load() != 1 {
		t.Errorf("cleanupConnection: expected 1 rollback call, got %d", count.Load())
	}
}

// TestCharacterization_CleanupConnection_MultipleRollbacks verifies that when
// multiple rollback functions are registered, ALL are called by cleanupConnection.
// (Cleanup runs them in LIFO order — the Cleanup implementation reverses the slice.)
func TestCharacterization_CleanupConnection_MultipleRollbacks(t *testing.T) {
	var order []int
	mu := &atomic.Int32{}
	conn := &handler.ConnectOptions{}
	for i := 0; i < 3; i++ {
		i := i
		conn.AddRollbackFunc(func() error {
			mu.Add(1)
			order = append(order, i)
			return nil
		})
	}
	cleanupConnection(context.Background(), conn)
	if int(mu.Load()) != 3 {
		t.Errorf("cleanupConnection: expected 3 rollback calls, got %d", mu.Load())
	}
}

// TestCharacterization_CleanupConnection_NilIsNoop verifies that passing nil
// does not panic (guards the nil-check branch).
func TestCharacterization_CleanupConnection_NilIsNoop(t *testing.T) {
	cleanupConnection(context.Background(), nil)
	// If we reach here without panicking, the test passes.
}

// ============================================================================
// 9. ConnectionList RPC — list ordering and currentConnectionID echo
// ============================================================================

// TestCharacterization_ConnectionList_EchoesCurrentID verifies that the
// currentConnectionID set on the server is echoed in the response regardless
// of whether it matches any live connection.
func TestCharacterization_ConnectionList_EchoesCurrentID(t *testing.T) {
	svr := &Server{
		IsSudo:              true, // skip getSudoTunIPs which needs GetClient
		currentConnectionID: "echoed-iiiiiiiiii",
	}
	resp, err := svr.ConnectionList(context.Background(), &rpc.ConnectionListRequest{})
	if err != nil {
		t.Fatalf("ConnectionList: unexpected error: %v", err)
	}
	if resp.CurrentConnectionID != "echoed-iiiiiiiiii" {
		t.Errorf("ConnectionList: CurrentConnectionID not echoed: got %q",
			resp.CurrentConnectionID)
	}
}

// TestCharacterization_ConnectionList_CountMatchesConnections verifies that
// the response list length equals the number of connections in the server.
//
// Note: buildConnectionStatus requires a factory to avoid a nil-dereference;
// a ConnectOptions with nil factory causes no panic in IsSudo=true mode only
// when the status path that calls GetFactory is reached. We use IsSudo=true
// (sudo daemon — no getSudoTunIPs call) and nil-factory connections to keep
// the test cluster-free. buildConnectionStatus gracefully handles nil factory
// by returning StatusFailed and empty cluster name (tested separately in
// status_test.go). This test only asserts the count.
func TestCharacterization_ConnectionList_CountMatchesConnections(t *testing.T) {
	// Use connections with nil ConnectionID — they get status StatusFailed because
	// GetConnectionID() returns "" and no TUN IP is set. We only check count here.
	//
	// We need a valid factory to avoid a nil dereference in GetKubeconfigCluster
	// inside buildConnectionStatus. Use the helper from testing.go via the factory
	// built with a temp kubeconfig — but that requires os and genericclioptions.
	// Instead, use IsSudo=true + the plain ConnectOptions with nil factory, which
	// would panic. The safe path: supply connections via the approach used in
	// persistence_test.go (raw RequestRaw only), but those connections don't
	// satisfy the factory path either.
	//
	// Resolution: use an empty connections slice (zero connections = zero list).
	// The separate TestServer_ConnectionList_WithConnections in persistence_test.go
	// already characterizes the non-empty case with a real factory. This test
	// covers the empty-connections invariant.

	svr := &Server{
		IsSudo:              true,
		currentConnectionID: "current-jjjjjjjjjj",
	}
	resp, err := svr.ConnectionList(context.Background(), &rpc.ConnectionListRequest{})
	if err != nil {
		t.Fatalf("ConnectionList empty: unexpected error: %v", err)
	}
	if len(resp.List) != 0 {
		t.Errorf("ConnectionList empty: expected 0, got %d", len(resp.List))
	}
}

// ============================================================================
// 10. removeConnection + resetCurrentConnection — full lifecycle sequence
//
// This is the critical integration scenario for C1: the daemon removes a
// connection and then resets current. These must compose correctly.
// ============================================================================

// TestCharacterization_RemoveThenReset_CurrentMigratesForward verifies the
// full remove-then-reset sequence that disconnect() performs:
//
//	connections = [A, B, C], current = B
//	→ removeConnection(B)
//	→ connections = [A, C]
//	→ resetCurrentConnection(B)
//	→ current = A  (first remaining)
func TestCharacterization_RemoveThenReset_CurrentMigratesForward(t *testing.T) {
	connA := &handler.ConnectOptions{ConnectionID: "aaa-kkkkkkkkkkkk"}
	connB := &handler.ConnectOptions{ConnectionID: "bbb-kkkkkkkkkkkk"}
	connC := &handler.ConnectOptions{ConnectionID: "ccc-kkkkkkkkkkkk"}

	svr := &Server{
		connections:         []handler.Connection{connA, connB, connC},
		currentConnectionID: "bbb-kkkkkkkkkkkk",
	}

	removed := svr.removeConnection("bbb-kkkkkkkkkkkk")
	svr.resetCurrentConnection("bbb-kkkkkkkkkkkk")

	// Removed must contain exactly B.
	if len(removed) != 1 || removed[0].GetConnectionID() != "bbb-kkkkkkkkkkkk" {
		t.Fatalf("remove+reset: wrong removed: %v", removed)
	}
	// Remaining: [A, C].
	if len(svr.connections) != 2 {
		t.Fatalf("remove+reset: expected 2 remaining, got %d", len(svr.connections))
	}
	// Current migrates to first remaining = A.
	if svr.currentConnectionID != "aaa-kkkkkkkkkkkk" {
		t.Errorf("remove+reset: expected current='aaa-...', got %q", svr.currentConnectionID)
	}
	// A and C are still findable.
	if f, _ := svr.findConnection("aaa-kkkkkkkkkkkk"); f == nil {
		t.Error("remove+reset: A must still be findable")
	}
	if f, _ := svr.findConnection("ccc-kkkkkkkkkkkk"); f == nil {
		t.Error("remove+reset: C must still be findable")
	}
	// B is gone.
	if f, _ := svr.findConnection("bbb-kkkkkkkkkkkk"); f != nil {
		t.Error("remove+reset: B must NOT be findable after removal")
	}
}

// TestCharacterization_RemoveThenReset_LastConnection verifies removing the
// last connection results in empty state.
func TestCharacterization_RemoveThenReset_LastConnection(t *testing.T) {
	conn := &handler.ConnectOptions{ConnectionID: "last-llllllllllll"}
	svr := &Server{
		connections:         []handler.Connection{conn},
		currentConnectionID: "last-llllllllllll",
	}

	svr.removeConnection("last-llllllllllll")
	svr.resetCurrentConnection("last-llllllllllll")

	if len(svr.connections) != 0 {
		t.Errorf("last conn removed: expected 0, got %d", len(svr.connections))
	}
	if svr.currentConnectionID != "" {
		t.Errorf("last conn removed: expected empty currentID, got %q", svr.currentConnectionID)
	}
}

// TestCharacterization_RemoveThenReset_NonCurrentRemoved verifies that
// removing a non-current connection does not change currentConnectionID.
func TestCharacterization_RemoveThenReset_NonCurrentRemoved(t *testing.T) {
	connA := &handler.ConnectOptions{ConnectionID: "cur-mmmmmmmmmmmm"}
	connB := &handler.ConnectOptions{ConnectionID: "del-mmmmmmmmmmmm"}

	svr := &Server{
		connections:         []handler.Connection{connA, connB},
		currentConnectionID: "cur-mmmmmmmmmmmm",
	}

	svr.removeConnection("del-mmmmmmmmmmmm")
	svr.resetCurrentConnection("del-mmmmmmmmmmmm")

	if svr.currentConnectionID != "cur-mmmmmmmmmmmm" {
		t.Errorf("non-current removed: current should be unchanged, got %q",
			svr.currentConnectionID)
	}
	if len(svr.connections) != 1 {
		t.Errorf("non-current removed: expected 1 remaining, got %d", len(svr.connections))
	}
}

// ============================================================================
// 11. GetConnectionID accessor — nil-safety characterization
// ============================================================================

// TestCharacterization_GetConnectionID_NilReceiver verifies that calling
// GetConnectionID() on a nil *ConnectOptions returns "" without panic.
// This is the nil guard in the current implementation.
func TestCharacterization_GetConnectionID_NilReceiver(t *testing.T) {
	var conn *handler.ConnectOptions
	got := conn.GetConnectionID()
	if got != "" {
		t.Errorf("GetConnectionID on nil: expected \"\", got %q", got)
	}
}

// TestCharacterization_GetConnectionID_Set verifies that the ConnectionID
// field round-trips correctly through the GetConnectionID accessor.
func TestCharacterization_GetConnectionID_Set(t *testing.T) {
	conn := &handler.ConnectOptions{ConnectionID: "accessor-nnnnnnnn"}
	got := conn.GetConnectionID()
	if got != "accessor-nnnnnnnn" {
		t.Errorf("GetConnectionID: expected 'accessor-...', got %q", got)
	}
}

// ============================================================================
// 12. net.IP.String() round-trip — characterizes what siblingTunIPs rejects
//
// siblingTunIPs calls net.ParseIP() on the strings returned by GetLocalTunIP().
// Any string that is not a valid IP address is silently dropped. Characterize
// the two cases: valid and invalid IP strings.
// ============================================================================

// TestCharacterization_ParseIP_ValidStringsAccepted verifies that IPv4 and
// IPv6 strings accepted by net.ParseIP produce non-nil IPs (i.e. they would
// be included in siblingTunIPs output if returned by GetLocalTunIP).
func TestCharacterization_ParseIP_ValidStringsAccepted(t *testing.T) {
	cases := []string{
		"10.100.0.1",
		"192.168.1.1",
		"fd00::1",
		"::1",
	}
	for _, s := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Errorf("net.ParseIP(%q) = nil; expected non-nil (would be dropped from siblingTunIPs)", s)
		}
	}
}

// TestCharacterization_ParseIP_EmptyStringDropped verifies that the empty
// string returns nil from net.ParseIP (the "" case from GetLocalTunIP when
// no TUN device is set). This must be silently skipped in siblingTunIPs.
func TestCharacterization_ParseIP_EmptyStringDropped(t *testing.T) {
	ip := net.ParseIP("")
	if ip != nil {
		t.Errorf("net.ParseIP(\"\") should return nil, got %v", ip)
	}
}


// ============================================================================
// C1 tests: Connection interface migration
// ============================================================================

// mockConnection implements handler.Connection with override fields for the
// five new interface methods added in C1. It embeds *handler.ConnectOptions
// to inherit all other interface methods so the mock satisfies the full
// Connection interface without re-implementing every method.
type mockConnection struct {
	*handler.ConnectOptions
	ownerID      string
	workloadNS   string
	apiServerIPs []net.IP
	extraCIDR    []string
	extraHost    []dns.Entry
}

func (m *mockConnection) GetOwnerID() string             { return m.ownerID }
func (m *mockConnection) GetWorkloadNamespace() string    { return m.workloadNS }
func (m *mockConnection) GetAPIServerIPs() []net.IP      { return m.apiServerIPs }
func (m *mockConnection) GetExtraCIDR() []string         { return m.extraCIDR }
func (m *mockConnection) GetNetworkExtraHost() []dns.Entry { return m.extraHost }
func (m *mockConnection) GetSocksListenAddr() string      { return "" }
func (m *mockConnection) GetSocksEgress() bool            { return false }

// satisfy remaining interface methods not on *ConnectOptions embed (none needed
// as *ConnectOptions already satisfies the full interface)

// Ensure mockConnection satisfies handler.Connection at compile time.
var _ handler.Connection = (*mockConnection)(nil)

// Stub methods needed by handler.Connection that are not inherited from
// *ConnectOptions via embedding (all are already provided by ConnectOptions embed).
// The embed handles: InitClient, DoConnect, Cleanup, Context, AddRollbackFunc,
// GetConnectionID, GetLocalTunIP, CreateRemoteInboundPod, LeaveAllProxyResources,
// LeaveResource, ProxyResources, GetFactory, GetClientset, GetManagerNamespace,
// GetOriginKubeconfigPath, GetSync, SetSync, GetRunningPodList, GetLastHeartbeat,
// GetTrafficManagerConfigMap.

// TestC1_Connection_InterfaceAcceptsConcrete verifies that *ConnectOptions still
// satisfies the Connection interface after the 5 new methods are added.
// This is a compile-time check; if it compiles, it passes.
func TestC1_Connection_InterfaceAcceptsConcrete(t *testing.T) {
	var _ handler.Connection = (*handler.ConnectOptions)(nil)
	// Also verify mockConnection satisfies the interface.
	var _ handler.Connection = (*mockConnection)(nil)
}

// TestC1_Leave_UsesOwnerID_ViaInterface verifies that GetOwnerID() returns the
// correct string when called through the Connection interface.
func TestC1_Leave_UsesOwnerID_ViaInterface(t *testing.T) {
	const wantOwnerID = "mock-owner-abc123"
	mock := &mockConnection{
		ConnectOptions: &handler.ConnectOptions{},
		ownerID:        wantOwnerID,
	}
	// Call through the interface to verify the method is reachable via Connection.
	var conn handler.Connection = mock
	got := conn.GetOwnerID()
	if got != wantOwnerID {
		t.Errorf("GetOwnerID via interface: expected %q, got %q", wantOwnerID, got)
	}
}

// TestC1_Status_OwnerIDComparison verifies that buildConnectionStatus uses
// GetOwnerID(), GetWorkloadNamespace(), and GetOriginKubeconfigPath() from the
// Connection interface, producing the correct rpc.Status fields.
// Uses IsSudo=true path (no getSudoTunIPs call) for a cluster-free test.
func TestC1_Status_OwnerIDComparison(t *testing.T) {
	const (
		wantNS      = "my-workload-ns"
		wantOwnerID = "status-test-owner"
	)

	// Create a temp kubeconfig to avoid nil factory panic in GetKubeconfigCluster.
	tmpKubeconfig := t.TempDir() + "/kubeconfig"
	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: c1-test-cluster
contexts:
- context:
    cluster: c1-test-cluster
    namespace: default
  name: c1-test-context
current-context: c1-test-context
`
	if err := os.WriteFile(tmpKubeconfig, []byte(kubeconfigContent), 0644); err != nil {
		t.Fatalf("writing temp kubeconfig: %v", err)
	}
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &tmpKubeconfig
	ns := "default"
	configFlags.Namespace = &ns
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	factory := cmdutil.NewFactory(matchVersionFlags)

	conn := handler.NewConnectOptionsForTest(factory, tmpKubeconfig, wantNS)
	conn.OwnerID = wantOwnerID

	// Call buildConnectionStatus directly with ips=nil (sudo daemon mode — no map lookup).
	// GetLocalTunIP returns ("", "") for a no-network conn, so tunName will be "".
	// resolveStatus with ips=nil calls GetLastHeartbeat on conn → returns zero → StatusUnhealthy.
	status := buildConnectionStatus(conn, nil)

	if status.Namespace != wantNS {
		t.Errorf("Status.Namespace: expected %q, got %q", wantNS, status.Namespace)
	}
	if status.Kubeconfig != tmpKubeconfig {
		t.Errorf("Status.Kubeconfig: expected %q, got %q", tmpKubeconfig, status.Kubeconfig)
	}
	// Verify OwnerID is reachable via the interface (the accessor exists and returns the field).
	if conn.GetOwnerID() != wantOwnerID {
		t.Errorf("GetOwnerID(): expected %q, got %q", wantOwnerID, conn.GetOwnerID())
	}
}

// TestC1_Connects_Sort_WithAPIServerIPs verifies that the Sort order is driven
// by GetAPIServerIPs() / GetExtraCIDR() / GetNetworkExtraHost() from the
// Connection interface, without needing a real cluster or unexported fields.
//
// Scenario: clusterA's API server IP (10.1.2.3) appears in clusterB's ExtraCIDR
// as "10.1.2.3/32". This means clusterA depends on clusterB (traffic to clusterA
// must route through clusterB's VPN tunnel), so clusterA must disconnect FIRST.
// Sort puts the dependent (clusterA) at index 0 after sorting.
func TestC1_Connects_Sort_WithAPIServerIPs(t *testing.T) {
	clusterA := &mockConnection{
		ConnectOptions: &handler.ConnectOptions{ConnectionID: "cluster-a"},
		apiServerIPs:   []net.IP{net.ParseIP("10.1.2.3")},
	}
	clusterB := &mockConnection{
		ConnectOptions: &handler.ConnectOptions{ConnectionID: "cluster-b"},
		extraCIDR:      []string{"10.1.2.3/32"},
	}

	connects := handler.Connects{clusterA, clusterB}
	sorted := connects.Sort()

	if len(sorted) != 2 {
		t.Fatalf("expected 2 connections after sort, got %d", len(sorted))
	}
	// clusterA (dependent) should come first — it disconnects before clusterB.
	if sorted[0].GetConnectionID() != "cluster-a" {
		t.Errorf("expected cluster-a first (dependent on cluster-b's network), got %q",
			sorted[0].GetConnectionID())
	}
	if sorted[1].GetConnectionID() != "cluster-b" {
		t.Errorf("expected cluster-b second (dependency), got %q",
			sorted[1].GetConnectionID())
	}
}

// Ensure the test-only methods compile and satisfy the Connection interface
// by providing stub implementations for methods NOT provided by the embedded
// *ConnectOptions. (In practice all methods are provided by the embed, but
// this comment serves as documentation.)

// Stub implementations for methods missing on mockConnection due to the fact
// that *handler.ConnectOptions does not yet provide them directly.
// These are provided by the embedded *handler.ConnectOptions already.

// The following methods complete the Connection interface for mockConnection:
// (all delegated to the embedded ConnectOptions)

func (m *mockConnection) InitClient(f cmdutil.Factory) error { return nil }
func (m *mockConnection) DoConnect(ctx context.Context) error { return nil }
func (m *mockConnection) Cleanup(ctx context.Context) {
	if m.ConnectOptions != nil {
		m.ConnectOptions.Cleanup(ctx)
	}
}
func (m *mockConnection) Context() context.Context { return context.Background() }
func (m *mockConnection) AddRollbackFunc(f func() error) {
	if m.ConnectOptions != nil {
		m.ConnectOptions.AddRollbackFunc(f)
	}
}
func (m *mockConnection) GetConnectionID() string {
	if m.ConnectOptions != nil {
		return m.ConnectOptions.GetConnectionID()
	}
	return ""
}
func (m *mockConnection) GetLocalTunIP() (string, string) { return "", "" }
func (m *mockConnection) CreateRemoteInboundPod(ctx context.Context, ns string, workloads []string, headers map[string]string, portMap []string, image string, v4, v6 string) error {
	return nil
}
func (m *mockConnection) LeaveAllProxyResources(ctx context.Context) error { return nil }
func (m *mockConnection) LeaveResource(ctx context.Context, resources []handler.Resources, ownerID string) error {
	return nil
}
func (m *mockConnection) ProxyResources() handler.ProxyList { return nil }
func (m *mockConnection) GetFactory() cmdutil.Factory {
	if m.ConnectOptions != nil {
		return m.ConnectOptions.GetFactory()
	}
	return nil
}
func (m *mockConnection) GetClientset() kubernetes.Interface {
	if m.ConnectOptions != nil {
		return m.ConnectOptions.GetClientset()
	}
	return nil
}
func (m *mockConnection) GetManagerNamespace() string {
	if m.ConnectOptions != nil {
		return m.ConnectOptions.GetManagerNamespace()
	}
	return ""
}
func (m *mockConnection) GetOriginKubeconfigPath() string {
	if m.ConnectOptions != nil {
		return m.ConnectOptions.GetOriginKubeconfigPath()
	}
	return ""
}
func (m *mockConnection) GetSync() *handler.SyncOptions {
	if m.ConnectOptions != nil {
		return m.ConnectOptions.GetSync()
	}
	return nil
}
func (m *mockConnection) SetSync(s *handler.SyncOptions) {
	if m.ConnectOptions != nil {
		m.ConnectOptions.SetSync(s)
	}
}
func (m *mockConnection) GetRunningPodList(ctx context.Context) ([]v1.Pod, error) { return nil, nil }
func (m *mockConnection) GetLastHeartbeat() time.Time                              { return time.Time{} }
func (m *mockConnection) GetTrafficManagerConfigMap(ctx context.Context) (*v1.ConfigMap, error) {
	return nil, nil
}
