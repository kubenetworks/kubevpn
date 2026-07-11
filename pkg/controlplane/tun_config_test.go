package controlplane

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func newTestServer(t *testing.T) *TunConfigServer {
	t.Helper()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-123"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data: map[string]string{
				config.KeyDHCP:  "",
				config.KeyDHCP6: "",
				config.KeyEnvoy: "",
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
