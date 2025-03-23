package dhcp

import (
	"context"
	"encoding/base64"
	"fmt"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"net"

	"github.com/cilium/ipam/service/allocator"
	"github.com/cilium/ipam/service/ipallocator"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type Manager struct {
	client    corev1.ConfigMapInterface
	cidr      *net.IPNet
	cidr6     *net.IPNet
	namespace string
	clusterID types.UID
}

func NewDHCPManager(client corev1.ConfigMapInterface, namespace string) *Manager {
	return &Manager{
		client:    client,
		namespace: namespace,
		cidr:      &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask},
		cidr6:     &net.IPNet{IP: config.RouterIP6, Mask: config.CIDR6.Mask},
	}
}

// InitDHCP
// TODO optimize dhcp, using mac address, ip and deadline as unit
func (m *Manager) InitDHCP(ctx context.Context) error {
	cm, err := m.client.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get configmap %s, err: %v", config.ConfigMapPodTrafficManager, err)
	}

	if err == nil {
		m.clusterID = util.GetClusterIDByCM(cm)
		return nil
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
	cm, err = m.client.Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create DHCP error, err: %v", err)
	}
	m.clusterID = util.GetClusterIDByCM(cm)
	return nil
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
	var v4, v6 net.IP
	err := m.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) (err error) {
		for {
			if v4, err = ipv4.AllocateNext(); err != nil {
				return err
			}
			if !isAlreadyExistedFunc(v4) {
				break
			}
		}
		for {
			if v6, err = ipv6.AllocateNext(); err != nil {
				return err
			}
			if !isAlreadyExistedFunc(v6) {
				break
			}
		}
		return
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to rent IP from DHCP server, err: %v", err)
		return nil, nil, err
	}
	return &net.IPNet{IP: v4, Mask: m.cidr.Mask}, &net.IPNet{IP: v6, Mask: m.cidr6.Mask}, nil
}

func (m *Manager) ReleaseIP(ctx context.Context, ips ...net.IP) error {
	if len(ips) == 0 {
		return nil
	}
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
}

func (m *Manager) updateDHCPConfigMap(ctx context.Context, f func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) error) error {
	cm, err := m.client.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
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
	_, err = m.client.Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update DHCP failed, err: %v", err)
	}
	return nil
}

func (m *Manager) Set(ctx context.Context, key, value string) error {
	err := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			p := []byte(fmt.Sprintf(`[{"op": "replace", "path": "/data/%s", "value": "%s"}]`, key, value))
			_, err := m.client.Patch(ctx, config.ConfigMapPodTrafficManager, types.JSONPatchType, p, metav1.PatchOptions{})
			return err
		})
	if err != nil {
		plog.G(ctx).Errorf("Failed to update configmap: %v", err)
		return err
	}
	return nil
}

func (m *Manager) Get(ctx context.Context, key string) (string, error) {
	cm, err := m.client.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if cm != nil && cm.Data != nil {
		if v, ok := cm.Data[key]; ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("can not get data")
}

func (m *Manager) ForEach(ctx context.Context, fnv4 func(net.IP), fnv6 func(net.IP)) error {
	cm, err := m.client.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
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
