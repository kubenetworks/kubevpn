package xds

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	runtimeservice "github.com/envoyproxy/go-control-plane/envoy/service/runtime/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// defaultEnvoyImage matches go.mod's go-control-plane/envoy version so the running Envoy
// speaks the same xDS protobuf schema the control plane generates. Override with the
// KUBEVPN_E2E_ENVOY_IMAGE env var to run against a locally-available image (e.g. the
// kubevpn sidecar image, which bundles envoy) in offline environments.
const defaultEnvoyImage = "envoyproxy/envoy:v1.35.0"

// envoyAdminPort is the admin port Envoy binds inside its container (published to the host).
const envoyAdminPort = 19901

func envoyTestImage() string {
	if img := os.Getenv("KUBEVPN_E2E_ENVOY_IMAGE"); img != "" {
		return img
	}
	return defaultEnvoyImage
}

// TestIntegration_UDP_EnvoyEndToEnd is a full end-to-end story test of the control
// plane's UDP support:
//
//	control plane (UDP Virtual → xDS snapshot) → real Envoy (udp_proxy filter) →
//	Go UDP client → Envoy → upstream UDP echo server → back to client
//
// It boots a real Envoy in Docker, feeds it a UDP Virtual over a real xDS gRPC server, then
// sends a UDP datagram from Go through Envoy and asserts the echoed reply. This is the only
// test that exercises the generated UDP config against a live Envoy — config-generation
// tests cannot tell whether Envoy actually accepts and serves it (e.g. the shared toCluster
// helper attaches HTTP options even to udp_proxy clusters).
//
// The test skips (never fails) when Docker is unavailable, the image cannot be pulled, or
// the docker host is unreachable from the test container, so default `go test ./pkg/...`
// stays green in CI.
//
// Offline/restricted networks: override the image with a reachable mirror, e.g.
//
//	KUBEVPN_E2E_ENVOY_IMAGE=docker.m.daocloud.io/envoyproxy/envoy:v1.35.0 \
//	    go test ./pkg/xds/ -run TestIntegration_UDP_EnvoyEndToEnd -v
//
// Use a NATIVE-arch Envoy image: an emulated (qemu) Envoy can silently fail to receive
// UDP datagrams even though TCP works.
func TestIntegration_UDP_EnvoyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping real-Envoy UDP e2e test")
	}
	ensureEnvoyImage(t)
	logger := log.NewEntry(log.New())
	hostIP := outboundIP(t)
	t.Logf("test container IP (reached by Envoy outbound): %s", hostIP)

	// --- Phase 1: upstream UDP echo server (the "local app" Envoy forwards to) ------------
	echoPort := startUDPEcho(t, hostIP)
	t.Logf("upstream UDP echo server on %s:%d", hostIP, echoPort)

	// --- Phase 2: control plane (production Virtual.To() → snapshot) ----------------------
	listenPort := freeTCPPort(t) // Envoy's inbound UDP listener port == Virtual container port
	const namespace = "e2e-ns"
	const uid = "deployments.apps.udpecho"
	nodeID := util.GenEnvoyUID(namespace, uid) // == snapshot key == Envoy --service-node

	v := &Virtual{
		SchemaVersion: CurrentSchemaVersion,
		Namespace:     namespace,
		UID:           uid,
		Ports: []ContainerPort{
			{ContainerPort: int32(listenPort), Protocol: corev1.ProtocolUDP},
		},
		Rules: []*Rule{
			{
				// Upstream endpoint Envoy forwards UDP datagrams to: this container's IP +
				// the echo server port (encoded as the EnvoyPort in PortMap).
				LocalTunIPv4: hostIP,
				OwnerID:      "owner-udp",
				PortMap:      map[int32]string{int32(listenPort): strconv.Itoa(echoPort)},
			},
		},
	}
	snapshotCache := newXDSSnapshotCache(t, logger, nodeID, v)

	// --- Phase 3: xDS gRPC server + Phase 4: real Envoy ----------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	xdsPort := startXDSServer(t, ctx, snapshotCache)
	t.Logf("xDS gRPC server on %s:%d (node %q)", hostIP, xdsPort, nodeID)

	bootstrap := envoyBootstrap(hostIP, xdsPort, envoyAdminPort)
	udpSpec := fmt.Sprintf("%d/udp", listenPort)
	dockerHost, adminBase, hostPorts, logs, name := startTestEnvoy(t, ctx, nodeID, bootstrap, udpSpec)

	// --- Phase 5: assert the UDP round-trip ----------------------------------------------
	const payload = "hello"
	const want = "HELLO"
	got := udpProbe(t, ctx, name, net.JoinHostPort(dockerHost, hostPorts[udpSpec]), listenPort, payload, 10)
	if got != want {
		t.Logf("envoy /listeners:\n%s", httpGet(adminBase+"/listeners?format=json"))
		t.Logf("envoy /clusters:\n%s", httpGet(adminBase+"/clusters?format=json"))
		t.Logf("envoy udp stats:\n%s", httpGet(adminBase+"/stats?filter=udp"))
		t.Logf("envoy logs:\n%s", logs.String())
		t.Fatalf("UDP round-trip through Envoy failed: sent %q, got %q (want %q)", payload, got, want)
	}
	t.Logf("✓ UDP round-trip through real Envoy succeeded: %q → %q", payload, got)
}

// TestIntegration_TCP_EnvoyEndToEnd is a full end-to-end story test of the control plane's
// TCP support — specifically the HTTP connection manager with header-based routing, which
// is the core TCP traffic-splitting feature:
//
//	control plane (TCP Virtual → xDS snapshot) → real Envoy (HTTP CM + RDS header route)
//	→ Go HTTP client (with matching header) → Envoy → upstream HTTP server → response
//
// The Virtual uses Fargate mode: the mesh-mode TCP listener sets BindToPort=false (it
// relies on iptables original_dst redirection, which a plain container can't provide), so
// only the Fargate listener actually binds a port and can receive traffic without iptables.
// The TCP routing code path (toListener → buildFilterChains → toRoute header match → EDS
// cluster/endpoint) is identical between mesh and fargate; fargate just makes it testable.
//
// Skip/offline behavior matches TestIntegration_UDP_EnvoyEndToEnd.
func TestIntegration_TCP_EnvoyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping real-Envoy TCP e2e test")
	}
	ensureEnvoyImage(t)
	logger := log.NewEntry(log.New())
	hostIP := outboundIP(t)
	t.Logf("test container IP (reached by Envoy outbound): %s", hostIP)

	// --- Phase 1: upstream HTTP server (the "local app" Envoy routes to) ------------------
	upstreamPort := startHTTPUpstream(t, "ok")
	t.Logf("upstream HTTP server on %s:%d", hostIP, upstreamPort)

	// --- Phase 2: control plane (production Virtual.To() → snapshot) ----------------------
	envoyBindPort := freeTCPPort(t) // Fargate EnvoyListenerPort: the port Envoy binds & we publish
	const containerPort = 8080
	const namespace = "e2e-ns"
	const uid = "deployments.apps.tcpweb"
	const routeHeader = "x-kubevpn-route"
	const routeValue = "e2e"
	nodeID := util.GenEnvoyUID(namespace, uid)

	v := &Virtual{
		SchemaVersion: CurrentSchemaVersion,
		Namespace:     namespace,
		UID:           uid,
		FargateMode:   true,
		Ports: []ContainerPort{
			{Name: "http", ContainerPort: containerPort, EnvoyListenerPort: int32(envoyBindPort), Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{routeHeader: routeValue},
				LocalTunIPv4: hostIP,
				OwnerID:      "owner-tcp",
				// Fargate PortMap form "envoyPort:localPort"; EnvoyPort is the upstream endpoint port.
				PortMap: map[int32]string{containerPort: fmt.Sprintf("%d:%d", upstreamPort, upstreamPort)},
			},
		},
	}
	snapshotCache := newXDSSnapshotCache(t, logger, nodeID, v)

	// --- Phase 3: xDS gRPC server + Phase 4: real Envoy ----------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	xdsPort := startXDSServer(t, ctx, snapshotCache)
	t.Logf("xDS gRPC server on %s:%d (node %q)", hostIP, xdsPort, nodeID)

	bootstrap := envoyBootstrap(hostIP, xdsPort, envoyAdminPort)
	tcpSpec := fmt.Sprintf("%d/tcp", envoyBindPort)
	dockerHost, adminBase, hostPorts, logs, _ := startTestEnvoy(t, ctx, nodeID, bootstrap, tcpSpec)

	// --- Phase 5: assert the HTTP request is routed through Envoy to the upstream ---------
	url := fmt.Sprintf("http://%s/", net.JoinHostPort(dockerHost, hostPorts[tcpSpec]))
	want := "ok:" + routeValue // upstream echoes the routing header back
	status, got := pollRouteGet(url, routeValue, want, 15)
	if got != want {
		t.Logf("envoy /listeners:\n%s", httpGet(adminBase+"/listeners?format=json"))
		t.Logf("envoy /clusters:\n%s", httpGet(adminBase+"/clusters?format=json"))
		t.Logf("envoy http stats:\n%s", httpGet(adminBase+"/stats?filter=http"))
		t.Logf("envoy logs:\n%s", logs.String())
		t.Fatalf("HTTP request through Envoy failed: status=%d body=%q (want 200 %q)", status, got, want)
	}
	t.Logf("✓ TCP/HTTP header-routed request through real Envoy succeeded: %q (status %d)", got, status)
}

// TestIntegration_MeshInboundCapture_EnvoyEndToEnd verifies the core property of the
// mesh-mode inbound fix against a real Envoy: the :15006 virtual-inbound capture listener
// is accepted and actually BOUND. In mesh mode the per-port listeners are BindToPort=false,
// so :15006 is the only bound TCP entry point — the sidecar iptables DNATs inbound TCP to
// it. Before the fix nothing bound :15006, so DNAT'd connections hit a closed port and were
// reset (the in-cluster "connection reset by peer"). A successful TCP dial to the published
// :15006 proves Envoy is listening. (The original_dst redirect itself needs real iptables,
// which is why full mesh routing is only verifiable in-cluster.)
func TestIntegration_MeshInboundCapture_EnvoyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping real-Envoy mesh capture e2e test")
	}
	ensureEnvoyImage(t)
	logger := log.NewEntry(log.New())
	hostIP := outboundIP(t)

	const containerPort = 9080
	const namespace = "e2e-ns"
	const uid = "deployments.apps.meshcap"
	nodeID := util.GenEnvoyUID(namespace, uid)

	// Mesh mode (FargateMode=false): per-port listener BindToPort=false; only the
	// :15006 capture listener binds.
	v := &Virtual{
		SchemaVersion: CurrentSchemaVersion,
		Namespace:     namespace,
		UID:           uid,
		Ports: []ContainerPort{
			{Name: "http", ContainerPort: containerPort, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{"x-kubevpn-route": "e2e"},
				LocalTunIPv4: hostIP,
				OwnerID:      "owner-mesh",
				PortMap:      map[int32]string{containerPort: fmt.Sprintf("%d", containerPort)},
			},
		},
	}
	snapshotCache := newXDSSnapshotCache(t, logger, nodeID, v)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	xdsPort := startXDSServer(t, ctx, snapshotCache)
	bootstrap := envoyBootstrap(hostIP, xdsPort, envoyAdminPort)
	capSpec := fmt.Sprintf("%d/tcp", config.PortEnvoyInbound)
	dockerHost, adminBase, hostPorts, logs, _ := startTestEnvoy(t, ctx, nodeID, bootstrap, capSpec)

	// Core assertion: Envoy must bind :15006 and accept connections.
	addr := net.JoinHostPort(dockerHost, hostPorts[capSpec])
	var dialErr error
	for i := 0; i < 30; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			dialErr = nil
			break
		}
		dialErr = err
		time.Sleep(500 * time.Millisecond)
	}
	if dialErr != nil {
		t.Logf("envoy /listeners:\n%s", httpGet(adminBase+"/listeners?format=json"))
		t.Logf("envoy logs:\n%s", logs.String())
		t.Fatalf("Envoy did not bind the :15006 inbound capture listener: %v", dialErr)
	}
	if l := httpGet(adminBase + "/listeners?format=json"); !strings.Contains(l, strconv.Itoa(config.PortEnvoyInbound)) {
		t.Fatalf("admin /listeners does not show the :15006 capture listener:\n%s", l)
	}
	t.Logf("✓ mesh-mode :15006 inbound capture listener bound and accepting on real Envoy")
}

// fargateHTTPPort builds a fargate ContainerPort for an HTTP workload: container port is the
// default-upstream port (the fargate default route targets hostIP:containerPort), envoy binds
// bindPort (published to the host).
func fargateHTTPPort(containerPort, bindPort int) ContainerPort {
	return ContainerPort{
		Name:              "http",
		ContainerPort:     int32(containerPort),
		EnvoyListenerPort: int32(bindPort),
		Protocol:          corev1.ProtocolTCP,
	}
}

// headerRule builds a rule routing requests with header x-kubevpn-route:value to the upstream
// at hostIP:upstreamPort (encoded as the fargate "envoyPort:localPort" PortMap form).
func headerRule(containerPort int, hostIP, ownerID, value string, upstreamPort int) *Rule {
	return &Rule{
		Headers:      map[string]string{"x-kubevpn-route": value},
		LocalTunIPv4: hostIP,
		OwnerID:      ownerID,
		PortMap:      map[int32]string{int32(containerPort): fmt.Sprintf("%d:%d", upstreamPort, upstreamPort)},
	}
}

// TestIntegration_TCP_HeaderSplit_EnvoyEndToEnd verifies multi-rule header-based traffic
// splitting through a real Envoy: header a → upstream A, header b → upstream B, no header →
// default route (upstream on the container port). Exercises toRoute header matchers + the
// fargate default-route-last behavior in Virtual.To().
func TestIntegration_TCP_HeaderSplit_EnvoyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping real-Envoy header-split e2e test")
	}
	ensureEnvoyImage(t)
	logger := log.NewEntry(log.New())
	hostIP := outboundIP(t)

	// Three upstreams: A, B (header-matched rules) and C (default route → container port).
	upA := startHTTPUpstream(t, "A")
	upB := startHTTPUpstream(t, "B")
	cPort := startHTTPUpstream(t, "C") // default route targets hostIP:cPort
	bind := freeTCPPort(t)
	nodeID := util.GenEnvoyUID("e2e-ns", "deployments.apps.split")

	v := &Virtual{
		SchemaVersion: CurrentSchemaVersion,
		Namespace:     "e2e-ns",
		UID:           "deployments.apps.split",
		FargateMode:   true,
		Ports:         []ContainerPort{fargateHTTPPort(cPort, bind)},
		Rules: []*Rule{
			headerRule(cPort, hostIP, "owner-a", "a", upA),
			headerRule(cPort, hostIP, "owner-b", "b", upB),
		},
	}
	snapshotCache := newXDSSnapshotCache(t, logger, nodeID, v)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	xdsPort := startXDSServer(t, ctx, snapshotCache)
	bootstrap := envoyBootstrap(hostIP, xdsPort, envoyAdminPort)
	tcpSpec := fmt.Sprintf("%d/tcp", bind)
	dockerHost, adminBase, hostPorts, logs, _ := startTestEnvoy(t, ctx, nodeID, bootstrap, tcpSpec)
	url := fmt.Sprintf("http://%s/", net.JoinHostPort(dockerHost, hostPorts[tcpSpec]))

	// Each upstream echoes the routing header, so the body identifies which one answered.
	for _, tc := range []struct{ name, header, want string }{
		{"header a → upstream A", "a", "A:a"},
		{"header b → upstream B", "b", "B:b"},
		{"no header → default upstream C", "", "C:"},
	} {
		status, got := pollRouteGet(url, tc.header, tc.want, 20)
		if got != tc.want {
			t.Logf("envoy /listeners:\n%s", httpGet(adminBase+"/listeners?format=json"))
			t.Logf("envoy logs:\n%s", logs.String())
			t.Fatalf("%s: status=%d body=%q (want %q)", tc.name, status, got, tc.want)
		}
		t.Logf("✓ %s → %q", tc.name, got)
	}
}

// TestIntegration_TCP_EndpointHotUpdate_EnvoyEndToEnd verifies the TUN-IP hot-update path
// (docs/09): a request routes to upstream A, then the control plane pushes a NEW snapshot
// whose rule endpoint points to upstream B, and the SAME request reroutes to B over live xDS
// — no Envoy restart.
func TestIntegration_TCP_EndpointHotUpdate_EnvoyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping real-Envoy hot-update e2e test")
	}
	ensureEnvoyImage(t)
	logger := log.NewEntry(log.New())
	hostIP := outboundIP(t)

	upA := startHTTPUpstream(t, "A")
	upB := startHTTPUpstream(t, "B")
	cPort := startHTTPUpstream(t, "C")
	bind := freeTCPPort(t)
	nodeID := util.GenEnvoyUID("e2e-ns", "deployments.apps.hotupdate")
	mkVirtual := func(upstream int) *Virtual {
		return &Virtual{
			SchemaVersion: CurrentSchemaVersion,
			Namespace:     "e2e-ns",
			UID:           "deployments.apps.hotupdate",
			FargateMode:   true,
			Ports:         []ContainerPort{fargateHTTPPort(cPort, bind)},
			Rules:         []*Rule{headerRule(cPort, hostIP, "owner-hot", "go", upstream)},
		}
	}

	snapshotCache := newXDSSnapshotCache(t, logger, nodeID, mkVirtual(upA))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	xdsPort := startXDSServer(t, ctx, snapshotCache)
	bootstrap := envoyBootstrap(hostIP, xdsPort, envoyAdminPort)
	tcpSpec := fmt.Sprintf("%d/tcp", bind)
	dockerHost, adminBase, hostPorts, logs, _ := startTestEnvoy(t, ctx, nodeID, bootstrap, tcpSpec)
	url := fmt.Sprintf("http://%s/", net.JoinHostPort(dockerHost, hostPorts[tcpSpec]))

	// Before update: routes to upstream A.
	if status, got := pollRouteGet(url, "go", "A:go", 20); got != "A:go" {
		t.Logf("envoy logs:\n%s", logs.String())
		t.Fatalf("pre-update: status=%d body=%q (want %q)", status, got, "A:go")
	}
	t.Logf("✓ before hot-update: routed to upstream A")

	// Push a new snapshot with the endpoint changed to upstream B (simulates TUN IP change).
	pushVirtual(t, logger, snapshotCache, nodeID, "2", mkVirtual(upB))

	// After update: same request reroutes to upstream B (allow time for xDS propagation).
	if status, got := pollRouteGet(url, "go", "B:go", 40); got != "B:go" {
		t.Logf("envoy /clusters:\n%s", httpGet(adminBase+"/clusters?format=json"))
		t.Logf("envoy logs:\n%s", logs.String())
		t.Fatalf("post-update: status=%d body=%q (want %q) — hot-update did not propagate", status, got, "B:go")
	}
	t.Logf("✓ after hot-update: rerouted to upstream B with no Envoy restart")
}

// TestIntegration_TCP_RuleTeardown_EnvoyEndToEnd verifies selective rule removal (one user
// "leaves"): two header rules route to A and B; after the control plane pushes a snapshot with
// rule B removed, header b falls back to the default route (upstream C) while header a is
// untouched.
func TestIntegration_TCP_RuleTeardown_EnvoyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping real-Envoy teardown e2e test")
	}
	ensureEnvoyImage(t)
	logger := log.NewEntry(log.New())
	hostIP := outboundIP(t)

	upA := startHTTPUpstream(t, "A")
	upB := startHTTPUpstream(t, "B")
	cPort := startHTTPUpstream(t, "C") // default route
	bind := freeTCPPort(t)
	nodeID := util.GenEnvoyUID("e2e-ns", "deployments.apps.teardown")
	base := func(rules ...*Rule) *Virtual {
		return &Virtual{
			SchemaVersion: CurrentSchemaVersion,
			Namespace:     "e2e-ns",
			UID:           "deployments.apps.teardown",
			FargateMode:   true,
			Ports:         []ContainerPort{fargateHTTPPort(cPort, bind)},
			Rules:         rules,
		}
	}
	ruleA := headerRule(cPort, hostIP, "owner-a", "a", upA)
	ruleB := headerRule(cPort, hostIP, "owner-b", "b", upB)

	snapshotCache := newXDSSnapshotCache(t, logger, nodeID, base(ruleA, ruleB))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	xdsPort := startXDSServer(t, ctx, snapshotCache)
	bootstrap := envoyBootstrap(hostIP, xdsPort, envoyAdminPort)
	tcpSpec := fmt.Sprintf("%d/tcp", bind)
	dockerHost, adminBase, hostPorts, logs, _ := startTestEnvoy(t, ctx, nodeID, bootstrap, tcpSpec)
	url := fmt.Sprintf("http://%s/", net.JoinHostPort(dockerHost, hostPorts[tcpSpec]))

	// Both rules active.
	if _, got := pollRouteGet(url, "a", "A:a", 20); got != "A:a" {
		t.Logf("envoy logs:\n%s", logs.String())
		t.Fatalf("pre-teardown header a: got %q (want %q)", got, "A:a")
	}
	if _, got := pollRouteGet(url, "b", "B:b", 20); got != "B:b" {
		t.Fatalf("pre-teardown header b: got %q (want %q)", got, "B:b")
	}
	t.Logf("✓ before teardown: a→A, b→B")

	// Remove rule B; header b should now fall back to the default route (upstream C).
	pushVirtual(t, logger, snapshotCache, nodeID, "2", base(ruleA))

	if status, got := pollRouteGet(url, "b", "C:b", 40); got != "C:b" {
		t.Logf("envoy /listeners:\n%s", httpGet(adminBase+"/listeners?format=json"))
		t.Logf("envoy logs:\n%s", logs.String())
		t.Fatalf("post-teardown header b: status=%d body=%q (want %q — fall back to default)", status, got, "C:b")
	}
	if _, got := pollRouteGet(url, "a", "A:a", 20); got != "A:a" {
		t.Fatalf("post-teardown header a changed: got %q (want %q — rule A must be untouched)", got, "A:a")
	}
	t.Logf("✓ after removing rule B: b→C (default), a→A unchanged")
}

// TestIntegration_MixedTCPUDP_EnvoyEndToEnd verifies a single workload exposing both a TCP and
// a UDP port: the TCP listener serves an HTTP request and the UDP listener echoes a datagram,
// both driven from one Virtual (Virtual.To() multi-port handling).
func TestIntegration_MixedTCPUDP_EnvoyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping real-Envoy mixed TCP/UDP e2e test")
	}
	ensureEnvoyImage(t)
	logger := log.NewEntry(log.New())
	hostIP := outboundIP(t)

	upHTTP := startHTTPUpstream(t, "M")
	upEcho := startUDPEcho(t, hostIP)
	tcpCPort := freeTCPPort(t) // TCP container port
	udpCPort := freeTCPPort(t) // UDP container port (just a distinct number)
	bindTCP := freeTCPPort(t)
	bindUDP := freeTCPPort(t)
	nodeID := util.GenEnvoyUID("e2e-ns", "deployments.apps.mixed")

	v := &Virtual{
		SchemaVersion: CurrentSchemaVersion,
		Namespace:     "e2e-ns",
		UID:           "deployments.apps.mixed",
		FargateMode:   true,
		Ports: []ContainerPort{
			{Name: "http", ContainerPort: int32(tcpCPort), EnvoyListenerPort: int32(bindTCP), Protocol: corev1.ProtocolTCP},
			{Name: "udp", ContainerPort: int32(udpCPort), EnvoyListenerPort: int32(bindUDP), Protocol: corev1.ProtocolUDP},
		},
		Rules: []*Rule{
			{
				// Empty headers → the TCP HTTP route matches any request; UDP ignores headers.
				LocalTunIPv4: hostIP,
				OwnerID:      "owner-mixed",
				PortMap: map[int32]string{
					int32(tcpCPort): fmt.Sprintf("%d:%d", upHTTP, upHTTP),
					int32(udpCPort): fmt.Sprintf("%d:%d", upEcho, upEcho),
				},
			},
		},
	}
	snapshotCache := newXDSSnapshotCache(t, logger, nodeID, v)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	xdsPort := startXDSServer(t, ctx, snapshotCache)
	bootstrap := envoyBootstrap(hostIP, xdsPort, envoyAdminPort)
	tcpSpec := fmt.Sprintf("%d/tcp", bindTCP)
	udpSpec := fmt.Sprintf("%d/udp", bindUDP)
	dockerHost, adminBase, hostPorts, logs, name := startTestEnvoy(t, ctx, nodeID, bootstrap, tcpSpec, udpSpec)

	// TCP/HTTP through Envoy.
	url := fmt.Sprintf("http://%s/", net.JoinHostPort(dockerHost, hostPorts[tcpSpec]))
	if status, got := pollRouteGet(url, "", "M:", 20); got != "M:" {
		t.Logf("envoy /listeners:\n%s", httpGet(adminBase+"/listeners?format=json"))
		t.Logf("envoy logs:\n%s", logs.String())
		t.Fatalf("mixed TCP: status=%d body=%q (want %q)", status, got, "M:")
	}
	t.Logf("✓ mixed workload TCP/HTTP succeeded")

	// UDP through Envoy.
	if got := udpProbe(t, ctx, name, net.JoinHostPort(dockerHost, hostPorts[udpSpec]), bindUDP, "ping", 10); got != "PING" {
		t.Logf("envoy udp stats:\n%s", httpGet(adminBase+"/stats?filter=udp"))
		t.Logf("envoy logs:\n%s", logs.String())
		t.Fatalf("mixed UDP: got %q (want %q)", got, "PING")
	}
	t.Logf("✓ mixed workload UDP succeeded — both listeners serve from one Virtual")
}

// TestIntegration_BootstrapFiles_EnvoyEndToEnd drives a real Envoy with each of the four
// PRODUCTION bootstrap files embedded by the sidecar injector (pkg/inject/*.yaml), rendered
// exactly as inject.renderEnvoyConfig does ({{.}} → xDS host). It proves each bootstrap boots
// Envoy, connects to our xDS server (which the files hardcode at :9002), and serves
// xDS-driven traffic. Mesh bootstraps are exercised with UDP (the listener that binds without
// iptables); fargate bootstraps with TCP/HTTP header routing.
func TestIntegration_BootstrapFiles_EnvoyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found in PATH; skipping real-Envoy bootstrap-file e2e test")
	}
	ensureEnvoyImage(t)

	const adminPort = 9003 // hardcoded in the bootstrap yamls

	cases := []struct {
		file  string
		proto string
	}{
		{"envoy.yaml", "udp"},              // mesh, IPv6 admin/xds (ipv4_compat)
		{"envoy_ipv4.yaml", "udp"},         // mesh, IPv4
		{"fargate_envoy.yaml", "tcp"},      // fargate, IPv6
		{"fargate_envoy_ipv4.yaml", "tcp"}, // fargate, IPv4
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			logger := log.NewEntry(log.New())
			hostIP := outboundIP(t)
			bootstrap := renderBootstrap(t, c.file, hostIP)
			nodeID := util.GenEnvoyUID("e2e-ns", "deployments.apps.bootstrap")

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			switch c.proto {
			case "udp":
				echoPort := startUDPEcho(t, hostIP)
				listenPort := freeTCPPort(t)
				v := &Virtual{
					SchemaVersion: CurrentSchemaVersion,
					Namespace:     "e2e-ns",
					UID:           "deployments.apps.bootstrap",
					Ports:         []ContainerPort{{ContainerPort: int32(listenPort), Protocol: corev1.ProtocolUDP}},
					Rules:         []*Rule{{LocalTunIPv4: hostIP, OwnerID: "owner-bs", PortMap: map[int32]string{int32(listenPort): strconv.Itoa(echoPort)}}},
				}
				sc := newXDSSnapshotCache(t, logger, nodeID, v)
				startXDSServerOn(t, ctx, sc, config.PortXDS) // :9002, as hardcoded in the bootstrap
				udpSpec := fmt.Sprintf("%d/udp", listenPort)
				dockerHost, adminBase, hostPorts, logs, name := startTestEnvoyAdmin(t, ctx, nodeID, bootstrap, adminPort, udpSpec)
				if got := udpProbe(t, ctx, name, net.JoinHostPort(dockerHost, hostPorts[udpSpec]), listenPort, "hello", 10); got != "HELLO" {
					t.Logf("envoy udp stats:\n%s", httpGet(adminBase+"/stats?filter=udp"))
					t.Logf("envoy logs:\n%s", logs.String())
					t.Fatalf("%s: UDP round-trip got %q (want %q)", c.file, got, "HELLO")
				}
				t.Logf("✓ %s: UDP round-trip through real Envoy succeeded", c.file)
			case "tcp":
				upstream := startHTTPUpstream(t, "B")
				cPort := startHTTPUpstream(t, "C") // fargate default route target
				bind := freeTCPPort(t)
				v := &Virtual{
					SchemaVersion: CurrentSchemaVersion,
					Namespace:     "e2e-ns",
					UID:           "deployments.apps.bootstrap",
					FargateMode:   true,
					Ports:         []ContainerPort{fargateHTTPPort(cPort, bind)},
					Rules:         []*Rule{headerRule(cPort, hostIP, "owner-bs", "go", upstream)},
				}
				sc := newXDSSnapshotCache(t, logger, nodeID, v)
				startXDSServerOn(t, ctx, sc, config.PortXDS) // :9002
				tcpSpec := fmt.Sprintf("%d/tcp", bind)
				dockerHost, adminBase, hostPorts, logs, _ := startTestEnvoyAdmin(t, ctx, nodeID, bootstrap, adminPort, tcpSpec)
				url := fmt.Sprintf("http://%s/", net.JoinHostPort(dockerHost, hostPorts[tcpSpec]))
				if status, got := pollRouteGet(url, "go", "B:go", 20); got != "B:go" {
					t.Logf("envoy /listeners:\n%s", httpGet(adminBase+"/listeners?format=json"))
					t.Logf("envoy logs:\n%s", logs.String())
					t.Fatalf("%s: status=%d body=%q (want %q)", c.file, status, got, "B:go")
				}
				t.Logf("✓ %s: TCP/HTTP header-routed request through real Envoy succeeded", c.file)
			}
		})
	}
}

// newXDSSnapshotCache returns a SnapshotCache serving v's snapshot (version "1") under nodeID.
func newXDSSnapshotCache(t *testing.T, logger *log.Entry, nodeID string, v *Virtual) cache.SnapshotCache {
	t.Helper()
	sc := cache.NewSnapshotCache(false, cache.IDHash{}, logger)
	pushVirtual(t, logger, sc, nodeID, "1", v)
	return sc
}

// pushVirtual builds an xDS snapshot from the production Virtual.To() generator and serves it
// under nodeID at the given version — re-callable to push live updates to a connected Envoy
// (ADS). IPv4-only (enableIPv6=false) for determinism: To() emits one listener+cluster per IP
// family, so enabling IPv6 with an empty LocalTunIPv6 yields a duplicate-named listener bound
// to an invalid empty-address endpoint. (newProcessor.ProcessFile derives enableIPv6 from
// util.DetectSupportIPv6(), which would make these tests host-dependent.)
func pushVirtual(t *testing.T, logger *log.Entry, sc cache.SnapshotCache, nodeID, version string, v *Virtual) {
	t.Helper()
	listeners, clusters, routes, endpoints := To(v, false, logger)
	snapshot, err := cache.NewSnapshot(version, map[resource.Type][]types.Resource{
		resource.ListenerType: listeners,
		resource.RouteType:    routes,
		resource.ClusterType:  clusters,
		resource.EndpointType: endpoints,
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if err = snapshot.Consistent(); err != nil {
		t.Fatalf("snapshot inconsistent: %v", err)
	}
	if err = sc.SetSnapshot(context.Background(), nodeID, snapshot); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
}

// startXDSServer starts a real xDS gRPC server on an ephemeral port. See startXDSServerOn.
func startXDSServer(t *testing.T, ctx context.Context, snapshotCache cache.SnapshotCache) int {
	return startXDSServerOn(t, ctx, snapshotCache, 0)
}

// startXDSServerOn starts a real xDS gRPC server (same service registrations as the production
// server.go:runServer) backed by snapshotCache, bound to all interfaces on the given port
// (0 = ephemeral). Returns the actual port; the server is stopped via t.Cleanup. A fixed port
// is needed when driving Envoy with the production bootstrap files, which hardcode :9002.
func startXDSServerOn(t *testing.T, ctx context.Context, snapshotCache cache.SnapshotCache, port int) int {
	t.Helper()
	srv := serverv3.NewServer(ctx, snapshotCache, nil)
	grpcServer := grpc.NewServer()
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, srv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, srv)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, srv)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, srv)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, srv)
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, srv)
	runtimeservice.RegisterRuntimeDiscoveryServiceServer(grpcServer, srv)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("listen xds on :%d: %v", port, err)
	}
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)
	return lis.Addr().(*net.TCPAddr).Port
}

// startTestEnvoy runs Envoy in Docker with the given bootstrap, publishing the admin port
// plus each container port in publish (e.g. "8080/tcp", "53/udp") to a random host port.
// Envoy runs on the default bridge and reaches the xDS server + upstream outbound; the test
// reaches Envoy's published ports via a docker-host address (DOCKER_HOST / host.docker.internal
// / default gateway / loopback). It waits until admin /ready is reachable and returns the
// chosen host, the admin base URL, a map of containerPortSpec→hostPort, and the Envoy logs.
// Skips (never fails) on any setup/reachability problem.
func startTestEnvoy(t *testing.T, ctx context.Context, nodeID, bootstrap string, publish ...string) (dockerHost, adminBase string, hostPorts map[string]string, logs *bytes.Buffer, name string) {
	return startTestEnvoyAdmin(t, ctx, nodeID, bootstrap, envoyAdminPort, publish...)
}

// startTestEnvoyAdmin is startTestEnvoy with a configurable admin container port — needed for
// the production bootstrap files, whose admin server is hardcoded to :9003. It returns the
// Envoy container name so UDP tests can attach a client to its network namespace (see udpProbe).
func startTestEnvoyAdmin(t *testing.T, ctx context.Context, nodeID, bootstrap string, adminPort int, publish ...string) (dockerHost, adminBase string, hostPorts map[string]string, logs *bytes.Buffer, name string) {
	t.Helper()
	adminSpec := fmt.Sprintf("%d/tcp", adminPort)
	specs := append([]string{adminSpec}, publish...)
	name = "kubevpn-e2e-" + strings.NewReplacer("/", "-").Replace(publish[0])

	_ = exec.Command("docker", "rm", "-f", name).Run() // best-effort pre-clean
	args := []string{"run", "--rm", "--name", name}
	for _, s := range specs {
		args = append(args, "-p", s) // host port auto-assigned
	}
	args = append(args, "--entrypoint", "envoy", envoyTestImage(),
		"-l", "info",
		"--service-node", nodeID,
		"--service-cluster", nodeID,
		"--config-yaml", bootstrap,
	)
	logs = &bytes.Buffer{}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start envoy container: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		_ = cmd.Wait()
	})

	hostPorts = map[string]string{}
	for _, s := range specs {
		p := waitPublishedPort(t, name, s, 15*time.Second)
		if p == "" {
			t.Logf("envoy logs:\n%s", logs.String())
			t.Skipf("could not resolve Envoy published host port for %s", s)
		}
		hostPorts[s] = p
	}
	t.Logf("envoy published ports: %v", hostPorts)

	candidates := dockerHostCandidates(t)
	t.Logf("docker host candidates: %v", candidates)
	for _, h := range candidates {
		base := "http://" + net.JoinHostPort(h, hostPorts[adminSpec])
		if waitEnvoyReady(base, 15*time.Second) {
			dockerHost, adminBase = h, base
			break
		}
	}
	if dockerHost == "" {
		t.Logf("envoy logs:\n%s", logs.String())
		t.Skipf("envoy admin /ready unreachable via any docker host candidate %v on port %s", candidates, hostPorts[adminSpec])
	}
	t.Logf("envoy ready via docker host %s (%s)", dockerHost, adminBase)
	return dockerHost, adminBase, hostPorts, logs, name
}

// ensureEnvoyImage makes sure the Envoy image is present locally, pulling it if needed.
// Skips the test if the image is absent and cannot be pulled (e.g. offline CI).
func ensureEnvoyImage(t *testing.T) {
	t.Helper()
	img := envoyTestImage()
	if exec.Command("docker", "image", "inspect", img).Run() == nil {
		return
	}
	t.Logf("pulling %s (first run)…", img)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "pull", img).CombinedOutput()
	if err != nil {
		t.Skipf("cannot pull %s (offline?): %v\n%s", img, err, out)
	}
}

// envoyBootstrap builds a minimal ADS bootstrap pointing Envoy at the xDS server running in
// the test container (reached via xdsHost). Mirrors pkg/inject/envoy_ipv4.yaml. The admin
// server binds 0.0.0.0 so its published port is reachable from the test container.
func envoyBootstrap(xdsHost string, xdsPort, adminPort int) string {
	return fmt.Sprintf(`admin:
  address:
    socket_address:
      address: "0.0.0.0"
      port_value: %d
dynamic_resources:
  ads_config:
    api_type: GRPC
    transport_api_version: V3
    grpc_services:
      - envoy_grpc:
          cluster_name: xds_cluster
    set_node_on_first_message_only: true
  cds_config:
    resource_api_version: V3
    ads: {}
  lds_config:
    resource_api_version: V3
    ads: {}
static_resources:
  clusters:
    - type: STRICT_DNS
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      name: xds_cluster
      load_assignment:
        cluster_name: xds_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: %s
                      port_value: %d
`, adminPort, xdsHost, xdsPort)
}

// waitEnvoyReady polls Envoy's admin /ready endpoint until it returns 200 or timeout.
// adminBase is the scheme+host+port prefix, e.g. "http://127.0.0.1:32770".
func waitEnvoyReady(adminBase string, timeout time.Duration) bool {
	url := adminBase + "/ready"
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			code := resp.StatusCode
			resp.Body.Close()
			if code == http.StatusOK {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// waitPublishedPort polls `docker port <name> <spec>` until the host-published port for a
// container port appears, returning just the port number (e.g. "32771"). spec is like
// "19901/tcp" or "51249/udp". Returns "" on timeout.
func waitPublishedPort(t *testing.T, name, spec string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "port", name, spec).Output()
		// Output is one or more lines like "0.0.0.0:32771" / "[::]:32771"; take the last colon field.
		if err == nil {
			for _, line := range strings.Fields(string(out)) {
				if i := strings.LastIndex(line, ":"); i >= 0 {
					if p := strings.TrimSpace(line[i+1:]); p != "" {
						return p
					}
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return ""
}

// dockerHostCandidates returns addresses, in priority order, at which the docker host's
// published container ports may be reachable from this test container: the DOCKER_HOST TCP
// host, host.docker.internal, the default gateway, and loopback.
func dockerHostCandidates(t *testing.T) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}

	if dh := os.Getenv("DOCKER_HOST"); strings.HasPrefix(dh, "tcp://") {
		if h, _, err := net.SplitHostPort(strings.TrimPrefix(dh, "tcp://")); err == nil {
			add(h)
		}
	}
	if _, err := net.LookupHost("host.docker.internal"); err == nil {
		add("host.docker.internal")
	}
	add(defaultGatewayIP())
	add("127.0.0.1")
	return out
}

// defaultGatewayIP reads the test container's default-route gateway from /proc/net/route,
// returning "" if it cannot be determined. The gateway is often the docker host.
func defaultGatewayIP() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			continue
		}
		// Gateway is stored little-endian in host byte order.
		return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).String()
	}
	return ""
}

// outboundIP returns this container's primary non-loopback IPv4 address — the address
// Envoy (running in a sibling container) uses to reach the test process outbound.
func outboundIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("interface addrs: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	t.Skip("no non-loopback IPv4 address found; cannot reach Envoy container")
	return ""
}

// httpGet returns the body of a GET request, or the error string. For diagnostics only.
func httpGet(url string) string {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "GET error: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return truncate(string(body), 4000)
}

// truncate caps a diagnostic string to at most max characters (0 means no cap).
func truncate(s string, max int) string {
	if max > 0 && len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}

// freeTCPPort grabs an ephemeral TCP port and releases it for the caller to bind.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free tcp port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startHTTPUpstream starts an HTTP server bound to 0.0.0.0:0 (reachable by the Envoy
// container via hostIP) that responds with reply plus the echoed X-Kubevpn-Route header, so a
// test can identify which upstream answered and confirm headers transited Envoy. Returns the
// port; stopped via t.Cleanup.
func startHTTPUpstream(t *testing.T, reply string) int {
	t.Helper()
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen upstream http: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s:%s", reply, r.Header.Get("X-Kubevpn-Route"))
	})}
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Close() })
	return lis.Addr().(*net.TCPAddr).Port
}

// startUDPEcho starts a UDP echo server bound to hostIP:0 that replies with the uppercased
// payload (so a correct reply proves the datagram reached this upstream and returned, not a
// self-bounce inside Envoy). Returns the port; stopped via t.Cleanup.
//
// It binds the specific hostIP rather than 0.0.0.0 on purpose: Envoy's udp_proxy uses a
// *connected* upstream socket, so the kernel only delivers replies whose source is exactly the
// endpoint Envoy forwarded to (hostIP:echoPort). A socket bound to 0.0.0.0 lets the kernel pick
// the reply's source address by route, which on a multi-homed host (e.g. a CI runner where the
// docker bridge gateway shares hostIP's subnet) is NOT hostIP — Envoy then drops the reply and
// the round-trip silently fails. Binding hostIP pins the reply source. This mirrors production,
// where the upstream is the developer's TUN IP and the gvisor LocalUDPForwarder constructs the
// reply with source == tunIP (see pkg/core/gvisor_udp_forwarder.go); the kernel route-selection
// pitfall only exists in this test's direct-socket shortcut around gvisor.
func startUDPEcho(t *testing.T, hostIP string) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(hostIP), Port: 0})
	if err != nil {
		t.Fatalf("listen echo udp: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, readErr := conn.ReadFromUDP(buf)
			if readErr != nil {
				return // conn closed on cleanup
			}
			_, _ = conn.WriteToUDP(bytes.ToUpper(buf[:n]), addr)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// httpRouteGet sends one GET to url with an optional X-Kubevpn-Route header (skipped when
// routeVal is ""), returning the status code and body (0/"" on error).
func httpRouteGet(url, routeVal string) (int, string) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, ""
	}
	if routeVal != "" {
		req.Header.Set("X-Kubevpn-Route", routeVal)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// pollRouteGet retries httpRouteGet until it returns 200 with body==want or attempts run out;
// returns the last status and body observed. Used to wait out Envoy listener warm-up and xDS
// update propagation.
func pollRouteGet(url, routeVal, want string, attempts int) (int, string) {
	var status int
	var body string
	for i := 0; i < attempts; i++ {
		status, body = httpRouteGet(url, routeVal)
		if status == http.StatusOK && body == want {
			return status, body
		}
		time.Sleep(250 * time.Millisecond)
	}
	return status, body
}

// udpRoundTrip dials addr and sends payload, returning the reply ("" if none within attempts).
// UDP is lossy and Envoy may still be warming the listener, so it retries.
func udpRoundTrip(t *testing.T, addr, payload string, attempts int) string {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial udp %s: %v", addr, err)
	}
	defer conn.Close()
	for i := 0; i < attempts; i++ {
		if _, err = conn.Write([]byte(payload)); err != nil {
			t.Fatalf("write udp: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buf := make([]byte, 2048)
		n, readErr := conn.Read(buf)
		if readErr != nil {
			continue
		}
		return string(buf[:n])
	}
	return ""
}

// defaultUDPClientImage is a tiny image with a UDP-capable netcat (busybox `nc -u`),
// used only on macOS to send a datagram from inside Envoy's network namespace.
const defaultUDPClientImage = "busybox:stable"

// udpClientImage returns the image udpRoundTripInContainer runs, overridable for
// offline/mirrored environments via KUBEVPN_E2E_UDP_CLIENT_IMAGE.
func udpClientImage() string {
	if img := os.Getenv("KUBEVPN_E2E_UDP_CLIENT_IMAGE"); img != "" {
		return img
	}
	return defaultUDPClientImage
}

// ensureUDPClientImage makes sure the UDP-client image is present locally, pulling it if
// needed. Skips the test if it is absent and cannot be pulled (mirrors ensureEnvoyImage).
func ensureUDPClientImage(t *testing.T) {
	t.Helper()
	img := udpClientImage()
	if exec.Command("docker", "image", "inspect", img).Run() == nil {
		return
	}
	t.Logf("pulling %s (first run)…", img)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "pull", img).CombinedOutput()
	if err != nil {
		t.Skipf("cannot pull %s (offline?): %v\n%s", img, err, out)
	}
}

// udpProbe performs the UDP round-trip against Envoy's listener, choosing a transport that
// works on the host OS. On Linux the listener's published host port is reachable directly.
// On macOS the docker daemon runs in a VM (colima/lima) whose port-forwarder is TCP-only, so
// published UDP ports never reach the container from the host; we instead send from a sidecar
// container sharing Envoy's network namespace, reaching the listener on localhost inside the VM.
func udpProbe(t *testing.T, ctx context.Context, envoyName, hostUDPAddr string, containerPort int, payload string, attempts int) string {
	t.Helper()
	if runtime.GOOS == "darwin" {
		return udpRoundTripInContainer(t, ctx, envoyName, containerPort, payload, attempts)
	}
	return udpRoundTrip(t, hostUDPAddr, payload, attempts)
}

// udpRoundTripInContainer sends payload to Envoy's UDP listener from a throwaway container
// attached to Envoy's network namespace (so Envoy is reachable at 127.0.0.1:containerPort,
// since its listener binds 0.0.0.0), returning the reply ("" if none). UDP is lossy, so the
// send/receive is retried inside the container shell. Used on macOS where the docker VM does
// not forward UDP host ports (see udpProbe).
func udpRoundTripInContainer(t *testing.T, ctx context.Context, envoyName string, containerPort int, payload string, attempts int) string {
	t.Helper()
	ensureUDPClientImage(t)
	// busybox `nc -u -w1`: send stdin as one datagram, wait up to 1s for the reply, print it.
	script := fmt.Sprintf(
		`for i in $(seq %d); do r=$(printf '%s' | nc -u -w1 127.0.0.1 %d); [ -n "$r" ] && { printf '%%s' "$r"; exit 0; }; done`,
		attempts, payload, containerPort)
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--network", "container:"+envoyName, udpClientImage(), "sh", "-c", script).Output()
	if err != nil {
		t.Logf("in-container udp client error: %v", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// renderBootstrap reads a production Envoy bootstrap file from pkg/inject and renders its
// {{.}} placeholder with addr (the xDS host) — exactly as inject.renderEnvoyConfig does.
func renderBootstrap(t *testing.T, name, addr string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "inject", name))
	if err != nil {
		t.Fatalf("read bootstrap %s: %v", name, err)
	}
	tmpl, err := template.New("").Parse(string(raw))
	if err != nil {
		t.Fatalf("parse bootstrap %s: %v", name, err)
	}
	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, addr); err != nil {
		t.Fatalf("render bootstrap %s: %v", name, err)
	}
	return buf.String()
}
