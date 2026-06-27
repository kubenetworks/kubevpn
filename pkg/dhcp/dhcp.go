package dhcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"

	"github.com/cilium/ipam/service/allocator"
	"github.com/cilium/ipam/service/ipallocator"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			config.KeyEnvoy:       "",
			config.KeyTunIPPool:   "",
			config.KeyClusterCIDRs: "",
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
	return m.RentIPExcluding(ctx, nil)
}

// RentIPExcluding allocates the next available IPv4 and IPv6 addresses,
// skipping any IP that matches a local interface address or appears in excludeIPs.
func (m *Manager) RentIPExcluding(ctx context.Context, excludeIPs []net.IP) (*net.IPNet, *net.IPNet, error) {
	plog.G(ctx).Debugf("Renting IP from DHCP in namespace %s", m.namespace)
	addrs, _ := net.InterfaceAddrs()
	shouldSkip := func(ip net.IP) bool {
		for _, addr := range addrs {
			if addr == nil {
				continue
			}
			if addrIP, ok := addr.(*net.IPNet); ok {
				if addrIP.IP.Equal(ip) {
					return true
				}
			}
		}
		for _, excluded := range excludeIPs {
			if excluded.Equal(ip) {
				return true
			}
		}
		return false
	}
	var uselessIPs []net.IP
	var v4, v6 net.IP
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		uselessIPs = nil
		return m.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) (err error) {
			for {
				if v4, err = ipv4.AllocateNext(); err != nil {
					return err
				}
				if !shouldSkip(v4) {
					break
				}
				uselessIPs = append(uselessIPs, v4)
			}
			for {
				if v6, err = ipv6.AllocateNext(); err != nil {
					return err
				}
				if !shouldSkip(v6) {
					break
				}
				uselessIPs = append(uselessIPs, v6)
			}
			return
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
	cm.Data[config.KeyTunIPPool] = poolStr
	_, err = m.clientset.CoreV1().ConfigMaps(m.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update TUN_IP_POOL: %w", err)
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

