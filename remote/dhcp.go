package remote

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	net2 "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"kubevpn/util"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

var stopChan = make(chan os.Signal)

func AddCleanUpResourceHandler(client *kubernetes.Clientset, namespace string, services string, ip ...*net.IPNet) {
	signal.Notify(stopChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL /*, syscall.SIGSTOP*/)
	go func() {
		<-stopChan
		log.Info("prepare to exit, cleaning up")
		//cleanUpTrafficManagerIfRefCountIsZero(client, namespace)
		for _, ipNet := range ip {
			err := ReleaseIpToDHCP(client, namespace, ipNet)
			if err != nil {
				log.Errorf("failed to release ip to dhcp, err: %v", err)
			}
		}
		for _, s := range strings.Split(services, ",") {
			util.ScaleDeploymentReplicasTo(client, namespace, s, 1)
			newName := s + "-" + "shadow"
			deletePod(client, namespace, newName)
		}
		log.Info("clean up successful")
		os.Exit(0)
	}()
}

func deletePod(client *kubernetes.Clientset, namespace, podName string) {
	zero := int64(0)
	err := client.CoreV1().Pods(namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
	})
	if err != nil && errors.IsNotFound(err) {
		log.Infof("not found shadow pod: %s, no need to delete it", podName)
	}
}

// vendor/k8s.io/kubectl/pkg/polymorphichelpers/rollback.go:99
func updateRefCount(client *kubernetes.Clientset, namespace, name string, increment int) {
	err := retry.OnError(
		retry.DefaultRetry,
		func(err error) bool { return err != nil },
		func() error {
			configMap, err := client.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
			if err != nil {
				log.Errorf("update ref-count failed, increment: %d, error: %v", increment, err)
				return err
			}
			curCount, err := strconv.Atoi(configMap.GetAnnotations()["ref-count"])
			if err != nil {
				curCount = 0
			}

			patch, _ := json.Marshal([]interface{}{
				map[string]interface{}{
					"op":    "replace",
					"path":  "/metadata/annotations/" + "ref-count",
					"value": strconv.Itoa(curCount + increment),
				},
			})
			_, err = client.CoreV1().ConfigMaps(namespace).
				Patch(context.TODO(), util.TrafficManager, types.JSONPatchType, patch, metav1.PatchOptions{})
			return err
		},
	)
	if err != nil {
		log.Errorf("update ref count error, error: %v", err)
	} else {
		log.Info("update ref count successfully")
	}
}

func cleanUpTrafficManagerIfRefCountIsZero(client *kubernetes.Clientset, namespace string) {
	updateRefCount(client, namespace, util.TrafficManager, -1)
	configMap, err := client.CoreV1().ConfigMaps(namespace).Get(context.TODO(), util.TrafficManager, metav1.GetOptions{})
	if err != nil {
		log.Error(err)
		return
	}
	refCount, err := strconv.Atoi(configMap.GetAnnotations()["ref-count"])
	if err != nil {
		log.Error(err)
		return
	}
	// if refcount is less than zero or equals to zero, means no body will using this dns pod, so clean it
	if refCount <= 0 {
		log.Info("refCount is zero, prepare to clean up resource")
		_ = client.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), util.TrafficManager, metav1.DeleteOptions{})
		_ = client.CoreV1().Pods(namespace).Delete(context.TODO(), util.TrafficManager, metav1.DeleteOptions{})
	}
}

func InitDHCP(client *kubernetes.Clientset, namespace string, addr *net.IPNet) error {
	get, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), util.TrafficManager, metav1.GetOptions{})
	if err == nil && get != nil {
		return nil
	}
	if addr == nil {
		addr = &net.IPNet{IP: net.IPv4(192, 192, 254, 100), Mask: net.IPv4Mask(255, 255, 255, 0)}
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
		IP:   net.IPv4(192, 192, 254, byte(ip)),
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
		IP:   net.IPv4(192, 192, 254, byte(atoi)),
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
		atoi, _ := strconv.Atoi(v)
		result = append(result, atoi)
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
	binary.Read(buffer, binary.BigEndian, &u)
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
