package main

import (
	"context"
	"crypto/md5"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	net2 "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"kubevpn/remote"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
)

var (
	clientConfig = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: filepath.Join(homedir.HomeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName),
		},
		nil,
	)
	clientconfig, _  = clientConfig.ClientConfig()
	clientsets, _    = kubernetes.NewForConfig(clientconfig)
	namespaces, _, _ = clientConfig.Namespace()
)

func TestCidr(t *testing.T) {
	cidr, err := getCIDR(clientsets, namespaces)
	if err == nil {
		fmt.Println(cidr.String())
	}
}

func TestPing(t *testing.T) {
	list, _ := clientsets.CoreV1().Services(namespaces).List(context.Background(), metav1.ListOptions{})
	for _, service := range list.Items {
		for _, clusterIP := range service.Spec.ClusterIPs {
			_ = exec.Command("ping", clusterIP, "-c", "4").Run()
		}
	}
}

func TestMac(t *testing.T) {
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
