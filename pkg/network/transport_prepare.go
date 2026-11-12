package network

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/cluster/portforward"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// prepareTransport builds an unpublished generation. Every endpoint must be
// ready before the supervisor can publish it; partial resources are cleaned up.
func (nm *transportSupervisor) prepareTransport(ctx context.Context, kind transportKind) (g *transportGeneration, err error) {
	g = newTransportGeneration(ctx, kind)
	defer func() {
		if err != nil {
			g.cancel()
			for _, h := range g.handles {
				h.Stop()
			}
		}
	}()
	var addresses []string
	if kind == transportSSH {
		addresses, err = nm.prepareSSHGeneration(g)
	} else {
		addresses, err = nm.preparePortForwardGeneration(g)
	}
	if err != nil {
		return g, err
	}
	g.stage = "endpoints"
	if len(addresses) != 3 {
		return g, fmt.Errorf("transport endpoint missing")
	}
	for _, addr := range addresses {
		if addr == "" {
			return g, fmt.Errorf("transport endpoint missing")
		}
	}
	g.endpoints = transportEndpoints{addresses[0], addresses[1], addresses[2]}
	return g, nil
}

func managerTransportPorts() []int {
	return []int{config.PortTCP, config.PortUDP, config.PortXDS}
}

func (nm *transportSupervisor) prepareSSHGeneration(g *transportGeneration) ([]string, error) {
	if nm.cfg.SshConf == nil || nm.cfg.SshConf.IsEmpty() || nm.cfg.Clientset == nil {
		return nil, fmt.Errorf("invalid transport configuration: SSH requires config and client")
	}
	ports := managerTransportPorts()
	addresses := make([]string, 0, len(ports))
	probeCtx, probeCancel := context.WithTimeout(g.ctx, 3*time.Second)
	defer probeCancel()
	g.stage = "service_lookup"
	svc, err := nm.cfg.Clientset.CoreV1().Services(nm.cfg.ManagerNamespace).Get(probeCtx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(svc.Spec.ClusterIP)
	if err != nil {
		return nil, fmt.Errorf("SSH service has no reachable ClusterIP: %w", err)
	}
	g.stage = "ssh_handshake"
	client, err := ssh.DialSshRemote(probeCtx, nm.cfg.SshConf, probeCtx.Done())
	if err != nil {
		return nil, err
	}
	// PortMap listeners are lazy. Validate real SSH channels before publication,
	// without sending fake VPN packets to the manager.
	g.stage = "ssh_channel"
	for _, port := range ports {
		conn, err := client.DialContext(probeCtx, "tcp", netip.AddrPortFrom(ip, uint16(port)).String())
		if err != nil {
			_ = client.Close()
			return nil, err
		}
		_ = conn.Close()
	}
	_ = client.Close()
	g.stage = "ssh_portmap"
	for _, port := range ports {
		h, err := ssh.StartPortMap(g.ctx, nm.cfg.SshConf, netip.AddrPortFrom(ip, uint16(port)), netip.MustParseAddrPort("127.0.0.1:0"))
		if err != nil {
			return nil, err
		}
		g.handles = append(g.handles, h)
		addresses = append(addresses, h.Addr().String())
	}
	return addresses, nil
}

func (nm *transportSupervisor) preparePortForwardGeneration(g *transportGeneration) ([]string, error) {
	if nm.cfg.Config == nil || nm.cfg.RESTClient == nil || nm.cfg.GetRunningPodList == nil {
		return nil, fmt.Errorf("invalid transport configuration: port-forward requires Kubernetes client")
	}
	ports := managerTransportPorts()
	g.stage = "pod_list"
	listCtx, listCancel := context.WithTimeout(g.ctx, portForwardPodListTimeout)
	pods, err := nm.cfg.GetRunningPodList(listCtx)
	listCancel()
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("no running traffic manager pod")
	}
	_ = nm.addRoute(util.GetPodIP(pods[0])...)
	g.stage = "port_forward"
	h, err := portforward.StartPortForwardSession(g.ctx, nm.cfg.Config, nm.cfg.RESTClient, pods[0].Name, nm.cfg.ManagerNamespace, ports)
	if err != nil {
		return nil, err
	}
	g.handles = append(g.handles, h)
	addresses := make([]string, 0, len(ports))
	for _, port := range ports {
		addresses = append(addresses, h.Ports()[port])
	}
	return addresses, nil
}
