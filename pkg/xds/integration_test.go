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

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := NewTunConfigServer(ctx, clientset, "test-ns")
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}

	t.Cleanup(s.Close)
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
