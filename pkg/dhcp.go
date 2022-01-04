package pkg

import (
	"context"
	"crypto/md5"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"net"
	"sort"
	"strconv"
	"strings"
)

type DHCP interface {
	RentIP() net.IPNet
	ReleaseIP(ip net.IPNet)
}

type DHCPManager struct {
	client    *kubernetes.Clientset
	namespace string
	cidr      *net.IPNet
}

func NewDHCPManager(client *kubernetes.Clientset, namespace string, addr *net.IPNet) *DHCPManager {
	return &DHCPManager{
		client:    client,
		namespace: namespace,
		cidr:      addr,
	}
}

//	todo optimize dhcp, using mac address, ip and deadline as unit
func (d *DHCPManager) InitDHCP() error {
	configMap, err := d.client.CoreV1().ConfigMaps(d.namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err == nil && configMap != nil {
		return nil
	}
	if d.cidr == nil {
		d.cidr = &net.IPNet{IP: net.IPv4(254, 254, 254, 100), Mask: net.IPv4Mask(255, 255, 255, 0)}
	}
	result := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.TrafficManager,
			Namespace: d.namespace,
			Labels:    map[string]string{},
		},
		Data: map[string]string{"DHCP": "100"},
	}
	_, err = d.client.CoreV1().ConfigMaps(d.namespace).Create(context.Background(), result, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("create dhcp error, err: %v", err)
		return err
	}
	return nil
}

func (d *DHCPManager) RentIPBaseNICAddress() (*net.IPNet, error) {
	get, err := d.client.CoreV1().ConfigMaps(d.namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get ip from dhcp, err: %v", err)
		return nil, err
	}
	split := strings.Split(get.Data["DHCP"], ",")

	ip, added := getIP(split)

	get.Data["DHCP"] = strings.Join(added, ",")
	_, err = d.client.CoreV1().ConfigMaps(d.namespace).Update(context.Background(), get, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update dhcp error after get ip, need to put ip back, err: %v", err)
		return nil, err
	}

	return &net.IPNet{IP: net.IPv4(223, 254, 254, byte(ip)), Mask: net.CIDRMask(24, 32)}, nil
}

func (d *DHCPManager) RentIPRandom() (*net.IPNet, error) {
	get, err := d.client.CoreV1().ConfigMaps(d.namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get ip from dhcp, err: %v", err)
		return nil, err
	}
	alreadyInUse := strings.Split(get.Data["DHCP"], ",")
	m := sets.NewInt()
	for _, ip := range alreadyInUse {
		if atoi, err := strconv.Atoi(ip); err == nil {
			m.Insert(atoi)
		}
	}
	ip := 0
	for i := 1; i < 255; i++ {
		if !m.Has(i) && i != 100 {
			ip = i
			break
		}
	}

	get.Data["DHCP"] = strings.Join(append(alreadyInUse, strconv.Itoa(ip)), ",")
	_, err = d.client.CoreV1().ConfigMaps(d.namespace).Update(context.Background(), get, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update dhcp error after get ip, need to put ip back, err: %v", err)
		return nil, err
	}

	return &net.IPNet{IP: net.IPv4(223, 254, 254, byte(ip)), Mask: net.CIDRMask(24, 32)}, nil
}

func getIP(alreadyInUse []string) (int, []string) {
	var v uint32
	interfaces, _ := net.Interfaces()
	for _, i := range interfaces {
		if i.HardwareAddr != nil {
			hash := md5.New()
			hash.Write([]byte(i.HardwareAddr.String()))
			sum := hash.Sum(nil)
			v = util.BytesToInt(sum)
		}
	}
	m := sets.NewInt()
	for _, ip := range alreadyInUse {
		if atoi, err := strconv.Atoi(ip); err == nil {
			m.Insert(atoi)
		}
	}
	for {
		if i := int(v % 255); !m.Has(i) && i != 100 && i != 0 {
			return i, append(alreadyInUse, strconv.Itoa(i))
		}
		v++
	}
}

func sortString(m []string) []string {
	var result []int
	for _, v := range m {
		if len(v) > 0 {
			if atoi, err := strconv.Atoi(v); err == nil {
				result = append(result, atoi)
			}
		}
	}
	sort.Ints(result)
	var s []string
	for _, i := range result {
		s = append(s, strconv.Itoa(i))
	}
	return s
}

func (d *DHCPManager) ReleaseIpToDHCP(ip *net.IPNet) error {
	get, err := d.client.CoreV1().ConfigMaps(d.namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get dhcp, err: %v", err)
		return err
	}
	split := strings.Split(get.Data["DHCP"], ",")
	split = append(split, strings.Split(ip.IP.To4().String(), ".")[3])
	get.Data["DHCP"] = strings.Join(sortString(split), ",")
	_, err = d.client.CoreV1().ConfigMaps(d.namespace).Update(context.Background(), get, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update dhcp error after release ip, need to try again, err: %v", err)
		return err
	}
	return nil
}
