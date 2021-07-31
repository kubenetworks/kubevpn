package main

import (
	"context"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"kubevpn/dns"
	"kubevpn/exe"
	"kubevpn/gost"
	"kubevpn/remote"
	"kubevpn/util"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

var (
	baseCfg    = &baseConfig{}
	namespace  string
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
)

func init() {
	var err error
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &clientcmd.RecommendedHomeFile
	factory := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	if config, err = factory.ToRESTConfig(); err != nil {
		log.Fatal(err)
	}
	if restclient, err = rest.RESTClientFor(config); err != nil {
		log.Fatal(err)
	}
	if clientset, err = kubernetes.NewForConfig(config); err != nil {
		log.Fatal(err)
	}
	if namespace, _, err = factory.ToRawKubeConfigLoader().Namespace(); err != nil {
		log.Fatal(err)
	}
}

func prepare() {
	k8sCIDR, err := getCIDR(clientset, namespace)
	if err != nil {
		log.Fatal(err)
	}
	list := []string{k8sCIDR.String()}

	trafficManager := net.IPNet{
		IP:   net.IPv4(192, 168, 254, 100),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}
	err = remote.CreateServer(clientset, namespace, trafficManager.String())
	if err != nil {
		log.Fatal(err)
	}

	err = remote.InitDHCP(clientset, namespace, &net.IPNet{IP: net.IPv4(196, 168, 254, 100), Mask: net.IPv4Mask(255, 255, 255, 0)})
	if err != nil {
		log.Fatal(err)
	}
	tunIp, err := remote.GetIpFromDHCP(clientset, namespace)
	if err != nil {
		log.Fatal(err)
	}
	remote.AddCleanUpResourceHandler(clientset, namespace, tunIp)
	//if runtime.GOOS == "windows" {
	tunIp.Mask = net.IPv4Mask(0, 0, 0, 0)
	//} else {
	//	dhcp.Mask = net.IPv4Mask(255, 255, 255, 0)
	//}
	list = append(list, tunIp.String())
	//if runtime.GOOS == "windows" {
	ipNet := net.IPNet{
		IP:   net.IPv4(192, 168, 254, 100),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}
	list = append(list, ipNet.String())
	//}

	baseCfg.route.ChainNodes = []string{"ssh://127.0.0.1:2222"}
	baseCfg.route.ServeNodes = []string{
		fmt.Sprintf("tun://:8421/127.0.0.1:8421?net=%s&route=%s", tunIp.String(), strings.Join(list, ",")),
	}
	fmt.Println("your ip is " + tunIp.String())
	gost.Debug = true

	if runtime.GOOS == "windows" {
		exe.InstallTunTapDriver()
	}
}

func main() {
	prepare()

	readyChan := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		err := util.PortForwardPod(config, clientset, remote.TrafficManager, namespace, "2222:2222", readyChan, stop)
		if err != nil {
			log.Error(err)
		}
	}()
	<-readyChan
	log.Info("port forward ready")

	if err := start(); err != nil {
		log.Fatal(err)
	}
	time.Sleep(time.Second * 5)
	dnsServiceIp := dns.GetDNSServiceIpFromPod(clientset, restclient, config, remote.TrafficManager, namespace)
	if runtime.GOOS != "windows" {
		if err := dns.Dns(dnsServiceIp); err != nil {
			log.Fatal(err)
		}
	} else {
		if err := dns.Windows(dnsServiceIp); err != nil {
			log.Fatal(err)
		}
	}
	log.Info("dns service ok")
	_ = exec.Command("ping", "-c", "4", "192.168.254.100").Run()
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
