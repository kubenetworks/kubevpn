package endpoint

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel"
)

// inClusterNamespaceFile is the projected service-account namespace path. Its presence
// means this process runs as an in-cluster pod — i.e. this hub may be the traffic
// manager's own data plane, the only place where reserving the manager ClusterIP makes
// sense. Developer machines and CI runners have no such file, so the reservation degrades
// to a no-op there without any DNS round trip.
const inClusterNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// managerServiceIPTimeout bounds the one-shot ClusterIP lookup at server startup.
const managerServiceIPTimeout = 3 * time.Second

// reserveManagerServiceIP marks the traffic manager's own Service ClusterIP as hub-owned
// on hub, so the route table never learns a route for it (routeTable.addRoute) and every
// packet destined to it is handled by the local dial-out gvisor forwarder
// (dispatchServerDataPacket), never routed to a client conn.
//
// Failure prevented: the route-table self-registration loop. The hub learns routes from
// packet source IPs, and a client's gvisor stack answers a bounced packet with the bounced
// destination as its source. Once that destination is the manager ClusterIP, the learned
// route hijacks every client's flow to the ClusterIP (DNS :53, xDS :9002) away from the
// dial-out forwarder and bounces it back to clients — whose spoofed replies re-register the
// route — black-holing ClusterIP traffic for the connection's lifetime.
//
// The ClusterIP is resolved once per server startup by looking up the manager Service's
// DNS name from inside the cluster. Best-effort: any failure (not running as an in-cluster
// pod, no DNS answer) skips the reservation and leaves the previous behavior unchanged.
// Called for the gvisor TCP listener — the path client data connections terminate on.
func reserveManagerServiceIP(hub *tunnel.Router) {
	if hub == nil {
		return
	}
	namespaceBytes, err := os.ReadFile(inClusterNamespaceFile)
	if err != nil {
		// Not an in-cluster pod: this hub is a client-side or SSH-relay data plane,
		// not the traffic manager's own. Nothing to reserve.
		return
	}
	namespace := strings.TrimSpace(string(namespaceBytes))
	if namespace == "" {
		namespace = os.Getenv(config.EnvPodNamespace)
	}
	if namespace == "" {
		return
	}
	lookupCtx, cancel := context.WithTimeout(context.Background(), managerServiceIPTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, fmt.Sprintf("%s.%s.svc", config.ConfigMapPodTrafficManager, namespace))
	if err != nil || len(addrs) == 0 {
		plog.G(context.Background()).Debugf("[Route] Could not resolve %s.%s.svc to reserve the manager ClusterIP: %v",
			config.ConfigMapPodTrafficManager, namespace, err)
		return
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ips = append(ips, addr.IP)
	}
	hub.ReserveSelfIPs(ips...)
	pretty := make([]string, 0, len(ips))
	for _, ip := range ips {
		pretty = append(pretty, ip.String())
	}
	plog.G(context.Background()).Debugf("[Route] Reserved hub-owned IP(s) %v (%s.%s): flows to them stay on the local gvisor forwarder",
		pretty, config.ConfigMapPodTrafficManager, namespace)
}
