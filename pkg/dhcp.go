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
	"strconv"
	"strings"
)

const (
	DHCP     = "DHCP"
	SPLITTER = ","
)

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
		Data: map[string]string{DHCP: "100"},
	}
	_, err = d.client.CoreV1().ConfigMaps(d.namespace).Create(context.Background(), result, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("create dhcp error, err: %v", err)
		return err
	}
	return nil
}

func (d *DHCPManager) RentIPBaseNICAddress() (*net.IPNet, error) {
	var ipC = make(chan int, 1)
	err := d.updateDHCPConfigMap(func(i sets.Int) sets.Int {
		ip := getIP(i)
		ipC <- ip
		return i.Insert(ip)
	})
	if err != nil {
		return nil, err
	}
	p := make(net.IP, net.IPv4len)
	copy(p, util.RouterIP.To4())
	p[3] = byte(<-ipC)
	return &net.IPNet{IP: p, Mask: util.CIDR.Mask}, nil
}

func (d *DHCPManager) RentIPRandom() (*net.IPNet, error) {
	var ipC = make(chan int, 1)
	err := d.updateDHCPConfigMap(func(alreadyInUse sets.Int) sets.Int {
		ip := 0
		for i := 1; i < 255; i++ {
			if !alreadyInUse.Has(i) && i != 100 {
				ip = i
				break
			}
		}
		ipC <- ip
		return alreadyInUse.Insert(ip)
	})

	if err != nil {
		log.Errorf("update dhcp error after get ip, need to put ip back, err: %v", err)
		return nil, err
	}
	p := make(net.IP, net.IPv4len)
	copy(p, util.RouterIP.To4())
	p[3] = byte(<-ipC)
	return &net.IPNet{IP: p, Mask: util.CIDR.Mask}, nil
}

func getIP(alreadyInUse sets.Int) int {
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
	for {
		if i := int(v % 255); !alreadyInUse.Has(i) && i != 100 && i != 0 {
			return i
		}
		v++
	}
}

func convertToString(m sets.Int) []string {
	var result []string
	for _, i := range m.List() {
		result = append(result, strconv.Itoa(i))
	}
	return result
}

func (d *DHCPManager) ReleaseIpToDHCP(ip *net.IPNet) error {
	ipN, err := strconv.Atoi(strings.Split(ip.IP.To4().String(), ".")[3])
	if err != nil {
		return err
	}
	return d.updateDHCPConfigMap(func(i sets.Int) sets.Int {
		return i.Delete(ipN)
	})
}

func (d *DHCPManager) updateDHCPConfigMap(f func(sets.Int) sets.Int) error {
	get, err := d.client.CoreV1().ConfigMaps(d.namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get dhcp, err: %v", err)
		return err
	}
	alreadyInUsed := strings.Split(get.Data[DHCP], SPLITTER)
	ipSet := sets.NewInt()
	for _, ip := range alreadyInUsed {
		if i, err := strconv.Atoi(ip); err == nil {
			ipSet.Insert(i)
		}
	}
	get.Data[DHCP] = strings.Join(convertToString(f(ipSet)), SPLITTER)
	_, err = d.client.CoreV1().ConfigMaps(d.namespace).Update(context.Background(), get, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update dhcp error after release ip, need to try again, err: %v", err)
		return err
	}
	return nil
}
