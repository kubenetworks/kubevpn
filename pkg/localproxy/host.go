package localproxy

import (
	"context"
	"net"
	"strconv"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// HostConnector implements Connector by dialing targets directly from the host, using
// the host's own DNS resolution and network stack. Combined with the SOCKS5/HTTP CONNECT
// servers passing the requested hostname through untouched (socks5h semantics), it lets a
// proxy client reach anything the host itself can reach: the public internet via the host's
// default route, and — when a KubeVPN TUN is active and has installed the cluster routes and
// DNS — cluster Services/Pods too. Unlike ClusterConnector it performs no Kubernetes API
// lookup and no port-forward, so it needs no cluster client.
type HostConnector struct {
	dialer net.Dialer
}

// NewHostConnector returns a HostConnector whose per-dial timeout is bounded by
// config.DialTimeout, so an unreachable target fails within a known window rather than
// hanging on the OS default.
func NewHostConnector() *HostConnector {
	return &HostConnector{dialer: net.Dialer{Timeout: config.DialTimeout}}
}

// Connect dials host:port directly from the host. host may be a hostname (resolved by the
// host resolver — socks5h) or an IP literal.
func (c *HostConnector) Connect(ctx context.Context, host string, port int) (net.Conn, error) {
	return c.dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}
