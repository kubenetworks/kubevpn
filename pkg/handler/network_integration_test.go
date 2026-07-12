package handler

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

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// networkTestEnv bundles a TunConfigServer + gRPC server for testing NetworkManager.
type networkTestEnv struct {
	server     *controlplane.TunConfigServer
	grpcServer *grpc.Server
	port       int
}

func newNetworkTestEnv(t *testing.T) *networkTestEnv {
	t.Helper()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-net-test"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data: map[string]string{
				config.KeyTunIPPool: "",
				config.KeyEnvoy: "",
			},
		},
	)
	s, err := controlplane.NewTunConfigServer(context.Background(), clientset, "test-ns")
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

	t.Cleanup(grpcServer.Stop)
	return &networkTestEnv{server: s, grpcServer: grpcServer, port: port}
}

func newTestNetworkManager(t *testing.T, env *networkTestEnv, ownerID string) *NetworkManager {
	t.Helper()
	nm := newNetworkManager(NetworkConfig{
		ManagerNamespace: "test-ns",
		OwnerID:          ownerID,
	})
	nm.controlPlaneLocalPort = env.port
	return nm
}

// ---- NetworkManager.rentIP 集成测试 ----

// 1. rentIP 基本流程：通过 gRPC 从 TunConfigServer 获取 IP
func TestNetworkManager_RentIP_Basic(t *testing.T) {
	env := newNetworkTestEnv(t)
	nm := newTestNetworkManager(t, env, "nm-basic")

	err := nm.rentIP(context.Background())
	if err != nil {
		t.Fatalf("rentIP: %v", err)
	}
	if nm.localTunIPv4 == nil {
		t.Fatal("localTunIPv4 is nil after rentIP")
	}
	if !config.CIDR.Contains(nm.localTunIPv4.IP) {
		t.Fatalf("IP %s not in CIDR %s", nm.localTunIPv4, config.CIDR)
	}
	if nm.localTunIPv6 == nil {
		t.Fatal("localTunIPv6 is nil after rentIP")
	}
	t.Logf("Rented: v4=%s v6=%s", nm.localTunIPv4, nm.localTunIPv6)
}

// 2. rentIP 同一 ownerID 多次调用拿到同一 IP
func TestNetworkManager_RentIP_Idempotent(t *testing.T) {
	env := newNetworkTestEnv(t)
	nm := newTestNetworkManager(t, env, "nm-idem")

	nm.rentIP(context.Background())
	ip1 := nm.localTunIPv4.IP.String()

	// 清空本地状态，再次调用
	nm.localTunIPv4 = nil
	nm.localTunIPv6 = nil
	nm.rentIP(context.Background())
	ip2 := nm.localTunIPv4.IP.String()

	if ip1 != ip2 {
		t.Fatalf("non-idempotent: %s vs %s", ip1, ip2)
	}
}

// 3. 两个 NetworkManager 不同 ownerID → 不同 IP
func TestNetworkManager_RentIP_TwoOwners(t *testing.T) {
	env := newNetworkTestEnv(t)
	nm1 := newTestNetworkManager(t, env, "owner-a")
	nm2 := newTestNetworkManager(t, env, "owner-b")

	nm1.rentIP(context.Background())
	nm2.rentIP(context.Background())

	if nm1.localTunIPv4.IP.Equal(nm2.localTunIPv4.IP) {
		t.Fatalf("two owners got same IP: %s", nm1.localTunIPv4)
	}
}

// 4. rentIP 自动发送 ExcludeIPs（本地接口 IP 不会被分配）
func TestNetworkManager_RentIP_ExcludesLocalIPs(t *testing.T) {
	env := newNetworkTestEnv(t)
	nm := newTestNetworkManager(t, env, "nm-exclude")

	nm.rentIP(context.Background())

	// 验证分配到的 IP 不在本地接口上
	if isLocalIPConflict(nm.localTunIPv4.IP) {
		t.Fatalf("rentIP returned locally-conflicting IP: %s", nm.localTunIPv4.IP)
	}
}

// 5. collectLocalIPs 返回格式正确的 IP 字符串列表
func TestCollectLocalIPs(t *testing.T) {
	ips := collectLocalIPs()
	if len(ips) == 0 {
		t.Fatal("collectLocalIPs returned empty (expected at least loopback)")
	}
	for _, s := range ips {
		if net.ParseIP(s) == nil {
			t.Fatalf("collectLocalIPs returned invalid IP string: %q", s)
		}
	}
	// 127.0.0.1 should be in the list
	found := false
	for _, s := range ips {
		if s == "127.0.0.1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 127.0.0.1 in collectLocalIPs, got %v", ips)
	}
}

// 6. 多个 NetworkManager 模拟多集群连接
func TestNetworkManager_RentIP_MultiCluster(t *testing.T) {
	env := newNetworkTestEnv(t)

	nms := make([]*NetworkManager, 5)
	for i := range nms {
		nms[i] = newTestNetworkManager(t, env, fmt.Sprintf("cluster-%d", i))
		if err := nms[i].rentIP(context.Background()); err != nil {
			t.Fatalf("cluster %d: %v", i, err)
		}
	}

	seen := make(map[string]int)
	for i, nm := range nms {
		ip := nm.localTunIPv4.IP.String()
		if prev, ok := seen[ip]; ok {
			t.Fatalf("clusters %d and %d got same IP %s", prev, i, ip)
		}
		seen[ip] = i
	}
}

// 7. rentIP 在 server 不可达时返回错误
func TestNetworkManager_RentIP_ServerDown(t *testing.T) {
	nm := newNetworkManager(NetworkConfig{
		ManagerNamespace: "test-ns",
		OwnerID:          "no-server",
	})
	nm.controlPlaneLocalPort = 19999 // 没有 server 监听的端口

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := nm.rentIP(ctx)
	if err == nil {
		t.Fatal("expected error when server is down")
	}
}

// 8. rentIP 后 server 端持有正确的 ownerID → IP 映射
func TestNetworkManager_RentIP_ServerSideState(t *testing.T) {
	env := newNetworkTestEnv(t)
	nm := newTestNetworkManager(t, env, "verify-server")

	nm.rentIP(context.Background())

	// 直接通过 gRPC 查询 server 端的状态
	conn, _ := grpc.DialContext(context.Background(),
		fmt.Sprintf("127.0.0.1:%d", env.port),
		grpc.WithInsecure(), grpc.WithBlock(),
	)
	defer conn.Close()

	client := rpc.NewTunConfigServiceClient(conn)
	resp, err := client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   "verify-server",
		Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("server query: %v", err)
	}

	ip, _, _ := net.ParseCIDR(resp.IPv4)
	if !ip.Equal(nm.localTunIPv4.IP) {
		t.Fatalf("server has %s, client has %s", ip, nm.localTunIPv4.IP)
	}
}

// 9. 冲突导致 server 端重新分配后，原 IP 可被其他 owner 获取
func TestNetworkManager_RentIP_ConflictReleasesForOthers(t *testing.T) {
	env := newNetworkTestEnv(t)

	// owner-A 分配
	nmA := newTestNetworkManager(t, env, "owner-A")
	nmA.rentIP(context.Background())
	ipA := nmA.localTunIPv4.IP.String()

	// owner-A 通过 gRPC 带 ExcludeIPs 重新分配（模拟冲突）
	conn, _ := grpc.DialContext(context.Background(),
		fmt.Sprintf("127.0.0.1:%d", env.port),
		grpc.WithInsecure(), grpc.WithBlock(),
	)
	defer conn.Close()
	client := rpc.NewTunConfigServiceClient(conn)
	resp, _ := client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:    "owner-A",
		Namespace:  "test-ns",
		ExcludeIPs: []string{ipA},
	})
	ipA2, _, _ := net.ParseCIDR(resp.IPv4)
	if ipA2.String() == ipA {
		t.Fatalf("re-alloc returned same IP %s", ipA)
	}

	// owner-B 应该拿到 owner-A 释放的旧 IP
	nmB := newTestNetworkManager(t, env, "owner-B")
	nmB.rentIP(context.Background())
	if nmB.localTunIPv4.IP.String() != ipA {
		t.Fatalf("expected released IP %s, got %s", ipA, nmB.localTunIPv4.IP)
	}
}

// 10. doWatchTunIP 接收 server 端 IP 变更推送
func TestNetworkManager_WatchTunIP_ReceivesPush(t *testing.T) {
	env := newNetworkTestEnv(t)
	nm := newTestNetworkManager(t, env, "watch-owner")

	nm.rentIP(context.Background())

	nm.tunName = "test-tun"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var currentVersion int64
	target := fmt.Sprintf("127.0.0.1:%d", env.port)

	watchDone := make(chan error, 1)
	go func() {
		watchDone <- nm.doWatchTunIP(ctx, target, &currentVersion)
	}()

	// 等待 watcher 注册（轮询 gRPC — 不访问 server 内部）
	time.Sleep(200 * time.Millisecond)

	// 通过公开 API 推送 IP 变更
	newV4 := &net.IPNet{IP: net.ParseIP("198.18.0.99"), Mask: config.CIDR.Mask}
	newV6 := &net.IPNet{IP: net.ParseIP("2001:2::99"), Mask: config.CIDR6.Mask}
	env.server.NotifyIPChange("watch-owner", newV4, newV6)

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-watchDone

	if currentVersion == 0 {
		t.Fatal("watcher did not receive any updates")
	}
}
