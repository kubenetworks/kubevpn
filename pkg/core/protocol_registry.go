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

	handler := TunHandler(forwarder, hub)
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

	// Start background watcher for IP changes (hot-update)
	if addr != "" && node.Get("net") == "" {
		go watchTunIPChanges(listener)
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

	target := fmt.Sprintf("%s:%d", trafficManagerAddr, config.PortControlPlane)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return "", "", fmt.Errorf("dial control-plane %s: %w", target, err)
	}
	defer conn.Close()

	client := rpc.NewTunConfigServiceClient(conn)
	resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID:   ownerID,
		Namespace: namespace,
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

// watchTunIPChanges connects to TunConfigService.WatchTunIP and hot-updates the TUN device
// when the IP allocation changes. Falls back to periodic polling if the stream breaks.
func watchTunIPChanges(listener net.Listener) {
	trafficManagerAddr := os.Getenv("TrafficManagerService")
	ownerID := os.Getenv(config.EnvPodName)
	namespace := os.Getenv(config.EnvPodNamespace)
	target := fmt.Sprintf("%s:%d", trafficManagerAddr, config.PortControlPlane)

	if trafficManagerAddr == "" || ownerID == "" {
		return
	}

	var currentVersion int64
	ctx := context.Background()

	for {
		conn, err := grpc.DialContext(ctx, target, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			time.Sleep(10 * time.Second)
			continue
		}
		client := rpc.NewTunConfigServiceClient(conn)

		stream, err := client.WatchTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerID, Namespace: namespace})
		if err != nil {
			conn.Close()
			// Fallback: poll every 30s
			pollTunIP(ctx, target, ownerID, namespace, &currentVersion)
			continue
		}

		for {
			resp, recvErr := stream.Recv()
			if recvErr != nil {
				break
			}
			if resp.Version != currentVersion && currentVersion != 0 {
				applyTunIPChange(resp)
			}
			currentVersion = resp.Version
		}
		conn.Close()
		time.Sleep(5 * time.Second)
	}
}

func pollTunIP(ctx context.Context, target, ownerID, namespace string, version *int64) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for i := 0; i < 10; i++ {
		select {
		case <-ticker.C:
			conn, err := grpc.DialContext(ctx, target, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				continue
			}
			client := rpc.NewTunConfigServiceClient(conn)
			resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerID, Namespace: namespace})
			conn.Close()
			if err == nil && resp.Version != *version && *version != 0 {
				applyTunIPChange(resp)
				*version = resp.Version
			}
			if err == nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func applyTunIPChange(resp *rpc.TunIPResponse) {
	ctx := context.Background()
	interfaces, err := net.Interfaces()
	if err != nil {
		return
	}

	var tunName string
	for _, ifc := range interfaces {
		addrs, _ := ifc.Addrs()
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && config.CIDR.Contains(ipNet.IP) {
				tunName = ifc.Name
				break
			}
		}
		if tunName != "" {
			break
		}
	}
	if tunName == "" {
		plog.G(ctx).Warn("[TUN] Cannot find TUN device for IP update")
		return
	}

	oldIPv4, oldIPv6, _, _ := GetTunDeviceIPByName(tunName)

	if resp.IPv4 != "" {
		oldAddr := ""
		if oldIPv4 != nil {
			oldAddr = (&net.IPNet{IP: oldIPv4, Mask: net.CIDRMask(32, 32)}).String()
		}
		if err := tun.ChangeIP(tunName, oldAddr, resp.IPv4); err != nil {
			plog.G(ctx).Errorf("[TUN] ChangeIP v4 failed: %v", err)
			return
		}
		newIP, _, _ := net.ParseCIDR(resp.IPv4)
		tun.UpdateDNAT(oldIPv4, newIP)
	}
	if resp.IPv6 != "" && oldIPv6 != nil {
		oldAddr := (&net.IPNet{IP: oldIPv6, Mask: net.CIDRMask(128, 128)}).String()
		_ = tun.ChangeIP(tunName, oldAddr, resp.IPv6)
	}

	plog.G(ctx).Infof("[TUN] IP hot-updated: v4=%s v6=%s", resp.IPv4, resp.IPv6)
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
