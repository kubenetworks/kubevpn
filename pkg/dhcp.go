package pkg

import (
	"context"
	"fmt"
	"github.com/cilium/ipam/service/allocator"
	"github.com/cilium/ipam/service/ipallocator"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/config"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"net"
)

type DHCPManager struct {
	client    v12.ConfigMapInterface
	cidr      *net.IPNet
	namespace string
}

func NewDHCPManager(client v12.ConfigMapInterface, namespace string, cidr *net.IPNet) *DHCPManager {
	return &DHCPManager{
		client:    client,
		namespace: namespace,
		cidr:      cidr,
	}
}

//	todo optimize dhcp, using mac address, ip and deadline as unit
func (d *DHCPManager) InitDHCP() error {
	configMap, err := d.client.Get(context.Background(), config.PodTrafficManager, metav1.GetOptions{})
	if err == nil && configMap != nil {
		if _, found := configMap.Data[config.Envoy]; !found {
			_, err = d.client.Patch(
				context.Background(),
				configMap.Name,
				types.MergePatchType,
				[]byte(fmt.Sprintf("{\"data\":{\"%s\":\"%s\"}}", config.Envoy, "")),
				metav1.PatchOptions{},
			)
			return err
		}
		return nil
	}
	result := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.PodTrafficManager,
			Namespace: d.namespace,
			Labels:    map[string]string{},
		},
		Data: map[string]string{config.Envoy: ""},
	}
	_, err = d.client.Create(context.Background(), result, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("create dhcp error, err: %v", err)
		return err
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

func (d *DHCPManager) ReleaseIpToDHCP(ip *net.IPNet) error {
	return d.updateDHCPConfigMap(func(r *ipallocator.Range) error { return r.Release(ip.IP) })
}

func (d *DHCPManager) updateDHCPConfigMap(f func(*ipallocator.Range) error) error {
	cm, err := d.client.Get(context.Background(), config.PodTrafficManager, metav1.GetOptions{})
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
	if err = dhcp.Restore(d.cidr, []byte(cm.Data[config.DHCP])); err != nil {
		return err
	}
	if err = f(dhcp); err != nil {
		return err
	}
	_, bytes, err := dhcp.Snapshot()
	if err != nil {
		return err
	}
	cm.Data[config.DHCP] = string(bytes)
	_, err = d.client.Update(context.Background(), cm, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update dhcp failed, err: %v", err)
		return err
	}
	return nil
}
