package core

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
func udpBridgeProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
	listener, err := net.Listen("tcp", node.Addr)
	if err != nil {
		return nil, nil, err
	}
	plog.G(context.Background()).Infof("[UDP-Bridge] Listening on %s", node.Addr)
	return listener, UDPBridgeHandler(), nil
}

func tunProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
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

	// Self-DHCP via control-plane: if net= is empty, request IP from TunConfigService
	if addr == "" {
		var err error
		addr, addr6, err = requestTunIPFromControlPlane()
		if err != nil {
			return nil, nil, fmt.Errorf("request TUN IP from control-plane: %w", err)
		}
	}

	handler := TunHandler(forwarder, hub, nil)
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

// requestTunIPFromControlPlane connects to the traffic manager's TunConfigService
// and allocates a TUN IP for this sidecar.
func requestTunIPFromControlPlane() (ipv4, ipv6 string, err error) {
	trafficManagerAddr := os.Getenv("TrafficManagerService")
	if trafficManagerAddr == "" {
		return "", "", fmt.Errorf("TrafficManagerService env not set")
	}
	ownerID := os.Getenv(config.EnvPodName)
	if ownerID == "" {
		ownerID = "unknown"
	}
	namespace := os.Getenv(config.EnvPodNamespace)

	const controlPlaneDialTimeout = 30 * time.Second
	target := fmt.Sprintf("%s:%d", trafficManagerAddr, config.PortControlPlane)
	ctx, cancel := context.WithTimeout(context.Background(), controlPlaneDialTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return "", "", fmt.Errorf("dial control-plane %s: %w", target, err)
	}
	defer conn.Close()

	hostname, _ := os.Hostname() // sidecar pod hostname, for TUN_ALLOCS debugging
	client := rpc.NewTunConfigServiceClient(conn)
	resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID:   ownerID,
		Namespace: namespace,
		Hostname:  hostname,
	})
	if err != nil {
		return "", "", fmt.Errorf("GetTunIP: %w", err)
	}

	plog.G(ctx).Infof("[TUN] Got IP from control-plane: v4=%s v6=%s", resp.IPv4, resp.IPv6)

	// No explicit release needed — lease expiry (per DHCP protocol) handles IP reclaim.
	// When this process exits, WatchTunIP stream disconnects, and after LeaseDuration
	// without renewal, the IP is automatically reclaimed by the server's lease reaper.

	return resp.IPv4, resp.IPv6, nil
}

// GetTunDeviceIPByName returns the IPs assigned to the named interface within kubevpn CIDRs.
func GetTunDeviceIPByName(tunName string) (ipv4, ipv6, dockerIPv4 net.IP, err error) {
	ifc, err := net.InterfaceByName(tunName)
	if err != nil {
		return
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok {
			if config.CIDR.Contains(ipNet.IP) {
				ipv4 = ipNet.IP
			}
			if config.CIDR6.Contains(ipNet.IP) {
				ipv6 = ipNet.IP
			}
		}
	}
	return
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

func gtcpProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
	handler := GvisorTCPHandler(hub)
	listener, err := GvisorTCPListener(node.Addr)
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}

func gudpProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
	handler := GvisorUDPHandler()
	listener, err := GvisorUDPListener(node.Addr)
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}

func sshProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
	handler := SSHHandler()
	listener, err := SSHListener(node.Addr)
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}
