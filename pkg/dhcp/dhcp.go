package dhcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"

	"github.com/cilium/ipam/service/allocator"
	"github.com/cilium/ipam/service/ipallocator"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// tunIPPool is the YAML structure stored in the TUN_IP_POOL ConfigMap key.
type tunIPPool struct {
	IPv4 poolEntry `yaml:"ipv4" json:"ipv4"`
	IPv6 poolEntry `yaml:"ipv6" json:"ipv6"`
	// Agent pools are separate from the user pools because the underlying
	// allocator is contiguous (sequential offsets only): without a dedicated
	// segment, an agent lease could not be carved out of the middle of the
	// user pool's bitmap. Absent entries are lazily initialized from
	// config.CIDRAgent/CIDR6Agent for backward compatibility with ConfigMaps
	// written before the agent/user split.
	AgentIPv4 poolEntry `yaml:"agentIpv4,omitempty" json:"agentIpv4,omitempty"`
	AgentIPv6 poolEntry `yaml:"agentIpv6,omitempty" json:"agentIpv6,omitempty"`
}

type poolEntry struct {
	CIDR   string `yaml:"cidr" json:"cidr"`
	Bitmap string `yaml:"bitmap" json:"bitmap"`
}

func parseTunIPPool(data string) (*tunIPPool, error) {
	if data == "" {
		return &tunIPPool{}, nil
	}
	var pool tunIPPool
	if err := yaml.Unmarshal([]byte(data), &pool); err != nil {
		return nil, err
	}
	return &pool, nil
}

func marshalTunIPPool(pool *tunIPPool) (string, error) {
	data, err := yaml.Marshal(pool)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Manager manages DHCP IP allocation and release via a Kubernetes ConfigMap.
type Manager struct {
	clientset kubernetes.Interface
	cidr      *net.IPNet
	cidr6     *net.IPNet
	// Agent pools: separate segments so agent leases never collide with user
	// leases (the per-pool allocators are contiguous — see tunIPPool).
	cidrAgent  *net.IPNet
	cidr6Agent *net.IPNet
	namespace  string
}

// NewDHCPManager creates a Manager for the given namespace using the default CIDRs.
func NewDHCPManager(clientset kubernetes.Interface, namespace string) *Manager {
	return &Manager{
		clientset:  clientset,
		namespace:  namespace,
		cidr:       config.CIDR,
		cidr6:      config.CIDR6,
		cidrAgent:  config.CIDRAgent,
		cidr6Agent: config.CIDR6Agent,
	}
}

// InitDHCP ensures the DHCP ConfigMap exists.
func (m *Manager) InitDHCP(ctx context.Context) error {
	_, err := m.clientset.CoreV1().ConfigMaps(m.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get configmap %s: %w", config.ConfigMapPodTrafficManager, err)
	}

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: m.namespace,
			Labels:    map[string]string{},
		},
		Data: map[string]string{
			config.KeyEnvoy:        "",
			config.KeyTunIPPool:    "",
			config.KeyClusterCIDRs: "",
			config.KeyTunAllocs:    "",
		},
	}
	_, err = m.clientset.CoreV1().ConfigMaps(m.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create configmap: %w", err)
	}
	return nil
}

// RentIP allocates the next available IPv4 and IPv6 addresses from the DHCP pool (user range).
func (m *Manager) RentIP(ctx context.Context) (*net.IPNet, *net.IPNet, error) {
	return m.rentIP(ctx, nil, nil, nil, false)
}

// RentIPExcluding allocates the next available IPv4 and IPv6 addresses (user range),
// skipping any IP that matches a local interface address or appears in excludeIPs.
func (m *Manager) RentIPExcluding(ctx context.Context, excludeIPs []net.IP) (*net.IPNet, *net.IPNet, error) {
	return m.rentIP(ctx, nil, nil, excludeIPs, false)
}

// RentIPPreferring tries to allocate the given preferred IPv4/IPv6 first, so a
// reconnecting client reclaims its previous IP. It falls back to the next
// available address when a preferred IP is unavailable (already allocated, out
// of range, matches a local interface, or is in excludeIPs).
func (m *Manager) RentIPPreferring(ctx context.Context, prefV4, prefV6 net.IP, excludeIPs []net.IP) (*net.IPNet, *net.IPNet, error) {
	return m.rentIP(ctx, prefV4, prefV6, excludeIPs, false)
}

// RentIPForAgent allocates the next available IPv4 and IPv6 addresses from the agent pool.
func (m *Manager) RentIPForAgent(ctx context.Context) (*net.IPNet, *net.IPNet, error) {
	return m.rentIP(ctx, nil, nil, nil, true)
}

// RentIPExcludingForAgent allocates the next available IPv4 and IPv6 addresses from the agent pool,
// skipping any IP that matches a local interface address or appears in excludeIPs.
func (m *Manager) RentIPExcludingForAgent(ctx context.Context, excludeIPs []net.IP) (*net.IPNet, *net.IPNet, error) {
	return m.rentIP(ctx, nil, nil, excludeIPs, true)
}

// InRange reports whether ip falls within the managed IPv4 or IPv6 CIDR range.
func (m *Manager) InRange(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.To4() != nil {
		return m.cidr != nil && m.cidr.Contains(ip)
	}
	return m.cidr6 != nil && m.cidr6.Contains(ip)
}

// RentSpecificIP allocates exactly the given IPv4/IPv6 addresses, failing if any
// is unavailable (already allocated or out of range). Unlike RentIPPreferring it
// does NOT fall back to another address — used when an operator pins a specific
// TUN IP (via TUN_ALLOCS) and the caller wants the exact IP or a hard error so it
// can decide whether to reclaim it from the current holder. A nil v4 or v6 skips
// that family. The write is atomic: if either family fails, nothing is allocated.
func (m *Manager) RentSpecificIP(ctx context.Context, v4, v6 net.IP) error {
	plog.G(ctx).Debugf("Renting specific IP: v4=%v v6=%v", v4, v6)
	return m.retryUpdate(ctx, func(ipv4, ipv6, ipv4Agent, ipv6Agent *ipallocator.Range) error {
		allocFor := func(ip net.IP) *ipallocator.Range {
			if m.inAgentSegment(ip) {
				if ip.To4() != nil {
					return ipv4Agent
				}
				return ipv6Agent
			}
			if ip.To4() != nil {
				return ipv4
			}
			return ipv6
		}
		if v4 != nil {
			if err := allocFor(v4).Allocate(v4); err != nil {
				return fmt.Errorf("cannot allocate IPv4 %s: %w", v4, err)
			}
		}
		if v6 != nil {
			if err := allocFor(v6).Allocate(v6); err != nil {
				return fmt.Errorf("cannot allocate IPv6 %s: %w", v6, err)
			}
		}
		return nil
	})
}

// makeShouldSkip returns a predicate reporting whether an IP must be skipped
// because it matches a local interface address or appears in excludeIPs.
func makeShouldSkip(excludeIPs []net.IP) func(net.IP) bool {
	addrs, _ := net.InterfaceAddrs()
	return func(ip net.IP) bool {
		for _, addr := range addrs {
			if addrIP, ok := addr.(*net.IPNet); ok && addrIP.IP.Equal(ip) {
				return true
			}
		}
		for _, excluded := range excludeIPs {
			if excluded.Equal(ip) {
				return true
			}
		}
		return false
	}
}

// makeShouldSkipForRange returns a predicate reporting whether an IP must be skipped
// because it's outside the allowed range, matches a local interface address, or appears in excludeIPs.
func makeShouldSkipForRange(excludeIPs []net.IP, allowedCIDR *net.IPNet) func(net.IP) bool {
	baseSkip := makeShouldSkip(excludeIPs)
	return func(ip net.IP) bool {
		if allowedCIDR != nil && !allowedCIDR.Contains(ip) {
			return true
		}
		return baseSkip(ip)
	}
}

// allocateOne allocates one address from r: the preferred IP if given and
// usable, otherwise the next available one. IPs skipped along the way are
// appended to useless for later release.
func allocateOne(r *ipallocator.Range, preferred net.IP, shouldSkip func(net.IP) bool, useless *[]net.IP) (net.IP, error) {
	if preferred != nil && !shouldSkip(preferred) {
		if err := r.Allocate(preferred); err == nil {
			return preferred, nil
		}
		// preferred unavailable (already allocated / out of range) → fall back
	}
	for {
		ip, err := r.AllocateNext()
		if err != nil {
			return nil, fmt.Errorf("allocate next IP: %w: %w", err, config.ErrDHCPExhausted)
		}
		if !shouldSkip(ip) {
			return ip, nil
		}
		*useless = append(*useless, ip)
	}
}

// allocateOneInRange allocates one address from r within the specified CIDR range:
// the preferred IP if given, in range and usable, otherwise the next available one in range.
// IPs skipped along the way are appended to useless for later release.
func allocateOneInRange(r *ipallocator.Range, preferred net.IP, shouldSkip func(net.IP) bool, useless *[]net.IP, allowedCIDR *net.IPNet) (net.IP, error) {
	if preferred != nil && allowedCIDR.Contains(preferred) && !shouldSkip(preferred) {
		if err := r.Allocate(preferred); err == nil {
			return preferred, nil
		}
		// preferred unavailable (already allocated / out of range) → fall back
	}

	// First, try to find an available IP by directly checking the allowed range
	// This is more efficient than using AllocateNext() for non-starting ranges
	startIP := allowedCIDR.IP.Mask(allowedCIDR.Mask)
	// Create a copy to iterate on
	ip := make(net.IP, len(startIP))
	copy(ip, startIP)

	// Limit iterations to the size of a /17 (32768 IPs) to prevent infinite loops
	for i := 0; i < 32768; i++ {
		if !allowedCIDR.Contains(ip) {
			break // reached end of the allowed CIDR
		}
		if !shouldSkip(ip) {
			if err := r.Allocate(ip); err == nil {
				// Success! We found an available IP in the allowed range
				return ip, nil
			}
			// IP already allocated, try next one
		}
		// Increment to next IP
		for j := len(ip) - 1; j >= 0; j-- {
			ip[j]++
			if ip[j] != 0 {
				break // no more carry-over needed
			}
		}
	}

	// If the above didn't work, use the AllocateNext method but with a safety limit
	// This might happen if the allocator has internal state we can't easily predict
	// Limit to 65536 iterations (size of a /16) to prevent hanging
	for i := 0; i < 65536; i++ {
		ip, err := r.AllocateNext()
		if err != nil {
			return nil, fmt.Errorf("allocate next IP: %w: %w", err, config.ErrDHCPExhausted)
		}
		if allowedCIDR.Contains(ip) && !shouldSkip(ip) {
			// Success! We found an available IP in the allowed range
			return ip, nil
		}
		*useless = append(*useless, ip)
	}

	// If all else fails, return an error
	return nil, fmt.Errorf("allocate next IP: %w", config.ErrDHCPExhausted)
}

func (m *Manager) rentIP(ctx context.Context, prefV4, prefV6 net.IP, excludeIPs []net.IP, isAgent bool) (*net.IPNet, *net.IPNet, error) {
	plog.G(ctx).Debugf("Renting IP from DHCP in namespace %s (isAgent=%v)", m.namespace, isAgent)

	skipV4 := makeShouldSkip(excludeIPs)
	skipV6 := makeShouldSkip(excludeIPs)

	var uselessIPs []net.IP
	var v4, v6 net.IP
	// Not folded into retryUpdate: uselessIPs must be reset once per retry
	// attempt (before updateDHCPConfigMap runs), so the reset stays in the outer
	// retry closure rather than inside the mutate func.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		uselessIPs = nil
		return m.updateDHCPConfigMap(ctx, func(ipv4, ipv6, ipv4Agent, ipv6Agent *ipallocator.Range) (err error) {
			// Route to the segment-scoped allocator: each pool's allocator is
			// contiguous, so its AllocateNext is always in-segment — no
			// walk-and-skip over the other segment is needed (and for the IPv6
			// agent segment it would be mathematically impossible).
			ipv4Pool, ipv6Pool := ipv4, ipv6
			if isAgent {
				ipv4Pool, ipv6Pool = ipv4Agent, ipv6Agent
			}

			// Try preferred first
			if prefV4 != nil && !skipV4(prefV4) {
				if v4, err = allocateOne(ipv4Pool, prefV4, skipV4, &uselessIPs); err != nil {
					// preferred unavailable (already allocated / out of segment)
					v4, err = allocateOne(ipv4Pool, nil, skipV4, &uselessIPs)
				}
			} else {
				v4, err = allocateOne(ipv4Pool, nil, skipV4, &uselessIPs)
			}
			if err != nil {
				return err
			}

			// Same for v6
			if prefV6 != nil && !skipV6(prefV6) {
				if v6, err = allocateOne(ipv6Pool, prefV6, skipV6, &uselessIPs); err != nil {
					v6, err = allocateOne(ipv6Pool, nil, skipV6, &uselessIPs)
				}
			} else {
				v6, err = allocateOne(ipv6Pool, nil, skipV6, &uselessIPs)
			}
			return err
		})
	})
	if len(uselessIPs) != 0 {
		if er := m.releaseIP(ctx, uselessIPs...); er != nil {
			plog.G(ctx).Errorf("Failed to release useless IPs: %v", er)
		}
	}
	if err != nil {
		plog.G(ctx).Errorf("Failed to rent IP from DHCP server: %v", err)
		return nil, nil, err
	}
	// Stamp a host mask (/32, /128), not the pool mask. The pool (m.cidr/m.cidr6)
	// stays the allocation range for InRange/the allocator; a lease only identifies
	// one host. The client forces a host mask on its TUN device anyway, and the
	// sidecar gets pool reachability from its explicit route=CIDR4 — so no consumer
	// needs the pool prefix on the lease.
	v4Net := &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}
	v6Net := &net.IPNet{IP: v6, Mask: net.CIDRMask(128, 128)}
	plog.G(ctx).Debugf("Rented IP: v4=%s v6=%s", v4Net, v6Net)
	return v4Net, v6Net, nil
}

// ReleaseIP returns the given IPv4 and IPv6 addresses back to the DHCP pool.
func (m *Manager) ReleaseIP(ctx context.Context, v4, v6 net.IP) error {
	plog.G(ctx).Debugf("Releasing IP: v4=%v v6=%v", v4, v6)
	return m.retryUpdate(ctx, func(ipv4, ipv6, ipv4Agent, ipv6Agent *ipallocator.Range) error {
		allocFor := func(ip net.IP) *ipallocator.Range {
			if m.inAgentSegment(ip) {
				if ip.To4() != nil {
					return ipv4Agent
				}
				return ipv6Agent
			}
			if ip.To4() != nil {
				return ipv4
			}
			return ipv6
		}
		if v4 != nil {
			if err := allocFor(v4).Release(v4); err != nil {
				return err
			}
		}
		if v6 != nil {
			if err := allocFor(v6).Release(v6); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReleaseIPs returns the given IPs (any mix of IPv4/IPv6) back to the pool,
// auto-detecting the address family of each. Used to reclaim orphaned bitmap
// bits that have no owning lease.
func (m *Manager) ReleaseIPs(ctx context.Context, ips ...net.IP) error {
	return m.releaseIP(ctx, ips...)
}

func (m *Manager) releaseIP(ctx context.Context, ips ...net.IP) error {
	if len(ips) == 0 {
		return nil
	}
	return m.retryUpdate(ctx, func(ipv4, ipv6, ipv4Agent, ipv6Agent *ipallocator.Range) error {
		for _, ip := range ips {
			var pool *ipallocator.Range
			if m.inAgentSegment(ip) {
				if ip.To4() != nil {
					pool = ipv4Agent
				} else {
					pool = ipv6Agent
				}
			} else {
				if ip.To4() != nil {
					pool = ipv4
				} else {
					pool = ipv6
				}
			}
			if err := pool.Release(ip); err != nil {
				return err
			}
		}
		return nil
	})
}

// retryUpdate runs f against the DHCP ConfigMap under RetryOnConflict. f
// receives four allocators: the user pools (v4/v6) and the agent pools
// (v4/v6). Callers route by connection type — user leases go to the user
// pools, agent leases (KUBEVPN_AGENT_CONNECTION) to the agent pools. The
// per-pool allocators are contiguous, so mixing types in one pool is not
// possible; that is why the agent pools exist (see tunIPPool).
func (m *Manager) retryUpdate(ctx context.Context, f func(ipv4, ipv6, ipv4Agent, ipv6Agent *ipallocator.Range) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return m.updateDHCPConfigMap(ctx, f)
	})
}

// agentPoolCIDRs returns the CIDRs for the agent pool entries, preferring the
// CIDR persisted in the ConfigMap entry (so a bitmap written by an older
// manager keeps its segment) and falling back to the config defaults.
func (m *Manager) agentPoolCIDRs(pool *tunIPPool) (v4, v6 *net.IPNet) {
	v4, v6 = m.cidrAgent, m.cidr6Agent
	if v4 == nil {
		v4 = config.CIDRAgent
	}
	if v6 == nil {
		v6 = config.CIDR6Agent
	}
	if pool.AgentIPv4.CIDR != "" {
		if _, parsed, err := net.ParseCIDR(pool.AgentIPv4.CIDR); err == nil {
			v4 = parsed
		}
	}
	if pool.AgentIPv6.CIDR != "" {
		if _, parsed, err := net.ParseCIDR(pool.AgentIPv6.CIDR); err == nil {
			v6 = parsed
		}
	}
	return v4, v6
}

// inAgentSegment reports whether ip belongs to the agent address segment.
func (m *Manager) inAgentSegment(ip net.IP) bool {
	if ip.To4() != nil {
		return m.cidrAgent != nil && m.cidrAgent.Contains(ip)
	}
	return m.cidr6Agent != nil && m.cidr6Agent.Contains(ip)
}

// restoreAllocator creates a bitmap allocator for cidr and restores its state from the
// base64-encoded bitmap. It is the shared primitive for loading an IP pool from a
// ConfigMap entry, used by both the write-back path (updateDHCPConfigMap) and the
// read-only path (ForEach), which previously each duplicated the v4+v6 build+decode+
// restore sequence.
//
// strict controls decode-error handling:
//   - strict=true  (write-back path): an empty bitmap short-circuits to a fresh empty
//     allocator, and a decode error surfaces — corrupted state must not be silently
//     written back. (Matches the old `if bitmap != ""` + `if decErr != nil { return }`.)
//   - strict=false (read-only path): a decode error is swallowed and an empty allocator
//     returned — a corrupt bitmap degrades to iterating 0 IPs rather than failing the
//     read. (Matches the old `if b, decErr := ...; decErr == nil` tolerate-and-skip.)
//
// A Restore error surfaces in both modes (a structurally valid bitmap for the wrong
// CIDR is a hard error either way).
func restoreAllocator(cidr *net.IPNet, bitmap string, strict bool) (*ipallocator.Range, error) {
	r, err := ipallocator.NewAllocatorCIDRRange(cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return nil, err
	}
	if strict && bitmap == "" {
		return r, nil
	}
	b, decErr := base64.StdEncoding.DecodeString(bitmap)
	if decErr != nil {
		if strict {
			return nil, decErr
		}
		return r, nil
	}
	if err = r.Restore(cidr, b); err != nil {
		return nil, err
	}
	return r, nil
}

func (m *Manager) updateDHCPConfigMap(ctx context.Context, f func(ipv4, ipv6, ipv4Agent, ipv6Agent *ipallocator.Range) error) error {
	cm, err := m.clientset.CoreV1().ConfigMaps(m.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get configmap DHCP server: %w", err)
	}
	if cm.Data == nil {
		return fmt.Errorf("configmap is empty")
	}

	pool, err := parseTunIPPool(cm.Data[config.KeyTunIPPool])
	if err != nil {
		return fmt.Errorf("failed to parse TUN_IP_POOL: %w", err)
	}

	dhcp, err := restoreAllocator(m.cidr, pool.IPv4.Bitmap, true)
	if err != nil {
		return err
	}
	dhcp6, err := restoreAllocator(m.cidr6, pool.IPv6.Bitmap, true)
	if err != nil {
		return err
	}

	// Agent pools: lazily initialized from the agent CIDRs when the ConfigMap
	// predates the agent/user split (backward compatible with old managers).
	agentCIDR4, agentCIDR6 := m.agentPoolCIDRs(pool)
	dhcpAgent, err := restoreAllocator(agentCIDR4, pool.AgentIPv4.Bitmap, pool.AgentIPv4.Bitmap != "")
	if err != nil {
		return err
	}
	dhcpAgent6, err := restoreAllocator(agentCIDR6, pool.AgentIPv6.Bitmap, pool.AgentIPv6.Bitmap != "")
	if err != nil {
		return err
	}

	if err = f(dhcp, dhcp6, dhcpAgent, dhcpAgent6); err != nil {
		return err
	}

	var bytes []byte
	if _, bytes, err = dhcp.Snapshot(); err != nil {
		return err
	}
	pool.IPv4 = poolEntry{CIDR: m.cidr.String(), Bitmap: base64.StdEncoding.EncodeToString(bytes)}
	if _, bytes, err = dhcp6.Snapshot(); err != nil {
		return err
	}
	pool.IPv6 = poolEntry{CIDR: m.cidr6.String(), Bitmap: base64.StdEncoding.EncodeToString(bytes)}
	if _, bytes, err = dhcpAgent.Snapshot(); err != nil {
		return err
	}
	pool.AgentIPv4 = poolEntry{CIDR: agentCIDR4.String(), Bitmap: base64.StdEncoding.EncodeToString(bytes)}
	if _, bytes, err = dhcpAgent6.Snapshot(); err != nil {
		return err
	}
	pool.AgentIPv6 = poolEntry{CIDR: agentCIDR6.String(), Bitmap: base64.StdEncoding.EncodeToString(bytes)}

	poolStr, err := marshalTunIPPool(pool)
	if err != nil {
		return err
	}
	patch, err := json.Marshal([]map[string]string{{
		"op":    "add",
		"path":  "/data/" + config.KeyTunIPPool,
		"value": poolStr,
	}})
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}
	_, err = m.clientset.CoreV1().ConfigMaps(m.namespace).Patch(ctx, config.ConfigMapPodTrafficManager, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch TUN_IP_POOL: %w", err)
	}
	return nil
}

// ForEach iterates over all allocated IPv4 and IPv6 addresses, calling the respective callback for each.
func (m *Manager) ForEach(ctx context.Context, fnv4 func(net.IP), fnv6 func(net.IP)) error {
	cm, err := m.clientset.CoreV1().ConfigMaps(m.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get DHCP server configmap: %w", err)
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}

	pool, err := parseTunIPPool(cm.Data[config.KeyTunIPPool])
	if err != nil {
		return fmt.Errorf("failed to parse TUN_IP_POOL: %w", err)
	}

	dhcp, err := restoreAllocator(m.cidr, pool.IPv4.Bitmap, false)
	if err != nil {
		return err
	}
	dhcp.ForEach(fnv4)

	dhcp6, err := restoreAllocator(m.cidr6, pool.IPv6.Bitmap, false)
	if err != nil {
		return err
	}
	dhcp6.ForEach(fnv6)
	return nil
}
