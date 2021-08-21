package remote

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateServer(t *testing.T) {
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: filepath.Join(homedir.HomeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName),
		},
		nil,
	)
	config, err := clientConfig.ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	i := &net.IPNet{
		IP:   net.ParseIP("192.168.254.100"),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}

	j := &net.IPNet{
		IP:   net.ParseIP("172.20.0.0"),
		Mask: net.IPv4Mask(255, 255, 0, 0),
	}

	server, err := CreateServerOutbound(clientset, "test", i, []*net.IPNet{j})
	fmt.Println(server)
}

func TestGetIp(t *testing.T) {
	ip := &net.IPNet{
		IP:   net.IPv4(192, 168, 254, 100),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}
	fmt.Println(ip.String())
}

func TestGetIPFromDHCP(t *testing.T) {
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: filepath.Join(homedir.HomeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName),
		},
		nil,
	)
	config, err := clientConfig.ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	err = InitDHCP(clientset, "test", nil)
	if err != nil {
		fmt.Println(err)
	}
	for i := 0; i < 10; i++ {
		ipNet, err := GetIpFromDHCP(clientset, "test")
		ipNet2, err := GetIpFromDHCP(clientset, "test")
		if err != nil {
			fmt.Println(err)
			continue
		} else {
			fmt.Printf("%s->%s\n", ipNet.String(), ipNet2.String())
		}
		time.Sleep(time.Millisecond * 10)
		err = ReleaseIpToDHCP(clientset, "test", ipNet)
		err = ReleaseIpToDHCP(clientset, "test", ipNet2)
		if err != nil {
			fmt.Println(err)
		}
		time.Sleep(time.Millisecond * 10)
	}

}
