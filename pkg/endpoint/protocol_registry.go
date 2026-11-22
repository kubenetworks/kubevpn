package endpoint

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel"
)

func init() {
	RegisterProtocol("tun", tunProtocolFactory)
	RegisterProtocol("gtcp", gtcpProtocolFactory)
	RegisterProtocol("gudp", gudpProtocolFactory)
	RegisterProtocol("ssh", sshProtocolFactory)
	RegisterProtocol("udpbridge", udpBridgeProtocolFactory)
}

// udpBridgeProtocolFactory creates the reverse-UDP bridge listener used by the
// fargate sidecar to carry UDP inbound back to the developer (the SSH reverse
// tunnel is TCP-only).
func udpBridgeProtocolFactory(node *Node, hub *tunnel.Router) (net.Listener, Handler, error) {
	listener, err := net.Listen("tcp", node.Addr)
	if err != nil {
		return nil, nil, err
	}
	plog.G(context.Background()).Debugf("[UDP-Bridge] Listening on %s", node.Addr)
	return listener, UDPBridgeHandler(), nil
}

func tunProtocolFactory(node *Node, hub *tunnel.Router) (net.Listener, Handler, error) {
	var forwarder *Forwarder
	if node.Forward != "" {
		var err error
		forwarder, err = ParseForwarder(node.Forward)
		if err != nil {
			return nil, nil, err
		}
	}

	addr := node.Get("net")
	addr6 := node.Get("net6")

	// Self-DHCP via xds: if net= is empty, request IP from TunConfigService.
	// agent=<podUID> marks this tunnel as a node-agent connection: its lease is
	// allocated from the agent pool and recorded with OwnerID=<podUID>
	// IsAgent=true, so the traffic manager can tell agent tunnels apart.
	// Without the param (mesh sidecar / user data plane) identity is unchanged.
	if addr == "" {
		var err error
		addr, addr6, err = requestTunIPFromXDS(node.Get("agent"))
		if err != nil {
			return nil, nil, fmt.Errorf("request TUN IP from xds: %w", err)
		}
	}

	handler := tunnel.TunHandler(forwarder.TunnelConfig(), hub, nil)
	listener, err := tun.Listener(tun.Config{
		Name:    node.Get("name"),
		Addr:    addr,
		Addr6:   addr6,
		MTU:     node.GetInt("mtu"),
		Routes:  parseRoutes(node.Get("route")),
		Gateway: node.Get("gw"),
	})
	if err != nil {
		return nil, nil, err
	}

	return listener, handler, nil
}

// agentLeaseHeartbeatInterval is how often a node-agent's vpn container renews
// its TUN lease (see keepAgentLeaseAlive). The traffic manager's agent
// reconciler treats a lease as alive while LastRenew is recent (<30s), so the
// heartbeat must be well under that window.
const agentLeaseHeartbeatInterval = 5 * time.Second

// resolveTunIPOwner maps the -l URL's agent= query param to the GetTunIP
// identity. A non-empty param means "this is a node-agent tunnel": the pod UID
// becomes the lease owner and IsAgent=true routes the lease to the agent pool.
// An empty param keeps the legacy sidecar identity (POD_NAME env, else
// "unknown") with IsAgent=false — user/mesh connections are unaffected.
func resolveTunIPOwner(agentUID string) (ownerID string, isAgent bool) {
	if agentUID != "" {
		return agentUID, true
	}
	ownerID = os.Getenv(config.EnvPodName)
	if ownerID == "" {
		ownerID = "unknown"
	}
	return ownerID, false
}

// requestTunIPFromXDS connects to the traffic manager's TunConfigService
// and allocates a TUN IP for this sidecar.
func requestTunIPFromXDS(agentUID string) (ipv4, ipv6 string, err error) {
	trafficManagerAddr := os.Getenv("TrafficManagerService")
	if trafficManagerAddr == "" {
		return "", "", fmt.Errorf("env TrafficManagerService not set")
	}
	ownerID, isAgent := resolveTunIPOwner(agentUID)
	namespace := os.Getenv(config.EnvPodNamespace)

	const controlPlaneDialTimeout = 30 * time.Second
	target := fmt.Sprintf("%s:%d", trafficManagerAddr, config.PortXDS)
	ctx, cancel := context.WithTimeout(context.Background(), controlPlaneDialTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return "", "", fmt.Errorf("dial xds %s: %w", target, err)
	}

	hostname, _ := os.Hostname() // sidecar pod hostname, for TUN_ALLOCS debugging
	client := rpc.NewTunConfigServiceClient(conn)
	req := &rpc.TunIPRequest{
		OwnerID:   ownerID,
		Namespace: namespace,
		Hostname:  hostname,
		IsAgent:   isAgent,
	}
	resp, err := client.GetTunIP(ctx, req)
	if err != nil {
		conn.Close()
		return "", "", fmt.Errorf("get tun IP: %w", err)
	}

	plog.G(ctx).Debugf("[TUN] Got IP from xds: v4=%s v6=%s (owner=%s isAgent=%v)", resp.IPv4, resp.IPv6, ownerID, isAgent)

	// No explicit release needed — lease expiry (per DHCP protocol) handles IP reclaim.
	// When this process exits, the lease is no longer renewed and, after LeaseDuration
	// without renewal, the IP is automatically reclaimed by the server's lease reaper.
	if isAgent {
		// A one-shot GetTunIP would let the lease lapse after LeaseDuration: the
		// reaper would reclaim the agent's IP and the manager's agent reconciler
		// (lease-heartbeat verification) would consider the dataplane dead. Renew
		// periodically for the process lifetime; conn is deliberately left open.
		go keepAgentLeaseAlive(client, req)
	} else {
		conn.Close()
	}

	return resp.IPv4, resp.IPv6, nil
}

// keepAgentLeaseAlive renews the agent's TUN lease by re-issuing the same
// GetTunIP request (idempotent: an existing allocation is renewed in place,
// keeping its IP). Runs until the process exits; a failed renewal is logged and
// retried on the next tick — if the tunnel process is truly gone, the lease
// goes stale and the manager's reconciler re-creates the agent Job.
func keepAgentLeaseAlive(client rpc.TunConfigServiceClient, req *rpc.TunIPRequest) {
	ticker := time.NewTicker(agentLeaseHeartbeatInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), agentLeaseHeartbeatInterval)
		if _, err := client.GetTunIP(ctx, req); err != nil {
			plog.G(ctx).Warnf("[TUN] agent lease heartbeat failed: %v", err)
		}
		cancel()
	}
}

func parseRoutes(str string) []types.Route {
	var routes []types.Route
	for _, s := range strings.Split(str, ",") {
		if _, ipNet, _ := net.ParseCIDR(strings.TrimSpace(s)); ipNet != nil {
			routes = append(routes, types.Route{Dst: *ipNet})
		}
	}
	return routes
}

func gtcpProtocolFactory(node *Node, hub *tunnel.Router) (net.Listener, Handler, error) {
	handler := tunnel.GvisorTCPHandler(hub)
	listener, err := GvisorTCPListener(node.Addr)
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}

func gudpProtocolFactory(node *Node, hub *tunnel.Router) (net.Listener, Handler, error) {
	handler := GvisorUDPHandler()
	listener, err := GvisorUDPListener(node.Addr)
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}

func sshProtocolFactory(node *Node, hub *tunnel.Router) (net.Listener, Handler, error) {
	handler := SSHHandler()
	listener, err := SSHListener(node.Addr)
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}
