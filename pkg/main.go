package main

import (
	"context"
	"errors"
	"flag"
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
)

var (
	baseCfg        = &baseConfig{}
	kubeconfigpath string
	namespace      string
	services       string
	clientset      *kubernetes.Clientset
	restclient     *rest.RESTClient
	config         *rest.Config
)

func init() {
	gost.SetLogger(&gost.LogLogger{})
	flag.StringVar(&kubeconfigpath, "kubeconfig", clientcmd.RecommendedHomeFile, "kubeconfig")
	flag.StringVar(&namespace, "namespace", "", "namespace")
	flag.StringVar(&services, "services", "", "services")
	flag.Parse()
	fmt.Printf("kubeconfig path: %s, namespace: %s, serivces: %s\n", kubeconfigpath, namespace, services)
	var err error
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &kubeconfigpath
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
	if namespace == "" {
		if namespace, _, err = factory.ToRawKubeConfigLoader().Namespace(); err != nil {
			log.Fatal(err)
		}
	}

}

func prepare() {
	k8sCIDRs, err := getCIDR(clientset, namespace)
	if err != nil {
		log.Fatal(err)
	}
	var list []string
	for _, ipNet := range k8sCIDRs {
		list = append(list, ipNet.String())
	}

	trafficManager := net.IPNet{
		IP:   net.IPv4(192, 192, 254, 100),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}

	err = remote.InitDHCP(clientset, namespace, &trafficManager)
	if err != nil {
		log.Fatal(err)
	}
	tunIp, err := remote.GetIpFromDHCP(clientset, namespace)
	if err != nil {
		log.Fatal(err)
	}
	pod, err := remote.CreateServerOutbound(clientset, namespace, &trafficManager, k8sCIDRs)
	if err != nil {
		log.Fatal(err)
	}
	tempIps := []*net.IPNet{tunIp}
	for _, service := range strings.Split(services, ",") {
		virtualShadowIp, _ := remote.GetRandomIpFromDHCP(clientset, namespace)
		tempIps = append(tempIps, virtualShadowIp)
		err = remote.CreateServerOutboundAndInbound(clientset, namespace, service, tunIp.IP.String(), pod.Status.PodIP, virtualShadowIp.String())
		if err != nil {
			log.Error(err)
		}
	}
	remote.AddCleanUpResourceHandler(clientset, namespace, services, tempIps...)
	//if runtime.GOOS == "windows" {
	tunIp.Mask = net.IPv4Mask(0, 0, 0, 0)
	//} else {
	//	dhcp.Mask = net.IPv4Mask(255, 255, 255, 0)
	//}
	//list = append(list, tunIp.String())
	//if runtime.GOOS == "windows" {
	ipNet := net.IPNet{
		IP:   net.IPv4(192, 192, 254, 100),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}
	list = append(list, ipNet.String())
	//list = append(list, "192.192.254.162/32")
	//}

	baseCfg.route.ChainNodes = []string{"socks5://127.0.0.1:10800?notls=true"}
	baseCfg.route.ServeNodes = []string{
		fmt.Sprintf("tun://:8421/127.0.0.1:8421?net=%s&route=%s", tunIp.String(), strings.Join(list, ",")),
	}
	fmt.Println("your ip is " + tunIp.String())
	//fmt.Println(baseCfg.route.ServeNodes)
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
		err := util.PortForwardPod(config, clientset, util.TrafficManager, namespace, "10800:10800", readyChan, stop)
		if err != nil {
			log.Error(err)
		}
	}()
	<-readyChan
	log.Info("port forward ready")

	if err := start(); err != nil {
		log.Fatal(err)
	}
	//time.Sleep(time.Second * 5)
	dnsServiceIp := util.GetDNSServiceIpFromPod(clientset, restclient, config, util.TrafficManager, namespace)
	if err := dns.DNS(dnsServiceIp); err != nil {
		log.Fatal(err)
	}
	log.Info("dns service ok")
	_ = exec.Command("ping", "-c", "4", "192.192.254.100").Run()
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

func getCIDR(clientset *kubernetes.Clientset, ns string) (result []*net.IPNet, err error) {
	result = make([]*net.IPNet, 0)
	var cidrs []*net.IPNet
	if nodeList, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, node := range nodeList.Items {
			if _, ip, err := net.ParseCIDR(node.Spec.PodCIDR); err == nil && ip != nil {
				ip.Mask = net.CIDRMask(24, 32)
				ip.IP = ip.IP.Mask(ip.Mask)
				cidrs = append(cidrs, ip)
				err = nil
			}
		}
	}
	if services, err := clientset.CoreV1().Services(ns).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, service := range services.Items {
			if ip := net.ParseIP(service.Spec.ClusterIP); ip != nil {
				mask := net.CIDRMask(24, 32)
				cidrs = append(cidrs, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
			}
		}
	}
	if podList, err := clientset.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, pod := range podList.Items {
			if ip := net.ParseIP(pod.Status.PodIP); ip != nil {
				mask := net.CIDRMask(24, 32)
				cidrs = append(cidrs, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
			}
		}
	}
	tempMap := make(map[string]*net.IPNet)
	for _, cidr := range cidrs {
		if _, found := tempMap[cidr.String()]; !found {
			tempMap[cidr.String()] = cidr
			result = append(result, cidr)
		}
	}
	if len(result) != 0 {
		return
	}
	return nil, fmt.Errorf("can not found cidr")
}
