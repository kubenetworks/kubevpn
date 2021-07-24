package main

import (
	"context"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"kubevpn/exe"
	"kubevpn/remote"
	"net"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	baseCfg   = &baseConfig{}
	namespace string
	clientset *kubernetes.Clientset
	config    *restclient.Config
	name      string
)

func init() {
	var err error
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: filepath.Join(homedir.HomeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName),
		},
		nil,
	)
	config, err = clientConfig.ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	namespace, _, _ = clientConfig.Namespace()

	k8sCIDR, err := getCIDR(clientset, namespace)
	if err != nil {
		log.Fatal(err)
	}
	list := []string{k8sCIDR.String()}

	name = remote.CreateServer(clientset, namespace, "192.168.254.100/24")
	fmt.Println(name)

	err = remote.InitDHCP(clientset, namespace, &net.IPNet{IP: net.IPv4(196, 168, 254, 100), Mask: net.IPv4Mask(255, 255, 255, 0)})
	if err != nil {
		log.Fatal(err)
	}
	dhcp, err := remote.GetIpFromDHCP(clientset, namespace)
	if err != nil {
		log.Fatal(err)
	}
	list = append(list, dhcp.String())

	baseCfg.route.ChainNodes = []string{"ssh://127.0.0.1:2222"}
	baseCfg.route.ServeNodes = []string{
		fmt.Sprintf("tun://:8421/127.0.0.1:8421?net=%s&route=%s", dhcp.String(), strings.Join(list, ",")),
	}
	fmt.Println("your ip is " + dhcp.String())
	baseCfg.Debug = true

	if runtime.GOOS == "windows" {
		exe.InstallTunTapDriver()
	}
}

func main() {
	readyChan := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		err := PortForwardPod(config,
			clientset,
			name,
			namespace,
			"2222:2222",
			readyChan,
			stop,
		)
		if err != nil {
			log.Error(err)
		}
	}()
	<-readyChan
	log.Info("port forward ready")

	if err := start(); err != nil {
		log.Fatal(err)
	}

	select {}
}

func start() error {
	var routerList []router
	rts, err := baseCfg.route.GenRouters()
	if err != nil {
		return err
	}
	routerList = append(routerList, rts...)

	for _, route := range baseCfg.Routes {
		rts, err = route.GenRouters()
		if err != nil {
			return err
		}
		routerList = append(routerList, rts...)
	}

	if len(routerList) == 0 {
		return errors.New("invalid config")
	}
	for i := range routerList {
		go routerList[i].Serve()
	}

	return nil
}

func getCIDR(clientset *kubernetes.Clientset, ns string) (*net.IPNet, error) {
	if nodeList, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, node := range nodeList.Items {
			if _, ip, err := net.ParseCIDR(node.Spec.PodCIDR); err == nil && ip != nil {
				ip.Mask = net.IPv4Mask(255, 255, 0, 0)
				return ip, nil
			}
		}
	}
	if services, err := clientset.CoreV1().Services(ns).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, service := range services.Items {
			if ip := net.ParseIP(service.Spec.ClusterIP); ip != nil {
				return &net.IPNet{IP: ip, Mask: net.IPv4Mask(255, 255, 0, 0)}, nil
			}
		}
	}
	if podList, err := clientset.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, pod := range podList.Items {
			if ip := net.ParseIP(pod.Status.PodIP); ip != nil {
				return &net.IPNet{IP: ip, Mask: net.IPv4Mask(255, 255, 0, 0)}, nil
			}
		}
	}
	return nil, fmt.Errorf("can not found cidr")
}
