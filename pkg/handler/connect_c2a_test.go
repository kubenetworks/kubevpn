package handler

import (
	"context"
	"testing"
)

// TestC2A_ControlSession_SatisfiesProxyController verifies the Interface Segregation
// contract for the user-daemon session: *ConnectOptions satisfies the shared
// Connection interface AND the control-plane role interface ProxyController, and
// the shared accessors return safe zero values on a zero-value construction.
// It deliberately does NOT satisfy DataPlane — the data-plane methods (DoConnect,
// GetLocalTunIP, GetAPIServerIPs, GetNetworkExtraHost, GetLastHeartbeat) are absent
// from ConnectOptions, which is the whole point of the split: a wrong-plane call
// now fails to compile instead of returning a stub error at runtime.
func TestC2A_ControlSession_SatisfiesProxyController(t *testing.T) {
	c := &ConnectOptions{} // = &ControlSession{}

	// Compile-time role assertions (also enforced in connection.go; repeated here
	// so a future regression that re-adds a data-plane stub to ConnectOptions is
	// caught by this test failing to express the intent).
	var _ Connection = c
	var _ ProxyController = c

	// Shared accessors return safe zero values.
	if ctx := c.Context(); ctx != nil {
		t.Errorf("expected nil Context() on ControlSession, got %v", ctx)
	}
	if s := c.GetSync(); s != nil {
		t.Errorf("expected nil GetSync on ControlSession, got %v", s)
	}
	if addr := c.GetSocksListenAddr(); addr != "" {
		t.Errorf("expected empty SocksListenAddr on ControlSession, got %s", addr)
	}
	if c.GetSocksEgress() {
		t.Error("expected false SocksEgress on ControlSession")
	}

	// Cleanup on zero-value ControlSession must not panic.
	// cleanupControlPlane is called; clientset is nil so the Delete calls are skipped,
	// LeaveAllProxyResources returns nil immediately (proxyManager is nil).
	c.Cleanup(context.Background())
}

// TestC2A_DataSession_SatisfiesDataPlane verifies the Interface Segregation contract
// for the root-daemon session: *DataSession satisfies Connection AND the data-plane
// role interface DataPlane, and the shared GetSync accessor returns nil (file sync is
// a control-plane responsibility). It deliberately does NOT satisfy ProxyController —
// the proxy methods (CreateRemoteInboundPod, LeaveAllProxyResources, LeaveResource,
// ProxyResources, SetSync) are absent from DataSession.
func TestC2A_DataSession_SatisfiesDataPlane(t *testing.T) {
	ds := &DataSession{}

	var _ Connection = ds
	var _ DataPlane = ds

	// GetSync is a shared safe read returning nil on the data plane.
	if s := ds.GetSync(); s != nil {
		t.Errorf("expected nil GetSync on DataSession, got %v", s)
	}
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
