package handler

import (
	"context"
	"testing"
)

// TestC2A_ControlSession_StubsReturnCorrectZeroValues verifies that a zero-value ConnectOptions
// (user-daemon construction, DoConnect never called) behaves correctly:
// all data-plane accessor stubs return safe zero values, DoConnect returns an error,
// and Cleanup does not panic.
func TestC2A_ControlSession_StubsReturnCorrectZeroValues(t *testing.T) {
	c := &ConnectOptions{} // = &ControlSession{}

	// DoConnect stub must return an error, not panic.
	err := c.DoConnect(context.Background())
	if err == nil {
		t.Error("expected DoConnect to return error on ControlSession")
	}

	// Context stub must return nil.
	if ctx := c.Context(); ctx != nil {
		t.Errorf("expected nil Context() on ControlSession, got %v", ctx)
	}

	// GetLocalTunIP stubs.
	v4, v6 := c.GetLocalTunIP()
	if v4 != "" || v6 != "" {
		t.Errorf("expected empty TUN IPs on ControlSession, got %s/%s", v4, v6)
	}

	// GetLastHeartbeat stub.
	if hb := c.GetLastHeartbeat(); !hb.IsZero() {
		t.Errorf("expected zero heartbeat on ControlSession, got %v", hb)
	}

	// GetAPIServerIPs stub.
	if ips := c.GetAPIServerIPs(); ips != nil {
		t.Errorf("expected nil APIServerIPs on ControlSession, got %v", ips)
	}

	// GetNetworkExtraHost stub.
	if hosts := c.GetNetworkExtraHost(); hosts != nil {
		t.Errorf("expected nil NetworkExtraHost on ControlSession, got %v", hosts)
	}

	// Cleanup on zero-value ControlSession must not panic.
	// cleanupControlPlane is called; clientset is nil so the Delete calls are skipped,
	// LeaveAllProxyResources returns nil immediately (proxyManager is nil).
	c.Cleanup(context.Background())
}

// TestC2A_DataSession_StubsReturnCorrectZeroValues verifies that a zero-value DataSession
// returns safe zero values for control-plane stubs and does not panic.
func TestC2A_DataSession_StubsReturnCorrectZeroValues(t *testing.T) {
	ds := &DataSession{}

	// CreateRemoteInboundPod stub must return error.
	err := ds.CreateRemoteInboundPod(context.Background(), "ns", nil, nil, nil, "", "", "")
	if err == nil {
		t.Error("expected CreateRemoteInboundPod to return error on DataSession")
	}

	// LeaveAllProxyResources stub must return nil (safe no-op).
	if err := ds.LeaveAllProxyResources(context.Background()); err != nil {
		t.Errorf("expected nil from LeaveAllProxyResources on DataSession, got %v", err)
	}

	// LeaveResource stub must return nil.
	if err := ds.LeaveResource(context.Background(), nil, ""); err != nil {
		t.Errorf("expected nil from LeaveResource on DataSession, got %v", err)
	}

	// ProxyResources stub must return nil.
	if pr := ds.ProxyResources(); pr != nil {
		t.Errorf("expected nil ProxyResources on DataSession, got %v", pr)
	}

	// GetSync stub must return nil.
	if s := ds.GetSync(); s != nil {
		t.Errorf("expected nil GetSync on DataSession, got %v", s)
	}

	// SetSync must not panic.
	ds.SetSync(nil)
}

// TestC2A_Cleanup_ControlPlanePath verifies that Cleanup on a zero-value ConnectOptions
// (user-daemon path) is idempotent and does not panic.
func TestC2A_Cleanup_ControlPlanePath(t *testing.T) {
	c := &ConnectOptions{}
	// First call: cleanupControlPlane path (no clientset, no proxyManager).
	c.Cleanup(context.Background())
	// Second call must be a no-op (cleanedUp flag prevents re-entry).
	c.Cleanup(context.Background())
}

// TestC2A_Cleanup_DataPlanePath verifies that Cleanup on a DataSession with rollback functions
// calls those functions and marks the connection as cleaned up.
func TestC2A_Cleanup_DataPlanePath(t *testing.T) {
	rollbackCalled := false
	ds := &DataSession{}
	ds.AddRollbackFunc(func() error {
		rollbackCalled = true
		return nil
	})

	// First Cleanup: data-plane path.
	ds.Cleanup(context.Background())

	if !rollbackCalled {
		t.Error("expected rollback function to be called in DataSession cleanup")
	}

	// Second call must be a no-op (cleanedUp flag).
	rollbackCalled = false
	ds.Cleanup(context.Background())
	if rollbackCalled {
		t.Error("rollback function must not be called again after cleanedUp is set")
	}
}

// TestC2A_TypeAlias_ControlSessionEqualsConnectOptions verifies that ControlSession is a true
// type alias (not a type definition): both pointer types are identical and assignable.
func TestC2A_TypeAlias_ControlSessionEqualsConnectOptions(t *testing.T) {
	var cs *ControlSession = &ConnectOptions{}
	var co *ConnectOptions = cs
	_ = co
	// If this compiles, the alias is correct. Both are the same type.
}

// TestC2A_DataSession_GetConnectionID verifies nil-safe receiver behavior.
func TestC2A_DataSession_GetConnectionID(t *testing.T) {
	var ds *DataSession
	if id := ds.GetConnectionID(); id != "" {
		t.Fatalf("expected empty ID on nil DataSession, got %q", id)
	}
	ds = &DataSession{ConnectionID: "abc123def456"}
	if id := ds.GetConnectionID(); id != "abc123def456" {
		t.Fatalf("expected abc123def456, got %q", id)
	}
}

// TestC2A_DataSession_GetOwnerID verifies nil-safe receiver behavior.
func TestC2A_DataSession_GetOwnerID(t *testing.T) {
	var ds *DataSession
	if id := ds.GetOwnerID(); id != "" {
		t.Fatalf("expected empty ID on nil DataSession, got %q", id)
	}
	ds = &DataSession{OwnerID: "owner-id-1"}
	if id := ds.GetOwnerID(); id != "owner-id-1" {
		t.Fatalf("expected owner-id-1, got %q", id)
	}
}

// TestC2A_DataSession_NilNmGuards verifies that data-plane accessors guard ds.nm == nil.
func TestC2A_DataSession_NilNmGuards(t *testing.T) {
	ds := &DataSession{} // nm is nil

	// GetLocalTunIP: both empty.
	v4, v6 := ds.GetLocalTunIP()
	if v4 != "" || v6 != "" {
		t.Errorf("expected empty TUN IPs when nm is nil, got %q/%q", v4, v6)
	}

	// GetLastHeartbeat: zero time.
	if hb := ds.GetLastHeartbeat(); !hb.IsZero() {
		t.Errorf("expected zero heartbeat when nm is nil, got %v", hb)
	}

	// GetAPIServerIPs: nil.
	if ips := ds.GetAPIServerIPs(); ips != nil {
		t.Errorf("expected nil APIServerIPs when nm is nil, got %v", ips)
	}

	// GetNetworkExtraHost: nil.
	if hosts := ds.GetNetworkExtraHost(); hosts != nil {
		t.Errorf("expected nil NetworkExtraHost when nm is nil, got %v", hosts)
	}

	// Context: nil (DoConnect not called).
	if ctx := ds.Context(); ctx != nil {
		t.Errorf("expected nil Context before DoConnect, got %v", ctx)
	}
}

// TestC2A_SessionBase_AddRollbackFunc verifies that AddRollbackFunc is promoted to both types.
func TestC2A_SessionBase_AddRollbackFunc(t *testing.T) {
	t.Run("ConnectOptions", func(t *testing.T) {
		c := &ConnectOptions{}
		var order []int
		c.AddRollbackFunc(func() error { order = append(order, 1); return nil })
		c.AddRollbackFunc(func() error { order = append(order, 2); return nil })
		funcs := c.getRollbackFuncs()
		if len(funcs) != 2 {
			t.Fatalf("expected 2, got %d", len(funcs))
		}
	})
	t.Run("DataSession", func(t *testing.T) {
		ds := &DataSession{}
		var order []int
		ds.AddRollbackFunc(func() error { order = append(order, 1); return nil })
		ds.AddRollbackFunc(func() error { order = append(order, 2); return nil })
		funcs := ds.getRollbackFuncs()
		if len(funcs) != 2 {
			t.Fatalf("expected 2, got %d", len(funcs))
		}
	})
}
