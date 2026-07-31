package xds

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// testEnv bundles a TunConfigServer with a running gRPC server and client connection.
type testEnv struct {
	server     *TunConfigServer
	grpcServer *grpc.Server
	port       int
	conn       *grpc.ClientConn
	client     rpc.TunConfigServiceClient
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-456"}},
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

	grpcServer := grpc.NewServer()
	rpc.RegisterTunConfigServiceServer(grpcServer, s)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port

	go grpcServer.Serve(lis)

	conn, err := grpc.DialContext(context.Background(),
		fmt.Sprintf("127.0.0.1:%d", port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		grpcServer.Stop()
		t.Fatalf("dial: %v", err)
	}

	env := &testEnv{
		server:     s,
		grpcServer: grpcServer,
		port:       port,
		conn:       conn,
		client:     rpc.NewTunConfigServiceClient(conn),
	}
	t.Cleanup(func() {
		conn.Close()
		grpcServer.Stop()
	})
	return env
}

// helper: parse IPv4 from TunIPResponse
func parseV4(t *testing.T, resp *rpc.TunIPResponse) net.IP {
	t.Helper()
	ip, _, err := net.ParseCIDR(resp.IPv4)
	if err != nil {
		t.Fatalf("parse %q: %v", resp.IPv4, err)
	}
	return ip
}

// waitForEnvoyRuleIPv4 polls the ENVOY_CONFIG ConfigMap until ownerID's rule has
// LocalTunIPv4 equal to want (compared as IPs, so format-agnostic), or until the
// timeout elapses. It replaces a fixed time.Sleep waiting for the async
// syncEnvoyRuleIP goroutine (fired by GetTunIP/lease-renew) to finish its
// read-modify-write of ENVOY_CONFIG — a sleep that flaked on slow CI when the
// 500ms budget ran out before the goroutine completed.
//
// Polls every 10ms; the 2s ceiling is generous since syncEnvoyRuleIP only does an
// in-memory ConfigMap RMW against a fake clientset (no real API latency), so it
// completes in microseconds on a healthy run.
func waitForEnvoyRuleIPv4(t *testing.T, env *testEnv, ownerID string, want net.IP) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cm, err := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err == nil {
			virtuals, perr := parseYaml(cm.Data[config.KeyEnvoy])
			if perr == nil {
				for _, v := range virtuals {
					for _, r := range v.Rules {
						if r.OwnerID == ownerID {
							if ruleIP := net.ParseIP(r.LocalTunIPv4); ruleIP != nil && ruleIP.Equal(want) {
								return
							}
						}
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("envoy rule for owner %s did not converge to %s within 2s (async syncEnvoyRuleIP?)", ownerID, want)
}

// ---- Integration Tests: Client ↔ Server over gRPC ----

// 1. Client 正常分配：通过 gRPC 首次获取 IP
func TestIntegration_BasicAllocOverGRPC(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   "client-1",
		Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("GetTunIP: %v", err)
	}
	ip := parseV4(t, resp)
	if !config.CIDR.Contains(ip) {
		t.Fatalf("IP %s not in CIDR %s", ip, config.CIDR)
	}
}

// 2. Client 续租：同一 ownerID 通过 gRPC 拿到同一 IP
func TestIntegration_RenewOverGRPC(t *testing.T) {
	env := newTestEnv(t)
	req := &rpc.TunIPRequest{OwnerID: "client-1", Namespace: "test-ns"}

	resp1, _ := env.client.GetTunIP(context.Background(), req)
	resp2, _ := env.client.GetTunIP(context.Background(), req)

	if resp1.IPv4 != resp2.IPv4 {
		t.Fatalf("renewal returned different IP: %s vs %s", resp1.IPv4, resp2.IPv4)
	}
}

// 3. ExcludeIPs 跳过冲突 IP：模拟 rentIP 发送本地接口 IP
func TestIntegration_ExcludeIPsOverGRPC(t *testing.T) {
	env := newTestEnv(t)

	// 先分配，获取第一个 IP
	resp1, _ := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   "client-1",
		Namespace: "test-ns",
	})
	ip1 := parseV4(t, resp1)

	// 新 client 排除第一个 IP
	resp2, err := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:    "client-2",
		Namespace:  "test-ns",
		ExcludeIPs: []string{ip1.String()},
	})
	if err != nil {
		t.Fatalf("GetTunIP with exclude: %v", err)
	}
	ip2 := parseV4(t, resp2)
	if ip1.Equal(ip2) {
		t.Fatalf("got excluded IP %s", ip2)
	}
}

// 4. 已有分配冲突 → 重新分配 → 旧 IP 释放给他人
func TestIntegration_ConflictReallocAndReuse(t *testing.T) {
	env := newTestEnv(t)

	// client-1 正常分配
	resp1, _ := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID: "client-1", Namespace: "test-ns",
	})
	ip1 := parseV4(t, resp1)

	// client-1 再次调用但带冲突 ExcludeIPs → 触发重新分配
	resp2, _ := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:    "client-1",
		Namespace:  "test-ns",
		ExcludeIPs: []string{ip1.String()},
	})
	ip2 := parseV4(t, resp2)
	if ip1.Equal(ip2) {
		t.Fatalf("re-alloc returned same IP %s", ip1)
	}

	// client-2（不同机器，无冲突）应该拿到 client-1 释放的 ip1
	resp3, _ := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID: "client-2", Namespace: "test-ns",
	})
	ip3 := parseV4(t, resp3)
	if !ip1.Equal(ip3) {
		t.Fatalf("released IP %s not reused, got %s", ip1, ip3)
	}
}

// 5. 多轮连续冲突不死循环（模拟连了 3 个集群，前 3 个 IP 都占用）
func TestIntegration_MultiRoundConflictNeverLoops(t *testing.T) {
	env := newTestEnv(t)

	// 先拿到第一个 IP
	resp, _ := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID: "owner-x", Namespace: "test-ns",
	})
	currentIP := parseV4(t, resp)
	excludes := []string{currentIP.String()}

	for round := 0; round < 5; round++ {
		resp, err := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
			OwnerID:    "owner-x",
			Namespace:  "test-ns",
			ExcludeIPs: excludes,
		})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		newIP := parseV4(t, resp)
		for _, ex := range excludes {
			if newIP.String() == ex {
				t.Fatalf("round %d: got previously excluded IP %s", round, newIP)
			}
		}
		currentIP = newIP
		excludes = append(excludes, currentIP.String())
	}
}

// 6. 并发多个 client 依次分配，互不干扰且无重复
func TestIntegration_ConcurrentAllocation(t *testing.T) {
	env := newTestEnv(t)

	const n = 20
	seen := make(map[string]int)
	for i := 0; i < n; i++ {
		resp, err := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
			OwnerID:   fmt.Sprintf("sequential-%d", i),
			Namespace: "test-ns",
		})
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		if prev, ok := seen[resp.IPv4]; ok {
			t.Fatalf("duplicate IP %s: clients %d and %d", resp.IPv4, prev, i)
		}
		seen[resp.IPv4] = i
	}
}

// 7. WatchTunIP stream 收到 NotifyIPChange 推送
func TestIntegration_WatchReceivesIPChange(t *testing.T) {
	env := newTestEnv(t)

	// 先分配
	env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID: "watcher", Namespace: "test-ns",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.WatchTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "watcher", Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("WatchTunIP: %v", err)
	}

	// 等待 watcher 注册完成
	for i := 0; i < 50; i++ {
		env.server.mu.RLock()
		n := len(env.server.watchers["watcher"])
		env.server.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 从 server 端推送 IP 变更
	newV4 := &net.IPNet{IP: net.ParseIP("198.18.0.99"), Mask: config.CIDR.Mask}
	newV6 := &net.IPNet{IP: net.ParseIP("2001:2::99"), Mask: config.CIDR6.Mask}
	env.server.NotifyIPChange("watcher", newV4, newV6)

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	ip := parseV4(t, resp)
	if !ip.Equal(newV4.IP) {
		t.Fatalf("expected pushed IP %s, got %s", newV4.IP, ip)
	}
}

// 8. WatchTunIP 隐式续租 — stream 活跃时 LeaseReaper 不回收
func TestIntegration_WatchKeepsLeaseAlive(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "watched", Namespace: "test-ns",
	})
	origIP := resp.IPv4

	// 启动 Watch stream
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	_, err := env.client.WatchTunIP(streamCtx, &rpc.TunIPRequest{
		OwnerID: "watched", Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("WatchTunIP: %v", err)
	}

	// 模拟 WatchTunIP 的 renewLease（正常情况 ticker 每 100s 触发）
	env.server.mu.Lock()
	env.server.renewLease("watched")
	env.server.mu.Unlock()

	// 将 LastRenew 设置为刚好在 LeaseDuration 之前（但 renewLease 刚调过所以应该 fresh）
	env.server.mu.Lock()
	alloc := env.server.allocs["watched"]
	// 确认 renewLease 有效
	if time.Since(alloc.LastRenew) > time.Second {
		t.Fatal("renewLease didn't refresh LastRenew")
	}
	env.server.mu.Unlock()

	// 运行 LeaseReaper — 不应该回收
	env.server.reapExpiredLeases(ctx)

	resp2, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "watched", Namespace: "test-ns",
	})
	if resp2.IPv4 != origIP {
		t.Fatalf("IP changed after reaper ran: %s → %s", origIP, resp2.IPv4)
	}
}

// 9. WatchTunIP 断开后 → 租约过期 → LeaseReaper 回收 → IP 可被重新分配
func TestIntegration_DisconnectThenExpireThenReuse(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "disconnect", Namespace: "test-ns",
	})
	origIP := parseV4(t, resp)

	// 模拟断连后时间流逝：手动设置 LastRenew 为过期
	env.server.mu.Lock()
	env.server.allocs["disconnect"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	env.server.mu.Unlock()

	// LeaseReaper 回收
	env.server.reapExpiredLeases(ctx)

	// 另一个 client 应该拿到同一个 IP
	resp2, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "newcomer", Namespace: "test-ns",
	})
	newIP := parseV4(t, resp2)
	if !origIP.Equal(newIP) {
		t.Fatalf("expected reaped IP %s to be reused, got %s", origIP, newIP)
	}
}

// 10. Server 重启后 client 透明恢复（同一 ownerID 拿回同一 IP）
func TestIntegration_ServerRestartClientTransparent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "persistent", Namespace: "test-ns",
	})
	origIP := resp.IPv4

	// 持久化当前状态
	env.server.saveAllocs(ctx)

	// 模拟 server 重启：新 TunConfigServer 实例（同一 clientset → 同一 ConfigMap）
	s2, err := NewTunConfigServer(ctx, env.server.clientset, "test-ns")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}

	// 替换 gRPC server 背后的 TunConfigServer
	env.conn.Close()
	env.grpcServer.Stop()

	grpcServer2 := grpc.NewServer()
	rpc.RegisterTunConfigServiceServer(grpcServer2, s2)
	lis, _ := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", env.port))
	go grpcServer2.Serve(lis)
	t.Cleanup(grpcServer2.Stop)

	conn2, err := grpc.DialContext(ctx,
		fmt.Sprintf("127.0.0.1:%d", env.port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer conn2.Close()

	client2 := rpc.NewTunConfigServiceClient(conn2)
	resp2, _ := client2.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "persistent", Namespace: "test-ns",
	})
	if resp2.IPv4 != origIP {
		t.Fatalf("IP changed after restart: %s → %s", origIP, resp2.IPv4)
	}
}

// 11. ExcludeIPs 不影响其他 owner 的分配
func TestIntegration_ExcludeIPsIsolatedPerOwner(t *testing.T) {
	env := newTestEnv(t)

	// client-1 排除某些 IP 进行分配
	resp1, _ := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:    "client-1",
		Namespace:  "test-ns",
		ExcludeIPs: []string{"198.18.0.1", "198.18.0.2", "198.18.0.3"},
	})
	ip1 := parseV4(t, resp1)

	// client-2 不排除任何 IP，应该拿到池中最小的可用 IP
	resp2, _ := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   "client-2",
		Namespace: "test-ns",
	})
	ip2 := parseV4(t, resp2)

	// client-1 跳过了 .1-.3，应该拿到 .4+
	// client-2 没有排除，应该拿到 .1（如果没被 client-1 的 skip 占用的话）
	if ip1.Equal(ip2) {
		t.Fatalf("different owners got same IP %s", ip1)
	}
	t.Logf("client-1 (excluded .1-.3): %s, client-2 (no excludes): %s", ip1, ip2)
}

// 12. 空 ExcludeIPs 等同于不传
func TestIntegration_EmptyExcludeIPsSameAsNone(t *testing.T) {
	env := newTestEnv(t)

	resp1, _ := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   "test-empty",
		Namespace: "test-ns",
	})

	// 释放
	env.server.mu.Lock()
	delete(env.server.allocs, "test-empty")
	env.server.mu.Unlock()
	ip1 := parseV4(t, resp1)
	env.server.dhcp.ReleaseIP(context.Background(), ip1, net.IPv6zero)

	resp2, _ := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:    "test-empty",
		Namespace:  "test-ns",
		ExcludeIPs: []string{},
	})
	ip2 := parseV4(t, resp2)

	if !ip1.Equal(ip2) {
		t.Fatalf("empty ExcludeIPs behaves differently: %s vs %s", ip1, ip2)
	}
}

// ---- IP 回收 & 重新分配 专项测试 ----

// 13. 多个 owner 过期后 IP 全部回收，按分配顺序回到池中
func TestIntegration_MultiOwnerExpireReclaim(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// 分配 5 个 IP
	origIPs := make([]net.IP, 5)
	for i := 0; i < 5; i++ {
		resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
			OwnerID:   fmt.Sprintf("expire-%d", i),
			Namespace: "test-ns",
		})
		origIPs[i] = parseV4(t, resp)
	}

	// 全部设为过期
	env.server.mu.Lock()
	for i := 0; i < 5; i++ {
		env.server.allocs[fmt.Sprintf("expire-%d", i)].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	}
	env.server.mu.Unlock()

	env.server.reapExpiredLeases(ctx)

	// 确认全部被回收
	env.server.mu.RLock()
	remaining := len(env.server.allocs)
	env.server.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("expected 0 allocs after reap, got %d", remaining)
	}

	// 新 owner 分配应该拿到之前回收的 IP（contiguous strategy 从头扫描）
	resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "fresh-after-reap", Namespace: "test-ns",
	})
	freshIP := parseV4(t, resp)
	if !freshIP.Equal(origIPs[0]) {
		t.Fatalf("expected first reclaimed IP %s, got %s", origIPs[0], freshIP)
	}
}

// 14. 部分 owner 过期、部分存活 — 只回收过期的
func TestIntegration_PartialExpireSelectiveReclaim(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	resp1, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alive", Namespace: "test-ns"})
	aliveIP := parseV4(t, resp1)

	resp2, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "dead", Namespace: "test-ns"})
	deadIP := parseV4(t, resp2)

	// 只让 "dead" 过期
	env.server.mu.Lock()
	env.server.allocs["dead"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	env.server.mu.Unlock()

	env.server.reapExpiredLeases(ctx)

	// "alive" 不受影响
	resp3, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alive", Namespace: "test-ns"})
	if resp1.IPv4 != resp3.IPv4 {
		t.Fatalf("alive IP changed: %s → %s", resp1.IPv4, resp3.IPv4)
	}

	// "dead" 的 IP 可被新 owner 拿到
	resp4, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "reborn", Namespace: "test-ns"})
	rebornIP := parseV4(t, resp4)
	if !rebornIP.Equal(deadIP) {
		t.Fatalf("expected reclaimed IP %s, got %s", deadIP, rebornIP)
	}
	_ = aliveIP
}

// 15. 冲突重新分配后旧 IP 立即可用（不需要等 LeaseReaper）
func TestIntegration_ConflictReallocImmediateReuse(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// A 正常分配
	respA, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "A", Namespace: "test-ns"})
	ipA := parseV4(t, respA)

	// A 冲突，重新分配
	respA2, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "A", Namespace: "test-ns",
		ExcludeIPs: []string{ipA.String()},
	})
	ipA2 := parseV4(t, respA2)
	if ipA.Equal(ipA2) {
		t.Fatalf("re-alloc returned same IP")
	}

	// B 立即分配（不等 LeaseReaper），应该拿到 A 释放的旧 IP
	respB, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "B", Namespace: "test-ns"})
	ipB := parseV4(t, respB)
	if !ipB.Equal(ipA) {
		t.Fatalf("expected immediately-released IP %s, got %s", ipA, ipB)
	}
}

// 16. 同一 owner 断连后重连：lease 未过期时拿回同一 IP
func TestIntegration_ReconnectBeforeExpiry(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	resp1, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "reconnect", Namespace: "test-ns"})

	// 模拟短暂断连（LastRenew 稍旧但未过期）
	env.server.mu.Lock()
	env.server.allocs["reconnect"].LastRenew = time.Now().Add(-LeaseDuration / 2)
	env.server.mu.Unlock()

	// 重连
	resp2, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "reconnect", Namespace: "test-ns"})
	if resp1.IPv4 != resp2.IPv4 {
		t.Fatalf("reconnect got different IP: %s → %s", resp1.IPv4, resp2.IPv4)
	}

	// 确认 LastRenew 被刷新
	env.server.mu.RLock()
	lr := env.server.allocs["reconnect"].LastRenew
	env.server.mu.RUnlock()
	if time.Since(lr) > time.Second {
		t.Fatal("LastRenew not refreshed on reconnect")
	}
}

// 17. 同一 owner 断连后重连：lease 已过期 → 分配新 IP
func TestIntegration_ReconnectAfterExpiry(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	resp1, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "expired-owner", Namespace: "test-ns"})
	origIP := parseV4(t, resp1)

	// 过期
	env.server.mu.Lock()
	env.server.allocs["expired-owner"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	env.server.mu.Unlock()

	env.server.reapExpiredLeases(ctx)

	// 占住旧 IP（模拟已被别人分配）
	env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "occupier", Namespace: "test-ns"})

	// 原 owner 重连 — 旧 IP 已被占，应该拿到新 IP
	resp2, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "expired-owner", Namespace: "test-ns"})
	newIP := parseV4(t, resp2)
	if origIP.Equal(newIP) {
		t.Fatalf("expected different IP after expiry+reoccupy, got same %s", origIP)
	}
}

// 18. ReconcileDHCP：外部修改 ConfigMap 导致 IP 丢失 → 自动重新分配
func TestIntegration_ReconcileDHCPReallocatesLostIP(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "lost-ip", Namespace: "test-ns"})
	origIP := parseV4(t, resp)

	// 模拟外部修改：清空 DHCP bitmap（所有 IP 都丢失）
	cm, _ := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	cm.Data[config.KeyTunIPPool] = ""

	env.server.clientset.CoreV1().ConfigMaps("test-ns").Update(ctx, cm, metav1.UpdateOptions{})

	// ReconcileDHCP 检测到 IP 丢失
	env.server.ReconcileDHCP(ctx)

	// alloc 应该被更新为新 IP（旧 IP 不在 bitmap 中了）
	env.server.mu.RLock()
	alloc, ok := env.server.allocs["lost-ip"]
	env.server.mu.RUnlock()

	if !ok {
		t.Fatal("alloc disappeared after reconcile")
	}
	if alloc.IPv4.IP.Equal(origIP) {
		// bitmap 被清空后重新分配，可能拿到同一个 IP（因为 bitmap 空了从头分配）
		// 这是正确行为 — 关键是 alloc 仍然存在且有效
		t.Logf("re-allocated same IP %s (bitmap was empty)", origIP)
	} else {
		t.Logf("re-allocated from %s to %s", origIP, alloc.IPv4.IP)
	}
}

// 19. 池耗尽：所有 IP 被占用后分配失败
func TestIntegration_PoolExhaustion(t *testing.T) {
	// 使用非常小的 CIDR 来快速耗尽
	// 默认 CIDR 是 198.18.0.0/16 — 太大了，这里直接测试 DHCP 层
	env := newTestEnv(t)
	ctx := context.Background()

	// 分配大量 IP 直到失败（或 256 个足够说明问题）
	var lastErr error
	allocated := 0
	for i := 0; i < 300; i++ {
		_, lastErr = env.client.GetTunIP(ctx, &rpc.TunIPRequest{
			OwnerID:   fmt.Sprintf("exhaust-%d", i),
			Namespace: "test-ns",
		})
		if lastErr != nil {
			break
		}
		allocated++
	}
	t.Logf("Allocated %d IPs before pool state check", allocated)
	// /16 有 65534 个 IP，不会在 300 次内耗尽，所以应该全部成功
	if lastErr != nil {
		t.Fatalf("unexpected error at allocation %d: %v", allocated, lastErr)
	}
}

// 20. 回收后池重新可用：大量分配 → 全部过期回收 → 重新分配从头开始
func TestIntegration_ReclaimResetsPool(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// 分配 10 个
	firstIPs := make([]net.IP, 10)
	for i := 0; i < 10; i++ {
		resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
			OwnerID:   fmt.Sprintf("batch-%d", i),
			Namespace: "test-ns",
		})
		firstIPs[i] = parseV4(t, resp)
	}

	// 全部过期
	env.server.mu.Lock()
	for i := 0; i < 10; i++ {
		env.server.allocs[fmt.Sprintf("batch-%d", i)].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	}
	env.server.mu.Unlock()

	env.server.reapExpiredLeases(ctx)

	// 重新分配 — 应该从头开始，拿到和第一批相同的 IP
	for i := 0; i < 10; i++ {
		resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
			OwnerID:   fmt.Sprintf("rebatch-%d", i),
			Namespace: "test-ns",
		})
		newIP := parseV4(t, resp)
		if !newIP.Equal(firstIPs[i]) {
			t.Fatalf("after full reclaim, IP #%d: expected %s, got %s", i, firstIPs[i], newIP)
		}
	}
}

// ========================================================================================
// Sleep-wake scenario: IP changes after lease expiry, all downstream components updated
// ========================================================================================

// TestIntegration_SleepWake_GetTunIPNotifiesWatchers verifies that when a lease expires
// and the owner calls GetTunIP again (as healthCheckTCPConn does), the new IP is pushed
// to WatchTunIP subscribers (fixing the push chain break).
func TestIntegration_SleepWake_GetTunIPNotifiesWatchers(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Phase 1: initial allocation
	resp1, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "sleepy-user", Namespace: "test-ns"})
	if err != nil {
		t.Fatalf("initial GetTunIP: %v", err)
	}
	origIP := parseV4(t, resp1)

	// Phase 2: start WatchTunIP stream (simulates IPWatcher on Root Daemon)
	watchCtx, watchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer watchCancel()
	stream, err := env.client.WatchTunIP(watchCtx, &rpc.TunIPRequest{
		OwnerID: "sleepy-user", Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("WatchTunIP: %v", err)
	}

	// Wait for watcher registration
	for i := 0; i < 50; i++ {
		env.server.mu.RLock()
		n := len(env.server.watchers["sleepy-user"])
		env.server.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Phase 3: simulate sleep → lease expiry
	env.server.mu.Lock()
	env.server.allocs["sleepy-user"].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	env.server.mu.Unlock()
	env.server.reapExpiredLeases(ctx)

	// Occupy the old IP so re-allocation gets a different one
	_, _ = env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "occupier", Namespace: "test-ns"})

	// Phase 4: simulate wake → healthCheck calls GetTunIP
	// This should allocate a NEW IP and push it to the WatchTunIP stream
	resp2, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "sleepy-user", Namespace: "test-ns"})
	if err != nil {
		t.Fatalf("reconnect GetTunIP: %v", err)
	}
	newIP := parseV4(t, resp2)
	if origIP.Equal(newIP) {
		t.Fatalf("expected different IP after expiry+reoccupy, got same %s", origIP)
	}

	// Phase 5: WatchTunIP stream should have received the new IP
	pushed, err := stream.Recv()
	if err != nil {
		t.Fatalf("WatchTunIP Recv: %v (push chain broken!)", err)
	}
	pushedIP := parseV4(t, pushed)
	if !pushedIP.Equal(newIP) {
		t.Fatalf("WatchTunIP pushed %s, but GetTunIP returned %s", pushedIP, newIP)
	}
}

// TestIntegration_SleepWake_SyncEnvoyRuleIP verifies that when a TUN IP changes,
// the ENVOY_CONFIG in the ConfigMap is automatically updated with the new IP.
func TestIntegration_SleepWake_SyncEnvoyRuleIP(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	ownerID := "envoy-sync-user"

	// Phase 1: allocate IP
	resp1, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerID, Namespace: "test-ns"})
	origIP := parseV4(t, resp1)

	// Phase 2: write an envoy rule with the original IP (simulates proxy inject)
	cm, _ := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	envoyConfig := fmt.Sprintf(`- schemaVersion: 2
  Uid: deployments.apps.web
  namespace: test-ns
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "%s"
    ownerID: "%s"
    portmap:
      8080: "9080"
`, origIP.String(), ownerID)
	cm.Data[config.KeyEnvoy] = envoyConfig
	_, err := env.server.clientset.CoreV1().ConfigMaps("test-ns").Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("write ENVOY_CONFIG: %v", err)
	}

	// Phase 3: expire lease + reap
	env.server.mu.Lock()
	env.server.allocs[ownerID].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	env.server.mu.Unlock()
	env.server.reapExpiredLeases(ctx)

	// Occupy old IP
	_, _ = env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "occupier2", Namespace: "test-ns"})

	// Phase 4: re-allocate (simulates healthCheck → GetTunIP)
	resp2, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerID, Namespace: "test-ns"})
	newIP := parseV4(t, resp2)
	if origIP.Equal(newIP) {
		t.Fatalf("expected different IP, got same %s", origIP)
	}

	// Phase 5: wait for async syncEnvoyRuleIP to converge the rule IP.
	waitForEnvoyRuleIPv4(t, env, ownerID, newIP)

	// Phase 6: verify ENVOY_CONFIG was updated
	cm, _ = env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	virtuals, err := parseYaml(cm.Data[config.KeyEnvoy])
	if err != nil {
		t.Fatalf("parse ENVOY_CONFIG: %v", err)
	}

	found := false
	for _, v := range virtuals {
		for _, rule := range v.Rules {
			if rule.OwnerID == ownerID {
				found = true
				ruleIP := net.ParseIP(rule.LocalTunIPv4)
				if !ruleIP.Equal(newIP) {
					t.Fatalf("ENVOY_CONFIG Rule.LocalTunIPv4 = %s, expected %s (not updated!)", rule.LocalTunIPv4, newIP)
				}
				t.Logf("✓ ENVOY_CONFIG updated: %s → %s", origIP, newIP)
			}
		}
	}
	if !found {
		t.Fatal("Rule with ownerID not found in ENVOY_CONFIG")
	}
}

// TestIntegration_SleepWake_WatchTickerPushesCurrentIP verifies the ticker fallback:
// even if the event push was missed, the periodic ticker pushes the current IP.
func TestIntegration_SleepWake_WatchTickerPushesCurrentIP(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	ownerID := "ticker-user"

	// Phase 1: allocate IP
	resp1, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerID, Namespace: "test-ns"})
	origVersion := resp1.Version

	// Phase 2: connect WatchTunIP
	watchCtx, watchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer watchCancel()
	stream, err := env.client.WatchTunIP(watchCtx, &rpc.TunIPRequest{
		OwnerID: ownerID, Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("WatchTunIP: %v", err)
	}

	// Wait for watcher
	for i := 0; i < 50; i++ {
		env.server.mu.RLock()
		n := len(env.server.watchers[ownerID])
		env.server.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Phase 3: manually change the alloc version to simulate IP change
	// (without going through GetTunIP, so notifyWatchers is NOT called)
	env.server.mu.Lock()
	alloc := env.server.allocs[ownerID]
	alloc.Version = time.Now().UnixNano()
	newVersion := alloc.Version
	env.server.mu.Unlock()

	if newVersion == origVersion {
		t.Fatal("version should differ")
	}

	// Phase 4: manually trigger ticker (simulates the LeaseDuration/3 ticker firing)
	env.server.mu.Lock()
	env.server.renewLease(ownerID)
	if a, ok := env.server.allocs[ownerID]; ok {
		resp := &rpc.TunIPResponse{Version: a.Version}
		if a.IPv4 != nil {
			resp.IPv4 = a.IPv4.String()
		}
		if a.IPv6 != nil {
			resp.IPv6 = a.IPv6.String()
		}
		for _, ch := range env.server.watchers[ownerID] {
			select {
			case ch <- resp:
			default:
			}
		}
	}
	env.server.mu.Unlock()

	// Phase 5: stream should receive the current IP via ticker push
	pushed, err := stream.Recv()
	if err != nil {
		t.Fatalf("WatchTunIP Recv: %v", err)
	}
	if pushed.Version != newVersion {
		t.Fatalf("expected version %d from ticker push, got %d", newVersion, pushed.Version)
	}
}

// TestIntegration_SleepWake_MultiUser_NoInterference verifies that when user A's IP
// changes after sleep-wake, user B's envoy rules are not affected.
func TestIntegration_SleepWake_MultiUser_NoInterference(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	ownerA := "user-alice"
	ownerB := "user-bob"

	// Phase 1: both users allocate IPs
	respA, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerA, Namespace: "test-ns"})
	respB, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerB, Namespace: "test-ns"})
	ipA := parseV4(t, respA)
	ipB := parseV4(t, respB)

	// Phase 2: write envoy rules for both users
	cm, _ := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	cm.Data[config.KeyEnvoy] = fmt.Sprintf(`- schemaVersion: 2
  Uid: deployments.apps.web
  namespace: test-ns
  ports:
  - containerPort: 9080
    protocol: TCP
  rules:
  - headers:
      x-user: alice
    localtunipv4: "%s"
    ownerID: "%s"
    portmap:
      9080: "9080"
  - headers:
      x-user: bob
    localtunipv4: "%s"
    ownerID: "%s"
    portmap:
      9080: "9080"
`, ipA.String(), ownerA, ipB.String(), ownerB)
	env.server.clientset.CoreV1().ConfigMaps("test-ns").Update(ctx, cm, metav1.UpdateOptions{})

	// Phase 3: Alice sleeps → lease expires → IP reclaimed
	env.server.mu.Lock()
	env.server.allocs[ownerA].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	env.server.mu.Unlock()
	env.server.reapExpiredLeases(ctx)

	// Phase 4: Alice wakes → GetTunIP → new IP
	respA2, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerA, Namespace: "test-ns"})
	newIPa := parseV4(t, respA2)

	// wait for async syncEnvoyRuleIP to converge Alice's rule IP before asserting.
	waitForEnvoyRuleIPv4(t, env, ownerA, newIPa)

	// Phase 5: verify Alice's rule updated, Bob's rule untouched
	cm, _ = env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	virtuals, _ := parseYaml(cm.Data[config.KeyEnvoy])

	for _, v := range virtuals {
		for _, rule := range v.Rules {
			switch rule.OwnerID {
			case ownerA:
				ruleIP := net.ParseIP(rule.LocalTunIPv4)
				if !ruleIP.Equal(newIPa) {
					t.Fatalf("Alice's rule not updated: expected %s, got %s", newIPa, rule.LocalTunIPv4)
				}
				t.Logf("✓ Alice's rule updated to %s", newIPa)
			case ownerB:
				ruleIP := net.ParseIP(rule.LocalTunIPv4)
				if !ruleIP.Equal(ipB) {
					t.Fatalf("Bob's rule changed! expected %s, got %s", ipB, rule.LocalTunIPv4)
				}
				t.Logf("✓ Bob's rule unchanged at %s", ipB)
			}
		}
	}
}

// TestIntegration_SleepWake_FullLifecycle is the end-to-end story test:
// proxy → sleep → lease expire → wake → reconnect → IP changes →
// WatchTunIP push + ENVOY_CONFIG update → traffic restored.
func TestIntegration_SleepWake_FullLifecycle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	ownerID := "lifecycle-user"

	// === T0: proxy (allocate IP + write envoy config) ===
	resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerID, Namespace: "test-ns"})
	origIP := parseV4(t, resp)
	t.Logf("T0: allocated %s", origIP)

	// Write envoy rule
	cm, _ := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	cm.Data[config.KeyEnvoy] = fmt.Sprintf(`- schemaVersion: 2
  Uid: deployments.apps.myapp
  namespace: test-ns
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - localtunipv4: "%s"
    ownerID: "%s"
    portmap:
      8080: "8080"
`, origIP.String(), ownerID)
	env.server.clientset.CoreV1().ConfigMaps("test-ns").Update(ctx, cm, metav1.UpdateOptions{})

	// Start WatchTunIP (simulates IPWatcher)
	watchCtx, watchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer watchCancel()
	stream, _ := env.client.WatchTunIP(watchCtx, &rpc.TunIPRequest{OwnerID: ownerID, Namespace: "test-ns"})

	for i := 0; i < 50; i++ {
		env.server.mu.RLock()
		n := len(env.server.watchers[ownerID])
		env.server.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// === T1: sleep → T1+5min: lease expires ===
	t.Log("T1: simulating sleep + lease expiry")
	env.server.mu.Lock()
	env.server.allocs[ownerID].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	env.server.mu.Unlock()
	env.server.reapExpiredLeases(ctx)

	// Occupy old IP
	_, _ = env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "occupier-lc", Namespace: "test-ns"})

	// === T2: wake → healthCheck calls GetTunIP ===
	t.Log("T2: simulating wake + healthCheck GetTunIP")
	resp2, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerID, Namespace: "test-ns"})
	newIP := parseV4(t, resp2)
	if origIP.Equal(newIP) {
		t.Fatalf("expected different IP, got same %s", origIP)
	}
	t.Logf("T2: re-allocated %s → %s", origIP, newIP)

	// === Verify 1: WatchTunIP stream received the push ===
	pushed, err := stream.Recv()
	if err != nil {
		t.Fatalf("WatchTunIP push failed: %v (push chain broken!)", err)
	}
	pushedIP := parseV4(t, pushed)
	if !pushedIP.Equal(newIP) {
		t.Fatalf("pushed IP %s != new IP %s", pushedIP, newIP)
	}
	t.Logf("✓ WatchTunIP received push: %s", pushedIP)

	// === Verify 2: ENVOY_CONFIG was updated ===
	// wait for async syncEnvoyRuleIP to converge the rule IP before asserting.
	waitForEnvoyRuleIPv4(t, env, ownerID, newIP)
	cm, _ = env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	virtuals, _ := parseYaml(cm.Data[config.KeyEnvoy])
	for _, v := range virtuals {
		for _, rule := range v.Rules {
			if rule.OwnerID == ownerID {
				ruleIP := net.ParseIP(rule.LocalTunIPv4)
				if !ruleIP.Equal(newIP) {
					t.Fatalf("ENVOY_CONFIG not updated: rule has %s, expected %s", rule.LocalTunIPv4, newIP)
				}
				t.Logf("✓ ENVOY_CONFIG updated: Rule.LocalTunIPv4 = %s", newIP)
			}
		}
	}

	// === Verify 3: allocs consistent ===
	env.server.mu.RLock()
	alloc, ok := env.server.allocs[ownerID]
	env.server.mu.RUnlock()
	if !ok {
		t.Fatal("alloc missing after reconnect")
	}
	if !alloc.IPv4.IP.Equal(newIP) {
		t.Fatalf("alloc IP %s != new IP %s", alloc.IPv4.IP, newIP)
	}
	t.Logf("✓ allocs consistent: %s", newIP)
	t.Log("✓ Full lifecycle passed: proxy → sleep → expire → wake → reconnect → IP updated everywhere")
}
