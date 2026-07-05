package xds

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
)

func newTestServer(t *testing.T) *TunConfigServer {
	t.Helper()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-123"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data: map[string]string{
				config.KeyTunIPPool: "",
				config.KeyEnvoy:     "",
			},
		},
	)
	s, err := NewTunConfigServer(context.Background(), clientset, "test-ns")
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}
	return s
}

// 1. 基本分配：无 ExcludeIPs，首次调用分配新 IP
func TestGetTunIP_BasicAllocation(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   "owner-1",
		Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("GetTunIP: %v", err)
	}
	if resp.IPv4 == "" {
		t.Fatal("expected non-empty IPv4")
	}
	if resp.Version == 0 {
		t.Fatal("expected non-zero version")
	}
	t.Logf("Allocated: %s", resp.IPv4)
}

// 2. 续租：同一 ownerID 第二次调用返回同一 IP 并刷新 LastRenew
func TestGetTunIP_RenewReturnsSameIP(t *testing.T) {
	s := newTestServer(t)
	req := &rpc.TunIPRequest{OwnerID: "owner-1", Namespace: "test-ns"}

	resp1, _ := s.GetTunIP(context.Background(), req)

	s.mu.RLock()
	renew1 := s.allocs["owner-1"].LastRenew
	s.mu.RUnlock()

	time.Sleep(10 * time.Millisecond)
	resp2, _ := s.GetTunIP(context.Background(), req)

	if resp1.IPv4 != resp2.IPv4 {
		t.Fatalf("expected same IP, got %s vs %s", resp1.IPv4, resp2.IPv4)
	}

	s.mu.RLock()
	renew2 := s.allocs["owner-1"].LastRenew
	s.mu.RUnlock()

	if !renew2.After(renew1) {
		t.Fatal("LastRenew not refreshed")
	}
}

// 3. ExcludeIPs 基本功能：排除的 IP 不会被分配
func TestGetTunIP_ExcludeIPsSkipsConflicts(t *testing.T) {
	s := newTestServer(t)

	// 先分配几个 IP 搞清楚前几个 IP 是什么
	resp1, _ := s.GetTunIP(context.Background(), &rpc.TunIPRequest{OwnerID: "probe-1", Namespace: "test-ns"})
	resp2, _ := s.GetTunIP(context.Background(), &rpc.TunIPRequest{OwnerID: "probe-2", Namespace: "test-ns"})

	ip1, _, _ := net.ParseCIDR(resp1.IPv4)
	ip2, _, _ := net.ParseCIDR(resp2.IPv4)

	// 新 owner 排除前两个 IP
	resp3, err := s.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:    "owner-new",
		Namespace:  "test-ns",
		ExcludeIPs: []string{ip1.String(), ip2.String()},
	})
	if err != nil {
		t.Fatalf("GetTunIP with excludes: %v", err)
	}

	ip3, _, _ := net.ParseCIDR(resp3.IPv4)
	if ip3.Equal(ip1) || ip3.Equal(ip2) {
		t.Fatalf("allocated excluded IP %s (exclude: %s, %s)", ip3, ip1, ip2)
	}
	t.Logf("Allocated %s (excluded %s, %s)", ip3, ip1, ip2)
}

// 4. 已有分配不冲突时，ExcludeIPs 不触发重新分配
func TestGetTunIP_ExcludeIPsNoConflictKeepsExisting(t *testing.T) {
	s := newTestServer(t)
	req := &rpc.TunIPRequest{OwnerID: "owner-1", Namespace: "test-ns"}

	resp1, _ := s.GetTunIP(context.Background(), req)

	// 用不相干的 ExcludeIPs 调用
	req2 := &rpc.TunIPRequest{
		OwnerID:    "owner-1",
		Namespace:  "test-ns",
		ExcludeIPs: []string{"10.0.0.1", "192.168.1.1"},
	}
	resp2, _ := s.GetTunIP(context.Background(), req2)

	if resp1.IPv4 != resp2.IPv4 {
		t.Fatalf("should keep existing IP when no conflict, got %s vs %s", resp1.IPv4, resp2.IPv4)
	}
}

// 5. 已有分配冲突时，ExcludeIPs 触发重新分配
func TestGetTunIP_ExcludeIPsConflictReallocates(t *testing.T) {
	s := newTestServer(t)
	req := &rpc.TunIPRequest{OwnerID: "owner-1", Namespace: "test-ns"}

	resp1, _ := s.GetTunIP(context.Background(), req)
	ip1, _, _ := net.ParseCIDR(resp1.IPv4)

	// 把当前 IP 放入 ExcludeIPs
	req2 := &rpc.TunIPRequest{
		OwnerID:    "owner-1",
		Namespace:  "test-ns",
		ExcludeIPs: []string{ip1.String()},
	}
	resp2, err := s.GetTunIP(context.Background(), req2)
	if err != nil {
		t.Fatalf("re-allocate: %v", err)
	}

	ip2, _, _ := net.ParseCIDR(resp2.IPv4)
	if ip1.Equal(ip2) {
		t.Fatalf("re-allocation returned same IP %s", ip1)
	}
	t.Logf("Re-allocated from %s to %s", ip1, ip2)
}

// 6. 重新分配后旧 IP 被释放，其他 owner 可以获取到
func TestGetTunIP_ReleasedIPAvailableForOthers(t *testing.T) {
	s := newTestServer(t)

	resp1, _ := s.GetTunIP(context.Background(), &rpc.TunIPRequest{OwnerID: "owner-1", Namespace: "test-ns"})
	ip1, _, _ := net.ParseCIDR(resp1.IPv4)

	// owner-1 冲突，重新分配
	s.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:    "owner-1",
		Namespace:  "test-ns",
		ExcludeIPs: []string{ip1.String()},
	})

	// owner-2（不同机器，没有 ip1 的冲突）应该能拿到 ip1
	resp3, _ := s.GetTunIP(context.Background(), &rpc.TunIPRequest{OwnerID: "owner-2", Namespace: "test-ns"})
	ip3, _, _ := net.ParseCIDR(resp3.IPv4)
	if !ip1.Equal(ip3) {
		t.Fatalf("expected released IP %s to be reused by owner-2, got %s", ip1, ip3)
	}
}

// 7. 多个 ExcludeIPs：模拟连接了多个集群，多个 IP 都需要跳过
func TestGetTunIP_MultipleExcludeIPs(t *testing.T) {
	s := newTestServer(t)

	// 分配前 5 个 IP 给 probe owners
	probeIPs := make([]string, 5)
	for i := 0; i < 5; i++ {
		resp, _ := s.GetTunIP(context.Background(), &rpc.TunIPRequest{
			OwnerID:   fmt.Sprintf("probe-%d", i),
			Namespace: "test-ns",
		})
		ip, _, _ := net.ParseCIDR(resp.IPv4)
		probeIPs[i] = ip.String()
	}

	// 新 owner 排除所有 5 个 IP
	resp, err := s.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:    "owner-multi",
		Namespace:  "test-ns",
		ExcludeIPs: probeIPs,
	})
	if err != nil {
		t.Fatalf("GetTunIP with 5 excludes: %v", err)
	}

	ip, _, _ := net.ParseCIDR(resp.IPv4)
	for _, excluded := range probeIPs {
		if ip.String() == excluded {
			t.Fatalf("allocated excluded IP %s", ip)
		}
	}
	t.Logf("Allocated %s (excluded %v)", ip, probeIPs)
}

// 8. 多个独立 owner 互不干扰
func TestGetTunIP_MultipleOwnersIndependent(t *testing.T) {
	s := newTestServer(t)
	ips := make(map[string]string)

	for i := 0; i < 5; i++ {
		ownerID := fmt.Sprintf("owner-%d", i)
		resp, err := s.GetTunIP(context.Background(), &rpc.TunIPRequest{
			OwnerID:   ownerID,
			Namespace: "test-ns",
		})
		if err != nil {
			t.Fatalf("GetTunIP %s: %v", ownerID, err)
		}
		if prev, ok := ips[resp.IPv4]; ok {
			t.Fatalf("duplicate IP %s for %s and %s", resp.IPv4, ownerID, prev)
		}
		ips[resp.IPv4] = ownerID
	}
}

// 9. WatchTunIP 隐式续租：stream 活跃时 LastRenew 持续刷新
func TestWatchTunIP_ImplicitLeaseRenewal(t *testing.T) {
	s := newTestServer(t)

	// 先分配
	s.GetTunIP(context.Background(), &rpc.TunIPRequest{OwnerID: "owner-1", Namespace: "test-ns"})

	s.mu.RLock()
	renewBefore := s.allocs["owner-1"].LastRenew
	s.mu.RUnlock()

	// 模拟 WatchTunIP 的 renewLease 调用
	time.Sleep(10 * time.Millisecond)
	s.mu.Lock()
	s.renewLease("owner-1")
	s.mu.Unlock()

	s.mu.RLock()
	renewAfter := s.allocs["owner-1"].LastRenew
	s.mu.RUnlock()

	if !renewAfter.After(renewBefore) {
		t.Fatal("renewLease did not update LastRenew")
	}
}

// 10. LeaseReaper 回收过期分配
func TestLeaseReaper_ExpiresStaleAllocations(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	resp, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "owner-stale", Namespace: "test-ns"})
	ip, _, _ := net.ParseCIDR(resp.IPv4)

	// 手动设置 LastRenew 为过去
	s.mu.Lock()
	s.allocs["owner-stale"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	s.mu.Unlock()

	s.reapExpiredLeases(ctx)

	s.mu.RLock()
	_, exists := s.allocs["owner-stale"]
	s.mu.RUnlock()

	if exists {
		t.Fatal("expired allocation was not reaped")
	}

	// 被回收的 IP 应该可以被其他 owner 分配到
	resp2, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "owner-new", Namespace: "test-ns"})
	ip2, _, _ := net.ParseCIDR(resp2.IPv4)
	if !ip.Equal(ip2) {
		t.Fatalf("expected reaped IP %s to be reused, got %s", ip, ip2)
	}
}

// 11. LeaseReaper 不回收未过期的分配
func TestLeaseReaper_KeepsFreshAllocations(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "owner-fresh", Namespace: "test-ns"})

	s.reapExpiredLeases(ctx)

	s.mu.RLock()
	_, exists := s.allocs["owner-fresh"]
	s.mu.RUnlock()

	if !exists {
		t.Fatal("fresh allocation was incorrectly reaped")
	}
}

// 12. Server 重启后恢复未过期分配，释放已过期分配
func TestLoadAllocs_RestoresAndExpires(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	resp1, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "fresh", Namespace: "test-ns"})
	resp2, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "stale", Namespace: "test-ns"})

	// 设置 stale 为过期
	s.mu.Lock()
	s.allocs["stale"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	s.mu.Unlock()

	// 持久化
	s.saveAllocs(ctx)

	// 模拟重启：创建新 server 实例（复用同一 clientset）
	s2, err := NewTunConfigServer(ctx, s.clientset, "test-ns")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}

	s2.mu.RLock()
	_, freshExists := s2.allocs["fresh"]
	_, staleExists := s2.allocs["stale"]
	s2.mu.RUnlock()

	if !freshExists {
		t.Fatal("fresh allocation not restored after restart")
	}
	if staleExists {
		t.Fatal("stale allocation should have been expired on restart")
	}

	// fresh 应该拿回同一 IP
	resp3, _ := s2.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "fresh", Namespace: "test-ns"})
	if resp1.IPv4 != resp3.IPv4 {
		t.Fatalf("fresh IP changed after restart: %s → %s", resp1.IPv4, resp3.IPv4)
	}
	_ = resp2 // stale IP was released
}

// 13. RentIPExcluding：nil excludeIPs 等同于 RentIP
func TestRentIPExcluding_NilSameAsRentIP(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	v4a, _, err := s.dhcp.RentIPExcluding(ctx, nil)
	if err != nil {
		t.Fatalf("RentIPExcluding(nil): %v", err)
	}
	_ = s.dhcp.ReleaseIP(ctx, v4a.IP, net.IPv6zero)

	v4b, _, err := s.dhcp.RentIP(ctx)
	if err != nil {
		t.Fatalf("RentIP: %v", err)
	}

	if !v4a.IP.Equal(v4b.IP) {
		t.Fatalf("RentIPExcluding(nil) and RentIP diverge: %s vs %s", v4a.IP, v4b.IP)
	}
}

// 14. contiguous allocator 的 release-then-rent 回环问题确认 + ExcludeIPs 解决
func TestExcludeIPs_PreventsReleaseRentLoop(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// 分配第一个 IP
	resp1, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "loop-owner", Namespace: "test-ns"})
	ip1, _, _ := net.ParseCIDR(resp1.IPv4)

	// 模拟 3 轮冲突：每次把当前 IP 加入 ExcludeIPs
	currentIP := ip1.String()
	excludes := []string{currentIP}

	for round := 0; round < 3; round++ {
		resp, err := s.GetTunIP(ctx, &rpc.TunIPRequest{
			OwnerID:    "loop-owner",
			Namespace:  "test-ns",
			ExcludeIPs: excludes,
		})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		newIP, _, _ := net.ParseCIDR(resp.IPv4)
		if newIP.String() == currentIP {
			t.Fatalf("round %d: got same IP %s back (release-rent loop!)", round, currentIP)
		}
		for _, ex := range excludes {
			if newIP.String() == ex {
				t.Fatalf("round %d: got previously excluded IP %s", round, newIP)
			}
		}
		currentIP = newIP.String()
		excludes = append(excludes, currentIP)
		t.Logf("round %d: re-allocated to %s", round, currentIP)
	}
}

// Fix 1: saveAllocs now writes the whole ConfigMap via an optimistic-locked
// Update (instead of a per-key JSONPatch). Verify it does NOT clobber the other
// independently-written keys (ENVOY_CONFIG, CLUSTER_CIDRS, TUN_IP_POOL).
func TestSaveAllocs_PreservesOtherConfigMapKeys(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-123"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data: map[string]string{
				config.KeyTunIPPool: "",
				// Valid empty Virtual list: parseYaml-safe, so the async syncEnvoyRuleIP
				// (fired by GetTunIP) reads it without the "cannot unmarshal string" error,
				// and leaves it untouched (no rules to update) — still proving saveAllocs
				// doesn't clobber it.
				config.KeyEnvoy:        "[]\n",
				config.KeyClusterCIDRs: "cidr-sentinel",
			},
		},
	)
	s, err := NewTunConfigServer(ctx, clientset, "test-ns")
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}

	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "owner-1", Namespace: "test-ns"}); err != nil {
		t.Fatalf("GetTunIP: %v", err)
	}

	cm, err := clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if cm.Data[config.KeyEnvoy] != "[]\n" {
		t.Errorf("ENVOY_CONFIG clobbered by saveAllocs: %q", cm.Data[config.KeyEnvoy])
	}
	if cm.Data[config.KeyClusterCIDRs] != "cidr-sentinel" {
		t.Errorf("CLUSTER_CIDRS clobbered by saveAllocs: %q", cm.Data[config.KeyClusterCIDRs])
	}
	if cm.Data[config.KeyTunAllocs] == "" {
		t.Error("TUN_ALLOCS not persisted")
	}
	if cm.Data[config.KeyTunIPPool] == "" {
		t.Error("TUN_IP_POOL bitmap not persisted")
	}
}

func countAllocatedBits(t *testing.T, s *TunConfigServer) int {
	t.Helper()
	count := 0
	inc := func(net.IP) { count++ }
	if err := s.dhcp.ForEach(context.Background(), inc, inc); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	return count
}

// Fix 2: a bitmap bit with no owning lease (e.g. leaked by a crash between
// renting and persisting) must be reclaimed by the startup/reaper scrub.
func TestScrubOrphanBits_ReclaimsLeakedIP(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-123"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: ""},
		},
	)
	// Simulate a leak: set bitmap bits via the dhcp manager WITHOUT recording an
	// owner in TUN_ALLOCS.
	mgr := dhcp.NewDHCPManager(clientset, "test-ns")
	if err := mgr.InitDHCP(ctx); err != nil {
		t.Fatalf("InitDHCP: %v", err)
	}
	if _, _, err := mgr.RentIP(ctx); err != nil {
		t.Fatalf("RentIP: %v", err)
	}

	// NewTunConfigServer loads allocs (empty) then scrubs orphan bits.
	s, err := NewTunConfigServer(ctx, clientset, "test-ns")
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}
	if n := countAllocatedBits(t, s); n != 0 {
		t.Fatalf("expected 0 allocated bits after scrub, got %d", n)
	}
}

// Fix 2: if persisting the allocation fails, GetTunIP must roll back — release
// the rented IP and drop the in-memory record — so no bitmap bit is orphaned.
func TestGetTunIP_RollsBackOnPersistFailure(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-123"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: ""},
		},
	)
	s, err := NewTunConfigServer(ctx, clientset, "test-ns")
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}

	// saveAllocs uses Get+Update; fail Update so persistence fails. RentIP and
	// ReleaseIP use Patch, so renting and rollback-release still work.
	clientset.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected update failure")
	})

	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "owner-x", Namespace: "test-ns"}); err == nil {
		t.Fatal("expected GetTunIP to fail when persistence fails")
	}
	if _, ok := s.allocs["owner-x"]; ok {
		t.Error("in-memory alloc not rolled back")
	}
	if n := countAllocatedBits(t, s); n != 0 {
		t.Errorf("expected bitmap empty after rollback, got %d bits", n)
	}
}

// Part B (docs/29): after its lease is reaped, a (stable) owner reclaims the
// same IP on reconnect even when a lower-numbered free IP exists that plain
// AllocateNext would have returned.
func TestGetTunIP_StickyReconnectPrefersRememberedIP(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	a, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	_, _ = s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "B", Namespace: "test-ns"})
	c, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "C", Namespace: "test-ns"})
	ipA, _, _ := net.ParseCIDR(a.IPv4)
	ipC, _, _ := net.ParseCIDR(c.IPv4)

	// Reap A and C (B keeps its IP) → frees ipA (low) and ipC (higher).
	s.mu.Lock()
	s.allocs["A"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	s.allocs["C"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	s.mu.Unlock()
	s.reapExpiredLeases(ctx)

	// C reconnects → must reclaim ipC, not the lower free ipA.
	c2, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "C", Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	ipC2, _, _ := net.ParseCIDR(c2.IPv4)
	if !ipC.Equal(ipC2) {
		t.Fatalf("sticky: expected C to reclaim %s, got %s (lower free ipA=%s)", ipC, ipC2, ipA)
	}
}

// Part B: if the remembered IP was taken by someone else, reconnect falls back
// to a different free IP (no collision).
func TestGetTunIP_StickyFallsBackWhenRememberedIPTaken(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	a, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ipA, _, _ := net.ParseCIDR(a.IPv4)

	s.mu.Lock()
	s.allocs["A"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	s.mu.Unlock()
	s.reapExpiredLeases(ctx)

	// B grabs the freed (lowest) IP, which is ipA.
	b, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "B", Namespace: "test-ns"})
	ipB, _, _ := net.ParseCIDR(b.IPv4)
	if !ipA.Equal(ipB) {
		t.Fatalf("setup: expected B to take freed %s, got %s", ipA, ipB)
	}

	// A reconnects → ipA is taken → must fall back to a different IP.
	a2, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	ipA2, _, _ := net.ParseCIDR(a2.IPv4)
	if ipA2.Equal(ipA) {
		t.Fatalf("expected fallback away from taken IP %s", ipA)
	}
}

// Part B + Fix 3: the remembered IP yields to ExcludeIPs (e.g. a sibling cluster
// holds it locally), falling back to a different IP.
func TestGetTunIP_StickyYieldsToExcludeIPs(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	a, _ := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ipA, _, _ := net.ParseCIDR(a.IPv4)

	s.mu.Lock()
	s.allocs["A"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	s.mu.Unlock()
	s.reapExpiredLeases(ctx)

	// A reconnects but excludes its old IP → must not reclaim it.
	a2, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns", ExcludeIPs: []string{ipA.String()}})
	if err != nil {
		t.Fatal(err)
	}
	ipA2, _, _ := net.ParseCIDR(a2.IPv4)
	if ipA2.Equal(ipA) {
		t.Fatalf("expected to yield to ExcludeIPs, but reclaimed %s", ipA)
	}
}

// Fix 4: a reconcile-driven IP change (NotifyIPChange) must also update the
// owner's envoy rule IP, not just the watchers — otherwise mesh traffic keeps
// routing to the stale local TUN IP.
func TestNotifyIPChange_SyncsEnvoyRuleIP(t *testing.T) {
	ctx := context.Background()
	virtuals := []*Virtual{{
		SchemaVersion: CurrentSchemaVersion,
		Namespace:     "test-ns",
		UID:           "deployments.apps.reviews",
		Rules: []*Rule{{
			Headers:      map[string]string{},
			OwnerID:      "owner-1",
			LocalTunIPv4: "198.18.0.5",
			LocalTunIPv6: "2001:2::5",
		}},
	}}
	envoyYAML, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatalf("marshal envoy: %v", err)
	}
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-123"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: string(envoyYAML)},
		},
	)
	s, err := NewTunConfigServer(ctx, clientset, "test-ns")
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}

	newV4 := &net.IPNet{IP: net.ParseIP("198.18.0.9"), Mask: net.CIDRMask(16, 32)}
	newV6 := &net.IPNet{IP: net.ParseIP("2001:2::9"), Mask: net.CIDRMask(64, 128)}
	s.NotifyIPChange("owner-1", newV4, newV6)

	// syncEnvoyRuleIP runs asynchronously; poll for the update.
	deadline := time.Now().Add(2 * time.Second)
	for {
		cm, _ := clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		vs, perr := parseYaml(cm.Data[config.KeyEnvoy])
		got := ""
		if perr == nil && len(vs) == 1 && len(vs[0].Rules) == 1 {
			got = vs[0].Rules[0].LocalTunIPv4
		}
		if got == "198.18.0.9" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("envoy rule IP not synced after NotifyIPChange, got %q want 198.18.0.9", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGetTunIP_HostnamePersistedToConfigMap exercises the full hostname lifecycle:
// a client passes its machine name to GetTunIP over real gRPC, the server records it
// in the TUN_ALLOCS ConfigMap, and a restarted server restores it via loadAllocs.
// It also asserts that a client which sends no hostname leaves a clean entry (omitempty).
func TestGetTunIP_HostnamePersistedToConfigMap(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Phase 1: allocate with a hostname (real gRPC), and a second owner without one.
	resp, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "owner-host", Namespace: "test-ns", Hostname: "dev-laptop-01",
	})
	if err != nil {
		t.Fatalf("GetTunIP(with hostname): %v", err)
	}
	if resp.IPv4 == "" {
		t.Fatal("expected non-empty IPv4")
	}
	if _, err = env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "owner-nohost", Namespace: "test-ns",
	}); err != nil {
		t.Fatalf("GetTunIP(no hostname): %v", err)
	}

	// Phase 2: the hostname is persisted in the TUN_ALLOCS ConfigMap.
	cm, err := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	raw := cm.Data[config.KeyTunAllocs]
	persisted, err := parsePersistedAllocs(raw)
	if err != nil {
		t.Fatalf("parse TUN_ALLOCS: %v", err)
	}
	if got := persisted["owner-host"].Hostname; got != "dev-laptop-01" {
		t.Fatalf("owner-host hostname = %q, want dev-laptop-01", got)
	}
	if got := persisted["owner-nohost"].Hostname; got != "" {
		t.Fatalf("owner-nohost hostname = %q, want empty", got)
	}
	// omitempty: only the owner that sent a hostname produces a hostname key in the raw YAML.
	if !strings.Contains(raw, "hostname: dev-laptop-01") {
		t.Fatalf("raw TUN_ALLOCS missing hostname entry:\n%s", raw)
	}
	if n := strings.Count(raw, "hostname:"); n != 1 {
		t.Fatalf("expected exactly 1 hostname key (omitempty), got %d:\n%s", n, raw)
	}

	// Phase 3: a restarted server restores the hostname from the ConfigMap.
	s2, err := NewTunConfigServer(ctx, env.server.clientset, "test-ns")
	if err != nil {
		t.Fatalf("restart NewTunConfigServer: %v", err)
	}
	s2.mu.RLock()
	got := s2.allocs["owner-host"]
	s2.mu.RUnlock()
	if got == nil {
		t.Fatal("owner-host alloc not restored after restart")
	}
	if got.Hostname != "dev-laptop-01" {
		t.Fatalf("restored hostname = %q, want dev-laptop-01", got.Hostname)
	}
}
