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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

type DHCPManager struct {
	client    corev1.ConfigMapInterface
	cidr      *net.IPNet
	namespace string
}

func NewDHCPManager(client corev1.ConfigMapInterface, namespace string, cidr *net.IPNet) *DHCPManager {
	return &DHCPManager{
		client:    client,
		namespace: namespace,
		cidr:      cidr,
	}
}

// todo optimize dhcp, using mac address, ip and deadline as unit
func (d *DHCPManager) InitDHCP() error {
	cm, err := d.client.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err == nil {
		// add key envoy in case of mount not exist content
		if _, found := cm.Data[config.KeyEnvoy]; !found {
			_, err = d.client.Patch(
				context.Background(),
				cm.Name,
				types.MergePatchType,
				[]byte(fmt.Sprintf(`{"data":{"%s":"%s"}}`, config.KeyEnvoy, "")),
				metav1.PatchOptions{},
			)
			return err
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
	_, err = d.client.Create(context.Background(), cm, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create dhcp error, err: %v", err)
	}
	return nil
}

func (d *DHCPManager) RentIPBaseNICAddress() (*net.IPNet, error) {
	ips := make(chan net.IP, 1)
	err := d.updateDHCPConfigMap(func(allocator *ipallocator.Range) error {
		ip, err := allocator.AllocateNext()
		if err != nil {
			return err
		}
		ips <- ip
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &net.IPNet{IP: <-ips, Mask: d.cidr.Mask}, nil
}

func (d *DHCPManager) RentIPRandom() (*net.IPNet, error) {
	var ipC = make(chan net.IP, 1)
	err := d.updateDHCPConfigMap(func(dhcp *ipallocator.Range) error {
		ip, err := dhcp.AllocateNext()
		if err != nil {
			return err
		}
		ipC <- ip
		return nil
	})
	if err != nil {
		log.Errorf("update dhcp error after get ip, need to put ip back, err: %v", err)
		return nil, err
	}
	return &net.IPNet{IP: <-ipC, Mask: d.cidr.Mask}, nil
}

func (d *DHCPManager) ReleaseIpToDHCP(ips ...*net.IPNet) error {
	return d.updateDHCPConfigMap(func(r *ipallocator.Range) error {
		for _, ip := range ips {
			if err := r.Release(ip.IP); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *DHCPManager) updateDHCPConfigMap(f func(*ipallocator.Range) error) error {
	cm, err := d.client.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get dhcp, err: %v", err)
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
	if err == nil {
		err = dhcp.Restore(d.cidr, str)
		if err != nil {
			return err
		}
	}
	if err = f(dhcp); err != nil {
		return err
	}
	_, bytes, err := dhcp.Snapshot()
	if err != nil {
		return err
	}
	cm.Data[config.KeyDHCP] = base64.StdEncoding.EncodeToString(bytes)
	_, err = d.client.Update(context.Background(), cm, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update dhcp failed, err: %v", err)
		return err
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

func (d *DHCPManager) Get(key string) (string, error) {
	cm, err := d.client.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
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
