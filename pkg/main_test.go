package pkg

import (
	"context"
	"crypto/md5"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/remote"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	net2 "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"net"
	"net/http"
	"os/exec"
	"testing"
	"time"
)

var (
	clientConfig = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: clientcmd.RecommendedHomeFile}, nil,
	)
	clientconfig, _  = clientConfig.ClientConfig()
	clientsets, _    = kubernetes.NewForConfig(clientconfig)
	namespaces, _, _ = clientConfig.Namespace()
)

func TestGetCIDR(t *testing.T) {
	cidr, err := getCIDR(clientsets, namespaces)
	if err == nil {
		for _, ipNet := range cidr {
			fmt.Println(ipNet)
		}
	}
}

func TestPingUsingCommand(t *testing.T) {
	list, _ := clientsets.CoreV1().Services(namespaces).List(context.Background(), metav1.ListOptions{})
	for _, service := range list.Items {
		for _, clusterIP := range service.Spec.ClusterIPs {
			_ = exec.Command("ping", clusterIP, "-c", "4").Run()
		}
	}
}

func TestGetMacAddress(t *testing.T) {
	interfaces, _ := net.Interfaces()
	hostInterface, _ := net2.ChooseHostInterface()
	for _, i := range interfaces {
		//fmt.Printf("%s -> %s\n", i.Name, i.HardwareAddr.String())
		addrs, _ := i.Addrs()
		for _, addr := range addrs {
			if hostInterface.Equal(addr.(*net.IPNet).IP) {
				hash := md5.New()
				hash.Write([]byte(i.HardwareAddr.String()))
				sum := hash.Sum(nil)
				toInt := remote.BytesToInt(sum)
				fmt.Println(toInt)
			}
		}
	}
}

func TestPingUsingCode(t *testing.T) {
	conn, err := net.DialTimeout("ip4:icmp", "www.baidu.com", time.Second*5)
	if err != nil {
		log.Print(err)
		return
	}
	var msg [512]byte
	msg[0] = 8
	msg[1] = 0
	msg[2] = 0
	msg[3] = 0
	msg[4] = 0
	msg[5] = 13
	msg[6] = 0
	msg[7] = 37

	length := 8
	check := checkSum(msg[0:length])
	msg[2] = byte(check >> 8)
	msg[3] = byte(check & 255)
	_, err = conn.Write(msg[0:length])
	if err != nil {
		log.Print(err)
		return
	}
	conn.Read(msg[0:])
	log.Println(msg[5] == 13)
	log.Println(msg[7] == 37)
}

func checkSum(msg []byte) uint16 {
	sum := 0
	for n := 1; n < len(msg)-1; n += 2 {
		sum += int(msg[n])*256 + int(msg[n+1])
	}
	sum = (sum >> 16) + (sum & 0xffff)
	sum += sum >> 16
	return uint16(^sum)
}

func TestHttpServer(t *testing.T) {
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "hello world!")
	})
	_ = http.ListenAndServe(":8080", nil)
}
