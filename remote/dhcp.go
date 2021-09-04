package remote

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	net2 "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"kubevpn/util"
	"net"
	"sort"
	"strconv"
	"strings"
)

// todo optimize dhcp, using mac address, ip and deadline as unit
func InitDHCP(client *kubernetes.Clientset, namespace string, addr *net.IPNet) error {
	get, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err == nil && get != nil {
		return nil
	}
	if addr == nil {
		addr = &net.IPNet{IP: net.IPv4(254, 254, 254, 100), Mask: net.IPv4Mask(255, 255, 255, 0)}
	}
	var ips []string
	for i := 2; i < 254; i++ {
		if i != 100 {
			ips = append(ips, strconv.Itoa(i))
		}
	}
	result := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.TrafficManager,
			Namespace: namespace,
			Labels:    map[string]string{},
		},
		Data: map[string]string{"DHCP": strings.Join(ips, ",")},
	}
	_, err = client.CoreV1().ConfigMaps(namespace).Create(context.Background(), result, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("create dhcp error, err: %v", err)
		return err
	}
	return nil
}

func GetIpFromDHCP(client *kubernetes.Clientset, namespace string) (*net.IPNet, error) {
	get, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get ip from dhcp, err: %v", err)
		return nil, err
	}
	split := strings.Split(get.Data["DHCP"], ",")

	ip, left := getIp(split)

	get.Data["DHCP"] = strings.Join(left, ",")
	_, err = client.CoreV1().ConfigMaps(namespace).Update(context.Background(), get, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update dhcp error after get ip, need to put ip back, err: %v", err)
		return nil, err
	}

	return &net.IPNet{
		IP:   net.IPv4(223, 254, 254, byte(ip)),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}, nil
}

func GetRandomIpFromDHCP(client *kubernetes.Clientset, namespace string) (*net.IPNet, error) {
	get, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get ip from dhcp, err: %v", err)
		return nil, err
	}
	split := strings.Split(get.Data["DHCP"], ",")

	ip := split[0]
	split = split[1:]

	get.Data["DHCP"] = strings.Join(split, ",")
	_, err = client.CoreV1().ConfigMaps(namespace).Update(context.Background(), get, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update dhcp error after get ip, need to put ip back, err: %v", err)
		return nil, err
	}

	atoi, _ := strconv.Atoi(ip)
	return &net.IPNet{
		IP:   net.IPv4(223, 254, 254, byte(atoi)),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}, nil
}

func getIp(availableIp []string) (int, []string) {
	var v uint32
	interfaces, _ := net.Interfaces()
	hostInterface, _ := net2.ChooseHostInterface()
out:
	for _, i := range interfaces {
		addrs, _ := i.Addrs()
		for _, addr := range addrs {
			if hostInterface.Equal(addr.(*net.IPNet).IP) {
				hash := md5.New()
				hash.Write([]byte(i.HardwareAddr.String()))
				sum := hash.Sum(nil)
				v = BytesToInt(sum)
				break out
			}
		}
	}
	m := make(map[int]int)
	for _, s := range availableIp {
		atoi, _ := strconv.Atoi(s)
		m[atoi] = atoi
	}
	for {
		if k, ok := m[int(v%256)]; ok {
			delete(m, k)
			return k, getValueFromMap(m)
		} else {
			v++
		}
	}
}

func getValueFromMap(m map[int]int) []string {
	var result []int
	for _, v := range m {
		result = append(result, v)
	}
	sort.Ints(result)
	var s []string
	for _, i := range result {
		s = append(s, strconv.Itoa(i))
	}
	return s
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

func BytesToInt(b []byte) uint32 {
	buffer := bytes.NewBuffer(b)
	var u uint32
	if err := binary.Read(buffer, binary.BigEndian, &u); err != nil {
		log.Warn(err)
	}
	return u
}

func ReleaseIpToDHCP(client *kubernetes.Clientset, namespace string, ip *net.IPNet) error {
	get, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get dhcp, err: %v", err)
		return err
	}
	split := strings.Split(get.Data["DHCP"], ",")
	split = append(split, strings.Split(ip.IP.To4().String(), ".")[3])
	get.Data["DHCP"] = strings.Join(sortString(split), ",")
	_, err = client.CoreV1().ConfigMaps(namespace).Update(context.Background(), get, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("update dhcp error after release ip, need to try again, err: %v", err)
		return err
	}
	return nil
}
