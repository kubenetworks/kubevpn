package handler

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"

	"github.com/cilium/ipam/service/allocator"
	"github.com/cilium/ipam/service/ipallocator"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type DHCPManager struct {
	client    corev1.ConfigMapInterface
	cidr      *net.IPNet
	cidr6     *net.IPNet
	namespace string
}

func NewDHCPManager(client corev1.ConfigMapInterface, namespace string) *DHCPManager {
	return &DHCPManager{
		client:    client,
		namespace: namespace,
		cidr:      &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask},
		cidr6:     &net.IPNet{IP: config.RouterIP6, Mask: config.CIDR6.Mask},
	}
}

// initDHCP
// TODO optimize dhcp, using mac address, ip and deadline as unit
func (d *DHCPManager) initDHCP(ctx context.Context) error {
	cm, err := d.client.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get configmap %s, err: %v", config.ConfigMapPodTrafficManager, err)
	}
	if err == nil {
		// add key envoy in case of mount not exist content
		if _, found := cm.Data[config.KeyEnvoy]; !found {
			_, err = d.client.Patch(
				ctx,
				cm.Name,
				types.MergePatchType,
				[]byte(fmt.Sprintf(`{"data":{"%s":"%s"}}`, config.KeyEnvoy, "")),
				metav1.PatchOptions{},
			)
			return fmt.Errorf("failed to patch configmap %s, err: %v", config.ConfigMapPodTrafficManager, err)
		}
		return nil
	}
	cm = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: d.namespace,
			Labels:    map[string]string{},
		},
		Data: map[string]string{
			config.KeyEnvoy:    "",
			config.KeyRefCount: "0",
		},
	}
	_, err = d.client.Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create dhcp error, err: %v", err)
	}
	return nil
}

func (d *DHCPManager) RentIPBaseNICAddress(ctx context.Context) (*net.IPNet, *net.IPNet, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, nil, err
	}
	var isAlreadyExistedFunc = func(ips ...net.IP) bool {
		for _, addr := range addrs {
			addrIP, ok := addr.(*net.IPNet)
			if ok {
				for _, ip := range ips {
					if addrIP.IP.Equal(ip) {
						return true
					}
				}
			}
		}
		return false
	}
	var v4, v6 net.IP
	err = d.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) (err error) {
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
		return nil, nil, err
	}
	return &net.IPNet{IP: v4, Mask: d.cidr.Mask}, &net.IPNet{IP: v6, Mask: d.cidr6.Mask}, nil
}

func (d *DHCPManager) RentIPRandom(ctx context.Context) (*net.IPNet, *net.IPNet, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, nil, err
	}
	var isAlreadyExistedFunc = func(ips ...net.IP) bool {
		for _, addr := range addrs {
			addrIP, ok := addr.(*net.IPNet)
			if ok {
				for _, ip := range ips {
					if addrIP.IP.Equal(ip) {
						return true
					}
				}
			}
		}
		return false
	}
	var v4, v6 net.IP
	err = d.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) (err error) {
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
		log.Errorf("failed to rent ip from DHCP server, err: %v", err)
		return nil, nil, err
	}
	return &net.IPNet{IP: v4, Mask: d.cidr.Mask}, &net.IPNet{IP: v6, Mask: d.cidr6.Mask}, nil
}

func (d *DHCPManager) ReleaseIP(ctx context.Context, ips ...net.IP) error {
	if len(ips) == 0 {
		return nil
	}
	return d.updateDHCPConfigMap(ctx, func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) error {
		for _, ip := range ips {
			var use *ipallocator.Range
			if ip.To4() != nil {
				use = ipv4
			} else {
				use = ipv6
			}
			if err := use.Release(ip); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *DHCPManager) updateDHCPConfigMap(ctx context.Context, f func(ipv4 *ipallocator.Range, ipv6 *ipallocator.Range) error) error {
	cm, err := d.client.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get cm DHCP server, err: %v", err)
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	var dhcp *ipallocator.Range
	dhcp, err = ipallocator.NewAllocatorCIDRRange(d.cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	var str []byte
	str, err = base64.StdEncoding.DecodeString(cm.Data[config.KeyDHCP])
	if err == nil {
		err = dhcp.Restore(d.cidr, str)
		if err != nil {
			return err
		}
	}

	var dhcp6 *ipallocator.Range
	dhcp6, err = ipallocator.NewAllocatorCIDRRange(d.cidr6, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	str, err = base64.StdEncoding.DecodeString(cm.Data[config.KeyDHCP6])
	if err == nil {
		err = dhcp6.Restore(d.cidr6, str)
		if err != nil {
			return err
		}
	}
	if err = f(dhcp, dhcp6); err != nil {
		return err
	}

	for index, i := range []*ipallocator.Range{dhcp, dhcp6} {
		var bytes []byte
		if _, bytes, err = i.Snapshot(); err != nil {
			return err
		}
		var key string
		if index == 0 {
			key = config.KeyDHCP
		} else {
			key = config.KeyDHCP6
		}
		cm.Data[key] = base64.StdEncoding.EncodeToString(bytes)
	}
	_, err = d.client.Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update dhcp failed, err: %v", err)
	}
	return nil
}

func (d *DHCPManager) Set(key, value string) error {
	cm, err := d.client.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get data, err: %v", err)
		return err
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[key] = value
	_, err = d.client.Update(context.Background(), cm, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update data failed, err: %v", err)
		return err
	}
	return nil
}

func (d *DHCPManager) Get(ctx2 context.Context, key string) (string, error) {
	cm, err := d.client.Get(ctx2, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
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

func (d *DHCPManager) ForEach(fn func(net.IP)) error {
	cm, err := d.client.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get cm DHCP server, err: %v", err)
		return err
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	dhcp, err := ipallocator.NewAllocatorCIDRRange(d.cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return err
	}
	str, err := base64.StdEncoding.DecodeString(cm.Data[config.KeyDHCP])
	if err != nil {
		return err
	}
	err = dhcp.Restore(d.cidr, str)
	if err != nil {
		return err
	}
	dhcp.ForEach(fn)
	return nil
}
