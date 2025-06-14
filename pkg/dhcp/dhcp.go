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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type Manager struct {
	clientset *kubernetes.Clientset
	cidr      *net.IPNet
	cidr6     *net.IPNet
	namespace string
	clusterID types.UID
}

func NewDHCPManager(clientset *kubernetes.Clientset, namespace string) *Manager {
	return &Manager{
		clientset: clientset,
		namespace: namespace,
		cidr:      &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask},
		cidr6:     &net.IPNet{IP: config.RouterIP6, Mask: config.CIDR6.Mask},
	}
}

// InitDHCP
// TODO optimize dhcp, using mac address, ipPair and deadline as unit
func (m *Manager) InitDHCP(ctx context.Context) error {
	cm, err := m.clientset.CoreV1().ConfigMaps(m.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get configmap %s, err: %v", config.ConfigMapPodTrafficManager, err)
	}

	if err == nil {
		m.clusterID, err = util.GetClusterID(ctx, m.clientset.CoreV1().Namespaces(), m.namespace)
		return err
	}

	cm = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: m.namespace,
			Labels:    map[string]string{},
		},
		Data: map[string]string{
			config.KeyEnvoy:            "",
			config.KeyDHCP:             "",
			config.KeyDHCP6:            "",
			config.KeyClusterIPv4POOLS: "",
		},
	}
	cm, err = m.clientset.CoreV1().ConfigMaps(m.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create configmap: %v", err)
	}
	m.clusterID, err = util.GetClusterID(ctx, m.clientset.CoreV1().Namespaces(), m.namespace)
	return err
}

func (m *Manager) RentIP(ctx context.Context) (*net.IPNet, *net.IPNet, error) {
	addrs, _ := net.InterfaceAddrs()
	var isAlreadyExistedFunc = func(ip net.IP) bool {
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
		return false
	}
	var uselessIPs []net.IP
	var v4, v6 net.IP
	err := m.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) (err error) {
		for {
			if v4, err = ipv4.AllocateNext(); err != nil {
				return err
			}
			if !isAlreadyExistedFunc(v4) {
				break
			}
			uselessIPs = append(uselessIPs, v4)
		}
		for {
			if v6, err = ipv6.AllocateNext(); err != nil {
				return err
			}
			if !isAlreadyExistedFunc(v6) {
				break
			}
			uselessIPs = append(uselessIPs, v6)
		}
		return
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
	return &net.IPNet{IP: v4, Mask: m.cidr.Mask}, &net.IPNet{IP: v6, Mask: m.cidr6.Mask}, nil
}

func (m *Manager) ReleaseIP(ctx context.Context, v4, v6 net.IP) error {
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
		return fmt.Errorf("failed to get configmap DHCP server, err: %v", err)
	}
	if cm.Data == nil {
		return fmt.Errorf("configmap is empty")
	}
	var dhcp *ipallocator.Range
	dhcp, err = ipallocator.NewAllocatorCIDRRange(m.cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}

	if str := cm.Data[config.KeyDHCP]; str != "" {
		var b []byte
		if b, err = base64.StdEncoding.DecodeString(str); err != nil {
			return err
		}
		if err = dhcp.Restore(m.cidr, b); err != nil {
			return err
		}
	}

	var dhcp6 *ipallocator.Range
	dhcp6, err = ipallocator.NewAllocatorCIDRRange(m.cidr6, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	if str := cm.Data[config.KeyDHCP6]; str != "" {
		var b []byte
		if b, err = base64.StdEncoding.DecodeString(str); err != nil {
			return err
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
	cm.Data[config.KeyDHCP] = base64.StdEncoding.EncodeToString(bytes)
	if _, bytes, err = dhcp6.Snapshot(); err != nil {
		return err
	}
	cm.Data[config.KeyDHCP6] = base64.StdEncoding.EncodeToString(bytes)
	_, err = m.clientset.CoreV1().ConfigMaps(m.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update DHCP: %v", err)
	}
	return nil
}

func (m *Manager) ForEach(ctx context.Context, fnv4 func(net.IP), fnv6 func(net.IP)) error {
	cm, err := m.clientset.CoreV1().ConfigMaps(m.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get cm DHCP server, err: %v", err)
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	var dhcp *ipallocator.Range
	dhcp, err = ipallocator.NewAllocatorCIDRRange(m.cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	var str []byte
	str, err = base64.StdEncoding.DecodeString(cm.Data[config.KeyDHCP])
	if err == nil {
		err = dhcp.Restore(m.cidr, str)
		if err != nil {
			return err
		}
	}
	dhcp.ForEach(fnv4)

	var dhcp6 *ipallocator.Range
	dhcp6, err = ipallocator.NewAllocatorCIDRRange(m.cidr6, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	str, err = base64.StdEncoding.DecodeString(cm.Data[config.KeyDHCP6])
	if err == nil {
		err = dhcp6.Restore(m.cidr6, str)
		if err != nil {
			return err
		}
	}
	dhcp6.ForEach(fnv6)
	return nil
}

func (m *Manager) GetClusterID() types.UID {
	return m.clusterID
}
