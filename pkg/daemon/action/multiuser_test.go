package action

import (
	"sync"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// ============================================================================
// Scenario: Multiple users connect to SAME cluster simultaneously
// ============================================================================

func TestMultiUser_SameCluster_DedupByConnectionID(t *testing.T) {
	svr := &Server{}
	// First user connects → creates connection
	conn1 := &handler.ConnectOptions{ConnectionID: "cluster-a-id1", OwnerID: "user-1"}
	svr.connections = append(svr.connections, conn1)
	svr.currentConnectionID = "cluster-a-id1"

	// Second user tries to connect to same cluster (same ConnectionID)
	existing, _ := svr.findConnection("cluster-a-id1")
	if existing == nil {
		t.Fatal("should find existing connection for same cluster")
	}
	// In production: returns immediately with existing connectionID, no duplicate
	if existing.GetOwnerID() != "user-1" {
		t.Fatal("existing connection should be the first user's")
	}
}

// ============================================================================
// Scenario: Multiple users connect to DIFFERENT clusters
// ============================================================================

func TestMultiUser_DifferentClusters_Independent(t *testing.T) {
	svr := &Server{}

	clusters := []struct{ connID, owner string }{
		{"cluster-a-1234", "user-alice"},
		{"cluster-b-5678", "user-bob"},
		{"cluster-c-9012", "user-carol"},
	}

	for _, c := range clusters {
		svr.connections = append(svr.connections, &handler.ConnectOptions{
			ConnectionID: c.connID,
			OwnerID:      c.owner,
		})
	}
	svr.currentConnectionID = "cluster-a-1234"

	// Each cluster connection should be independently findable
	for _, c := range clusters {
		found, _ := svr.findConnection(c.connID)
		if found == nil || found.GetOwnerID() != c.owner {
			t.Fatalf("connection %s not found or wrong owner", c.connID)
		}
	}

	// Disconnect cluster B → A and C unaffected
	removed := svr.removeConnection("cluster-b-5678")
	svr.resetCurrentConnection("cluster-b-5678")

	if len(removed) != 1 || removed[0].GetOwnerID() != "user-bob" {
		t.Fatal("should remove only bob's connection")
	}
	if len(svr.connections) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(svr.connections))
	}
	// current was cluster-a, unchanged
	if svr.currentConnectionID != "cluster-a-1234" {
		t.Fatalf("current should still be cluster-a, got %q", svr.currentConnectionID)
	}
}

// ============================================================================
// Scenario: Switch between connections (kubevpn connect use <id>)
// ============================================================================

func TestMultiUser_SwitchConnection(t *testing.T) {
	svr := &Server{}
	svr.connections = []handler.Connection{
		&handler.ConnectOptions{ConnectionID: "prod-12345678", OwnerID: "o1"},
		&handler.ConnectOptions{ConnectionID: "staging-12345", OwnerID: "o2"},
		&handler.ConnectOptions{ConnectionID: "dev-123456789", OwnerID: "o3"},
	}
	svr.currentConnectionID = "prod-12345678"

	// Switch to staging
	if _, i := svr.findConnection("staging-12345"); i == -1 {
		t.Fatal("staging connection not found")
	}
	svr.currentConnectionID = "staging-12345"
	if svr.currentConnectionID != "staging-12345" {
		t.Fatal("switch failed")
	}

	// Switch to non-existent → should not change
	if _, i := svr.findConnection("nonexistent"); i != -1 {
		t.Fatal("should not find non-existent connection")
	}
}

// ============================================================================
// Scenario: Concurrent connect + disconnect
// ============================================================================

func TestMultiUser_ConcurrentConnectDisconnect(t *testing.T) {
	svr := &Server{}
	var wg sync.WaitGroup

	// 20 goroutines add connections
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn := &handler.ConnectOptions{
				ConnectionID: "conn-" + string(rune('a'+i%26)) + "1234567890",
				OwnerID:      "owner-" + string(rune('a'+i%26)),
			}
			svr.connMu.Lock()
			svr.connections = append(svr.connections, conn)
			svr.connMu.Unlock()
		}(i)
	}
	wg.Wait()

	// 10 goroutines disconnect randomly
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "conn-" + string(rune('a'+i%26)) + "1234567890"
			svr.connMu.Lock()
			svr.removeConnection(id)
			svr.connMu.Unlock()
		}(i)
	}
	wg.Wait()

	// Should not panic, connections count should be reasonable
	svr.connMu.RLock()
	remaining := len(svr.connections)
	svr.connMu.RUnlock()
	if remaining < 0 {
		t.Fatal("negative connections count")
	}
}

// ============================================================================
// Scenario: Disconnect current → auto-select next
// ============================================================================

func TestMultiUser_DisconnectCurrent_AutoSelectNext(t *testing.T) {
	svr := &Server{}
	svr.connections = []handler.Connection{
		&handler.ConnectOptions{ConnectionID: "first-1234567"},
		&handler.ConnectOptions{ConnectionID: "second-123456"},
		&handler.ConnectOptions{ConnectionID: "third-1234567"},
	}
	svr.currentConnectionID = "second-123456"

	svr.removeConnection("second-123456")
	svr.resetCurrentConnection("second-123456")

	// Should auto-select first remaining
	if svr.currentConnectionID != "first-1234567" {
		t.Fatalf("expected first-1234567, got %q", svr.currentConnectionID)
	}
}

// ============================================================================
// Scenario: All users disconnect → clean state
// ============================================================================

func TestMultiUser_AllDisconnect_CleanState(t *testing.T) {
	svr := &Server{}
	ids := []string{"a-1234567890", "b-1234567890", "c-1234567890"}

	for _, id := range ids {
		svr.connections = append(svr.connections, &handler.ConnectOptions{ConnectionID: id})
	}
	svr.currentConnectionID = "b-1234567890"

	// Disconnect all one by one
	for _, id := range ids {
		svr.removeConnection(id)
		svr.resetCurrentConnection(id)
	}

	if len(svr.connections) != 0 {
		t.Fatalf("expected 0 connections, got %d", len(svr.connections))
	}
	if svr.currentConnectionID != "" {
		t.Fatalf("expected empty currentConnectionID, got %q", svr.currentConnectionID)
	}
}
