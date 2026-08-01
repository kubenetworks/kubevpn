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
	namespace string
}

// NewDHCPManager creates a Manager for the given namespace using the default CIDRs.
func NewDHCPManager(clientset kubernetes.Interface, namespace string) *Manager {
	return &Manager{
		clientset: clientset,
		namespace: namespace,
		cidr:      config.CIDR,
		cidr6:     config.CIDR6,
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

// RentIP allocates the next available IPv4 and IPv6 addresses from the DHCP pool.
func (m *Manager) RentIP(ctx context.Context) (*net.IPNet, *net.IPNet, error) {
	return m.rentIP(ctx, nil, nil, nil)
}

// RentIPExcluding allocates the next available IPv4 and IPv6 addresses,
// skipping any IP that matches a local interface address or appears in excludeIPs.
func (m *Manager) RentIPExcluding(ctx context.Context, excludeIPs []net.IP) (*net.IPNet, *net.IPNet, error) {
	return m.rentIP(ctx, nil, nil, excludeIPs)
}

// RentIPPreferring tries to allocate the given preferred IPv4/IPv6 first, so a
// reconnecting client reclaims its previous IP. It falls back to the next
// available address when a preferred IP is unavailable (already allocated, out
// of range, matches a local interface, or is in excludeIPs).
func (m *Manager) RentIPPreferring(ctx context.Context, prefV4, prefV6 net.IP, excludeIPs []net.IP) (*net.IPNet, *net.IPNet, error) {
	return m.rentIP(ctx, prefV4, prefV6, excludeIPs)
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
	plog.G(ctx).Infof("Renting specific IP: v4=%v v6=%v", v4, v6)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return m.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) error {
			if v4 != nil {
				if err := ipv4.Allocate(v4); err != nil {
					return fmt.Errorf("cannot allocate IPv4 %s: %w", v4, err)
				}
			}
			if v6 != nil {
				if err := ipv6.Allocate(v6); err != nil {
					return fmt.Errorf("cannot allocate IPv6 %s: %w", v6, err)
				}
			}
			return nil
		})
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

func (m *Manager) rentIP(ctx context.Context, prefV4, prefV6 net.IP, excludeIPs []net.IP) (*net.IPNet, *net.IPNet, error) {
	plog.G(ctx).Debugf("Renting IP from DHCP in namespace %s", m.namespace)
	shouldSkip := makeShouldSkip(excludeIPs)
	var uselessIPs []net.IP
	var v4, v6 net.IP
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		uselessIPs = nil
		return m.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) (err error) {
			if v4, err = allocateOne(ipv4, prefV4, shouldSkip, &uselessIPs); err != nil {
				return err
			}
			v6, err = allocateOne(ipv6, prefV6, shouldSkip, &uselessIPs)
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
	v4Net := &net.IPNet{IP: v4, Mask: m.cidr.Mask}
	v6Net := &net.IPNet{IP: v6, Mask: m.cidr6.Mask}
	plog.G(ctx).Infof("Rented IP: v4=%s v6=%s", v4Net, v6Net)
	return v4Net, v6Net, nil
}

// ReleaseIP returns the given IPv4 and IPv6 addresses back to the DHCP pool.
func (m *Manager) ReleaseIP(ctx context.Context, v4, v6 net.IP) error {
	plog.G(ctx).Infof("Releasing IP: v4=%v v6=%v", v4, v6)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return m.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) error {
			if err := ipv4.Release(v4); err != nil {
				return err
			}
			if err := ipv6.Release(v6); err != nil {
				return err
			}
			return nil
		})
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
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return m.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) error {
			for _, ip := range ips {
				var err error
				if ip.To4() != nil {
					err = ipv4.Release(ip)
				} else {
					err = ipv6.Release(ip)
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (m *Manager) updateDHCPConfigMap(ctx context.Context, f func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) error) error {
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

	dhcp, err := ipallocator.NewAllocatorCIDRRange(m.cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	if pool.IPv4.Bitmap != "" {
		b, decErr := base64.StdEncoding.DecodeString(pool.IPv4.Bitmap)
		if decErr != nil {
			return decErr
		}
		if err = dhcp.Restore(m.cidr, b); err != nil {
			return err
		}
	}

	dhcp6, err := ipallocator.NewAllocatorCIDRRange(m.cidr6, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	if pool.IPv6.Bitmap != "" {
		b, decErr := base64.StdEncoding.DecodeString(pool.IPv6.Bitmap)
		if decErr != nil {
			return decErr
		}
		if err = dhcp6.Restore(m.cidr6, b); err != nil {
			return err
		}
	}

	if err = f(dhcp, dhcp6); err != nil {
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

	dhcp, err := ipallocator.NewAllocatorCIDRRange(m.cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	if b, decErr := base64.StdEncoding.DecodeString(pool.IPv4.Bitmap); decErr == nil {
		if restoreErr := dhcp.Restore(m.cidr, b); restoreErr != nil {
			return restoreErr
		}
	}
	dhcp.ForEach(fnv4)

	dhcp6, err := ipallocator.NewAllocatorCIDRRange(m.cidr6, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	if b, decErr := base64.StdEncoding.DecodeString(pool.IPv6.Bitmap); decErr == nil {
		if restoreErr := dhcp6.Restore(m.cidr6, b); restoreErr != nil {
			return restoreErr
		}
	}
	dhcp6.ForEach(fnv6)
	return nil
}
