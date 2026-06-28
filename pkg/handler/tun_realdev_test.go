package handler

// Real-device end-to-end tests for the dry-run manual-IP feature: they stand up a
// REAL TunConfigService gRPC server (backed by a fake clientset) + a REAL local TUN
// device + the client IPWatcher, then edit TUN_ALLOCS and assert the actual TUN
// device IP changes (confirm) or stays (decline), across single/multi-user edits
// and disconnect / control-plane-restart / client-restart.
//
// These require CAP_NET_ADMIN to create TUN devices, so each test probe-skips when
// it cannot. No build tag: under root (e.g. CI `make ut` via sudo) they run for
// real; otherwise they Skip. Run directly:
//   sudo -E env PATH=$PATH go test ./pkg/handler/ -run TestRealTUNManualIP -v -timeout=300s

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// requireTUN skips the test if a TUN device cannot be created (no CAP_NET_ADMIN).
func requireTUN(t *testing.T) {
	t.Helper()
	ln, err := tun.Listener(tun.Config{Addr: "198.18.254.254/32", MTU: config.DefaultMTU})
	if err != nil {
		t.Skipf("real-TUN E2E needs CAP_NET_ADMIN: %v", err)
	}
	_ = ln.Close()
}

// cpEnv is a real in-process control-plane: TunConfigServer + gRPC on a fixed port,
// backed by a fake clientset whose ConfigMap is the editable TUN_ALLOCS store.
type cpEnv struct {
	server    *controlplane.TunConfigServer
	grpc      *grpc.Server
	port      int
	clientset kubernetes.Interface
	ns        string
}

func newCPEnv(t *testing.T) *cpEnv {
	t.Helper()
	ns := "test-ns"
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: "uid-realdev"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: ""},
		},
	)
	s, err := controlplane.NewTunConfigServer(context.Background(), clientset, ns)
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	g := grpc.NewServer()
	rpc.RegisterTunConfigServiceServer(g, s)
	go g.Serve(lis)
	e := &cpEnv{server: s, grpc: g, port: lis.Addr().(*net.TCPAddr).Port, clientset: clientset, ns: ns}
	t.Cleanup(func() { e.grpc.Stop() })
	return e
}

// restart stops the gRPC server and brings it back on the SAME port (so the client
// watcher, which caches its dial target, can reconnect). fresh=true rebuilds the
// TunConfigServer from scratch (reloading committed state from TUN_ALLOCS), modelling
// a control-plane pod restart; fresh=false reuses the same server (transient outage).
func (e *cpEnv) restart(t *testing.T, fresh bool) {
	t.Helper()
	e.grpc.Stop()
	if fresh {
		s, err := controlplane.NewTunConfigServer(context.Background(), e.clientset, e.ns)
		if err != nil {
			t.Fatalf("restart NewTunConfigServer: %v", err)
		}
		e.server = s
	}
	var lis net.Listener
	var err error
	for i := 0; i < 30; i++ { // SO_REUSEADDR usually lets us rebind immediately; retry briefly
		if lis, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", e.port)); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("re-listen on port %d: %v", e.port, err)
	}
	g := grpc.NewServer()
	rpc.RegisterTunConfigServiceServer(g, e.server)
	go g.Serve(lis)
	e.grpc = g
}

// startOwner = one "user": its own NetworkManager rents an IP, creates a REAL TUN
// device with it, and starts the IP watcher pointed at the control-plane.
func startOwner(t *testing.T, parent context.Context, env *cpEnv, owner string, used map[string]bool) (*NetworkManager, func()) {
	t.Helper()
	nm := newNetworkManager(NetworkConfig{ManagerNamespace: env.ns, OwnerID: owner})
	nm.controlPlaneLocalPort = env.port
	if err := nm.rentIP(parent); err != nil {
		t.Fatalf("rentIP %s: %v", owner, err)
	}
	if used != nil {
		used[nm.localTunIPv4.IP.String()] = true
		if nm.localTunIPv6 != nil {
			used[nm.localTunIPv6.IP.String()] = true
		}
	}
	var v6 net.IP
	if nm.localTunIPv6 != nil {
		v6 = nm.localTunIPv6.IP
	}
	name, devCloser := createDevice(t, nm.localTunIPv4.IP, v6) // dual-stack so v6 is editable
	nm.tunName = name
	octx, cancel := context.WithCancel(parent)
	nm.StartIPWatcher(octx)
	waitWatcher(t, env, owner, 5*time.Second) // ensure subscribed before any push
	return nm, func() { cancel(); devCloser() }
}

// createDevice makes a real TUN device with the given IP and returns its name and a
// closer that ACTUALLY tears it down (the listener wraps the device in a conn that
// must be Accept()ed and Closed — listener.Close alone does not remove the device).
func createDevice(t *testing.T, v4, v6 net.IP) (string, func()) {
	t.Helper()
	cfg := tun.Config{
		Addr: (&net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}).String(),
		MTU:  config.DefaultMTU,
	}
	if v6 != nil {
		cfg.Addr6 = (&net.IPNet{IP: v6, Mask: net.CIDRMask(128, 128)}).String()
	}
	ln, err := tun.Listener(cfg)
	if err != nil {
		t.Skipf("cannot create TUN device (need CAP_NET_ADMIN): %v", err)
	}
	conn, err := ln.Accept()
	if err != nil {
		_ = ln.Close()
		t.Fatalf("accept tun conn: %v", err)
	}
	dev, err := util.GetTunDevice(v4)
	if err != nil {
		_ = conn.Close()
		_ = ln.Close()
		t.Fatalf("resolve tun device for %s: %v", v4, err)
	}
	return dev.Name, func() { _ = conn.Close(); _ = ln.Close() }
}

// waitWatcher blocks until the owner has an active WatchTunIP subscription, so a
// subsequent proposal push is actually delivered (not lost to a not-yet-subscribed
// client and then de-duped away).
func waitWatcher(t *testing.T, env *cpEnv, owner string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for env.server.WatcherCount(owner) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("watcher for %s did not subscribe within %s", owner, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// editTunAllocs writes an operator-style TUN_ALLOCS edit (owner→CIDR) to the CM.
func editTunAllocs(t *testing.T, env *cpEnv, m map[string]string) {
	t.Helper()
	var b strings.Builder
	for owner, cidr := range m {
		fmt.Fprintf(&b, "%s:\n  ipv4: %s\n  version: 1\n  lastRenew: %d\n", owner, cidr, time.Now().Unix())
	}
	ctx := context.Background()
	cm, err := env.clientset.CoreV1().ConfigMaps(env.ns).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[config.KeyTunAllocs] = b.String()
	if _, err := env.clientset.CoreV1().ConfigMaps(env.ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update cm: %v", err)
	}
}

func localHasIP(ip net.IP) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// waitDeviceLacksIP blocks until the named device no longer has ip (the old address
// must be removed when changing IP — regression guard for the "two IPs" bug).
func waitDeviceLacksIP(t *testing.T, name string, ip net.IP, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for deviceHasIP(name, ip) {
		if time.Now().After(deadline) {
			t.Fatalf("device %s still has old IP %s after %s (should have been removed)", name, ip, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitIPGone blocks until ip is no longer assigned to any local interface (the OS
// removes a TUN device's addresses asynchronously after Close).
func waitIPGone(t *testing.T, ip net.IP, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for localHasIP(ip) {
		if time.Now().After(deadline) {
			t.Fatalf("ip %s still present on a local interface after %s", ip, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func deviceHasIP(name string, ip net.IP) bool {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return false
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

func waitDeviceHasIP(t *testing.T, name string, ip net.IP, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if deviceHasIP(name, ip) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("device %s did not get IP %s within %s", name, ip, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pickFreeIP returns an in-pool 198.18.x.x address that is neither a current local
// interface address nor already handed out by this test.
func pickFreeIP(t *testing.T, used map[string]bool) net.IP {
	t.Helper()
	local := map[string]bool{}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			local[n.IP.String()] = true
		}
	}
	for b := 1; b < 255; b++ {
		for c := 1; c < 255; c++ {
			ip := net.IPv4(198, 18, byte(b), byte(c)).To4()
			s := ip.String()
			if !local[s] && !used[s] {
				used[s] = true
				return ip
			}
		}
	}
	t.Fatal("no free IP in pool")
	return nil
}

func cidrPool(ip net.IP) string { return ip.String() + "/16" }

// 1. Single user edits TUN_ALLOCS → the real TUN device IP changes.
func TestRealTUNManualIP_SingleChange(t *testing.T) {
	requireTUN(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := newCPEnv(t)
	used := map[string]bool{}

	nm, closer := startOwner(t, ctx, env, "o1", used)
	defer closer()
	ip1 := nm.localTunIPv4.IP
	waitDeviceHasIP(t, nm.tunName, ip1, 2*time.Second)

	ip2 := pickFreeIP(t, used)
	editTunAllocs(t, env, map[string]string{"o1": cidrPool(ip2)})
	env.server.ReconcileAllocsFromConfigMap(ctx)

	waitDeviceHasIP(t, nm.tunName, ip2, 10*time.Second)
	if !nm.localTunIPv4.IP.Equal(ip2) {
		t.Fatalf("nm.localTunIPv4=%s, want %s", nm.localTunIPv4.IP, ip2)
	}
	waitDeviceLacksIP(t, nm.tunName, ip1, 5*time.Second) // old IP must be removed (no dup)
}

// 2. Edit to an IP that conflicts with another local TUN device → client declines,
//    device unchanged, ConfigMap annotated.
func TestRealTUNManualIP_DeclineConflict(t *testing.T) {
	requireTUN(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := newCPEnv(t)
	used := map[string]bool{}

	nm, closer := startOwner(t, ctx, env, "o1", used)
	defer closer()
	ip1 := nm.localTunIPv4.IP

	// A second real device occupies IPS, making it a local interface (a "sibling").
	ips := pickFreeIP(t, used)
	_, sibCloser := createDevice(t, ips, nil)
	defer sibCloser()

	editTunAllocs(t, env, map[string]string{"o1": cidrPool(ips)})
	env.server.ReconcileAllocsFromConfigMap(ctx)

	// Give the client time to receive the proposal, validate, and decline.
	time.Sleep(2 * time.Second)
	if deviceHasIP(nm.tunName, ips) {
		t.Fatalf("o1 device wrongly adopted conflicting IP %s", ips)
	}
	if !deviceHasIP(nm.tunName, ip1) {
		t.Fatalf("o1 device lost its original IP %s", ip1)
	}
	// Annotation breadcrumb (written async on decline).
	deadline := time.Now().Add(3 * time.Second)
	for {
		cm, _ := env.clientset.CoreV1().ConfigMaps(env.ns).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if _, ok := cm.Annotations["kubevpn.io/tun-allocs-rejected"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected tun-allocs-rejected annotation after decline")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 3. Multiple users edited to DIFFERENT IPs → each device changes independently.
func TestRealTUNManualIP_MultiUserDifferentIP(t *testing.T) {
	requireTUN(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := newCPEnv(t)
	used := map[string]bool{}

	nm1, c1 := startOwner(t, ctx, env, "o1", used)
	defer c1()
	nm2, c2 := startOwner(t, ctx, env, "o2", used)
	defer c2()

	ipa := pickFreeIP(t, used)
	ipb := pickFreeIP(t, used)
	editTunAllocs(t, env, map[string]string{"o1": cidrPool(ipa), "o2": cidrPool(ipb)})
	env.server.ReconcileAllocsFromConfigMap(ctx)

	waitDeviceHasIP(t, nm1.tunName, ipa, 10*time.Second)
	waitDeviceHasIP(t, nm2.tunName, ipb, 10*time.Second)
	if nm1.tunName == nm2.tunName {
		t.Fatal("o1 and o2 must be different devices")
	}
}

// 4. Multiple users edited to the SAME IP → exactly one wins, no two devices share it.
func TestRealTUNManualIP_MultiUserSameIP(t *testing.T) {
	requireTUN(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := newCPEnv(t)
	used := map[string]bool{}

	nm1, c1 := startOwner(t, ctx, env, "o1", used)
	defer c1()
	nm2, c2 := startOwner(t, ctx, env, "o2", used)
	defer c2()
	ip1, ip2 := nm1.localTunIPv4.IP, nm2.localTunIPv4.IP

	ipx := pickFreeIP(t, used)
	editTunAllocs(t, env, map[string]string{"o1": cidrPool(ipx), "o2": cidrPool(ipx)})
	env.server.ReconcileAllocsFromConfigMap(ctx)

	// Poll until stable: exactly one owner holds IPx, the other keeps its original.
	deadline := time.Now().Add(10 * time.Second)
	for {
		o1HasX := deviceHasIP(nm1.tunName, ipx)
		o2HasX := deviceHasIP(nm2.tunName, ipx)
		if o1HasX != o2HasX { // exactly one
			if o1HasX && !deviceHasIP(nm2.tunName, ip2) {
				t.Fatalf("o2 should keep its original IP %s", ip2)
			}
			if o2HasX && !deviceHasIP(nm1.tunName, ip1) {
				t.Fatalf("o1 should keep its original IP %s", ip1)
			}
			return // success: one winner, loser unchanged
		}
		if o1HasX && o2HasX {
			t.Fatalf("both devices hold %s — duplicate IP!", ipx)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no owner adopted %s within timeout", ipx)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// 5. Transient disconnect: control-plane drops then returns on the same port; the
//    watcher reconnects and a subsequent edit still takes effect.
func TestRealTUNManualIP_Disconnect(t *testing.T) {
	requireTUN(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := newCPEnv(t)
	used := map[string]bool{}

	nm, closer := startOwner(t, ctx, env, "o1", used)
	defer closer()
	waitDeviceHasIP(t, nm.tunName, nm.localTunIPv4.IP, 2*time.Second)

	env.restart(t, false) // server down then up, same port, same state
	waitWatcher(t, env, "o1", ipWatcherRetryInterval+10*time.Second)

	ip2 := pickFreeIP(t, used)
	editTunAllocs(t, env, map[string]string{"o1": cidrPool(ip2)})
	env.server.ReconcileAllocsFromConfigMap(ctx)
	waitDeviceHasIP(t, nm.tunName, ip2, 15*time.Second)
}

// 6. Control-plane restart: committed IP (persisted in TUN_ALLOCS) survives a fresh
//    server; a new edit after restart still works.
func TestRealTUNManualIP_ControlPlaneRestart(t *testing.T) {
	requireTUN(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := newCPEnv(t)
	used := map[string]bool{}

	nm, closer := startOwner(t, ctx, env, "o1", used)
	defer closer()

	ip2 := pickFreeIP(t, used)
	editTunAllocs(t, env, map[string]string{"o1": cidrPool(ip2)})
	env.server.ReconcileAllocsFromConfigMap(ctx)
	waitDeviceHasIP(t, nm.tunName, ip2, 10*time.Second) // committed

	env.restart(t, true) // fresh TunConfigServer, reloads committed state from TUN_ALLOCS
	waitWatcher(t, env, "o1", ipWatcherRetryInterval+10*time.Second)
	if !deviceHasIP(nm.tunName, ip2) {
		t.Fatalf("device lost committed IP %s across control-plane restart", ip2)
	}

	ip3 := pickFreeIP(t, used)
	editTunAllocs(t, env, map[string]string{"o1": cidrPool(ip3)})
	env.server.ReconcileAllocsFromConfigMap(ctx)
	waitDeviceHasIP(t, nm.tunName, ip3, 15*time.Second)
}

// 7. Client (machine) restart: a confirmed manual IP persists; the restarted client
//    (same OwnerID) reclaims it.
func TestRealTUNManualIP_ClientRestart(t *testing.T) {
	requireTUN(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := newCPEnv(t)
	used := map[string]bool{}

	nm, closer := startOwner(t, ctx, env, "o1", used)
	ip2 := pickFreeIP(t, used)
	editTunAllocs(t, env, map[string]string{"o1": cidrPool(ip2)})
	env.server.ReconcileAllocsFromConfigMap(ctx)
	waitDeviceHasIP(t, nm.tunName, ip2, 10*time.Second)

	// Simulate client restart (machine reboot): tear down, and wait for the OS to
	// actually remove the device's address before rebuilding — else the rebuilt
	// client would see its own committed IP as a local conflict and get bumped.
	closer()
	waitIPGone(t, ip2, 5*time.Second)

	nm2, closer2 := startOwner(t, ctx, env, "o1", used)
	defer closer2()
	if !nm2.localTunIPv4.IP.Equal(ip2) {
		t.Fatalf("client restart reclaimed %s, want committed %s", nm2.localTunIPv4.IP, ip2)
	}
	waitDeviceHasIP(t, nm2.tunName, ip2, 3*time.Second)
}

// pickFreeIP6 returns an in-pool 2001:2::/64 address not currently local / used.
func pickFreeIP6(t *testing.T, used map[string]bool) net.IP {
	t.Helper()
	local := map[string]bool{}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			local[n.IP.String()] = true
		}
	}
	for a := 0x1000; a < 0xffff; a++ {
		ip := net.ParseIP(fmt.Sprintf("2001:2::%x", a))
		s := ip.String()
		if !local[s] && !used[s] {
			used[s] = true
			return ip
		}
	}
	t.Fatal("no free IPv6 in pool")
	return nil
}

// editTunAllocsDual writes owner→{ipv4cidr, ipv6cidr} (either "" to leave unchanged).
func editTunAllocsDual(t *testing.T, env *cpEnv, m map[string][2]string) {
	t.Helper()
	var b strings.Builder
	for owner, pair := range m {
		fmt.Fprintf(&b, "%s:\n", owner)
		if pair[0] != "" {
			fmt.Fprintf(&b, "  ipv4: %s\n", pair[0])
		}
		if pair[1] != "" {
			fmt.Fprintf(&b, "  ipv6: %s\n", pair[1])
		}
		fmt.Fprintf(&b, "  version: 1\n  lastRenew: %d\n", time.Now().Unix())
	}
	ctx := context.Background()
	cm, err := env.clientset.CoreV1().ConfigMaps(env.ns).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[config.KeyTunAllocs] = b.String()
	if _, err := env.clientset.CoreV1().ConfigMaps(env.ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update cm: %v", err)
	}
}

// 8. Edit only ipv6 → device's v6 changes, v4 untouched.
func TestRealTUNManualIP_V6Change(t *testing.T) {
	requireTUN(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := newCPEnv(t)
	used := map[string]bool{}

	nm, closer := startOwner(t, ctx, env, "o1", used)
	defer closer()
	v4 := nm.localTunIPv4.IP
	v6old := nm.localTunIPv6.IP
	waitDeviceHasIP(t, nm.tunName, v6old, 2*time.Second)

	v6new := pickFreeIP6(t, used)
	editTunAllocsDual(t, env, map[string][2]string{"o1": {cidrPool(v4), v6new.String() + "/64"}}) // keep v4, change v6
	env.server.ReconcileAllocsFromConfigMap(ctx)

	waitDeviceHasIP(t, nm.tunName, v6new, 10*time.Second)
	if !deviceHasIP(nm.tunName, v4) {
		t.Fatalf("v4 %s should be unchanged on device", v4)
	}
	if !nm.localTunIPv6.IP.Equal(v6new) {
		t.Fatalf("nm.localTunIPv6=%s want %s", nm.localTunIPv6.IP, v6new)
	}
	waitDeviceLacksIP(t, nm.tunName, v6old, 5*time.Second) // old v6 must be removed
}

// 9. Edit both ipv4 and ipv6 → both change on the device.
func TestRealTUNManualIP_BothChange(t *testing.T) {
	requireTUN(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := newCPEnv(t)
	used := map[string]bool{}

	nm, closer := startOwner(t, ctx, env, "o1", used)
	defer closer()
	v4old := nm.localTunIPv4.IP
	v6old := nm.localTunIPv6.IP

	v4new := pickFreeIP(t, used)
	v6new := pickFreeIP6(t, used)
	editTunAllocsDual(t, env, map[string][2]string{"o1": {cidrPool(v4new), v6new.String() + "/64"}})
	env.server.ReconcileAllocsFromConfigMap(ctx)

	waitDeviceHasIP(t, nm.tunName, v4new, 10*time.Second)
	waitDeviceHasIP(t, nm.tunName, v6new, 10*time.Second)
	waitDeviceLacksIP(t, nm.tunName, v4old, 5*time.Second) // old v4 must be removed (the macOS bug)
	waitDeviceLacksIP(t, nm.tunName, v6old, 5*time.Second) // old v6 must be removed
}
