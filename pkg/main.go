package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	log "github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"sync"
	"time"
)

var (
	baseCfg        route
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
	flag.BoolVar(&gost.Debug, "debug", false, "true/false")
	flag.Parse()
	log.Printf("kubeconfig path: %s, namespace: %s, serivces: %s\n", kubeconfigpath, namespace, services)
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
		IP:   net.IPv4(223, 254, 254, 100),
		Mask: net.CIDRMask(24, 32),
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
	wg := sync.WaitGroup{}
	lock := sync.Mutex{}
	for _, service := range strings.Split(services, ",") {
		if len(service) > 0 {
			wg.Add(1)
			go func(finalService string) {
				defer wg.Done()
				lock.Lock()
				virtualShadowIp, _ := remote.GetRandomIpFromDHCP(clientset, namespace)
				tempIps = append(tempIps, virtualShadowIp)
				lock.Unlock()
				err = remote.CreateServerInbound(
					clientset,
					namespace,
					finalService,
					tunIp.IP.String(),
					pod.Status.PodIP,
					virtualShadowIp.String(),
					strings.Join(list, ","),
				)
				if err != nil {
					log.Error(err)
				}
			}(service)
		}
	}
	wg.Wait()
	remote.AddCleanUpResourceHandler(clientset, namespace, services, tempIps...)
	if util.IsWindows() {
		tunIp.Mask = net.CIDRMask(0, 32)
	} else {
		tunIp.Mask = net.CIDRMask(24, 32)
	}

	list = append(list, trafficManager.String())

	baseCfg.ChainNodes = "socks5://127.0.0.1:10800?notls=true"
	baseCfg.ServeNodes = fmt.Sprintf("tun://:8421/127.0.0.1:8421?net=%s&route=%s", tunIp.String(), strings.Join(list, ","))

	log.Info("your ip is " + tunIp.String())

	if util.IsWindows() {
		exe.InstallTunTapDriver()
	}
}

func main() {
	prepare()
	//readyChan := make(chan struct{}, math.MaxInt32)
	go func() {
		for {
			func() {
				defer func() {
					if err := recover(); err != nil {
						log.Warnf("recover error: %v, ignore", err)
					}
				}()
				err := util.PortForwardPod(
					config,
					clientset,
					util.TrafficManager,
					namespace,
					"10800:10800",
					make(chan struct{}),
					make(chan struct{}),
				)
				if apierrors.IsNotFound(err) {
					log.Fatalf("can not found port-forward resource, err: %v, exiting", err)
				}
				if err != nil {
					log.Errorf("port-forward occurs error, err: %v, retrying", err)
					time.Sleep(time.Second * 2)
				}
			}()
		}
	}()
	//<-readyChan
	time.Sleep(time.Second * 4)
	log.Info("port forward ready")

	if err := start(); err != nil {
		log.Fatal(err)
	}

	if runtime.GOOS == "windows" {
		if !util.FindRule() {
			util.AddFirewallRule()
		}
		util.DeleteWindowsFirewallRule()
	}
	log.Info("dns service ok")
	_ = exec.Command("ping", "-c", "4", "223.254.254.100").Run()

	//time.Sleep(time.Second * 5)
	dnsServiceIp := dns.GetDNSServiceIpFromPod(clientset, restclient, config, util.TrafficManager, namespace)
	if err := dns.DNS(dnsServiceIp, namespace); err != nil {
		log.Fatal(err)
	}
	select {}
}

func start() error {
	routers, err := baseCfg.GenRouters()
	if err != nil {
		return err
	}

	if routers == nil {
		return errors.New("invalid config")
	}

	go func() {
		if err = routers.Serve(); err != nil {
			log.Warn(err)
		}
	}()

	return nil
}

func getCIDR(clientset *kubernetes.Clientset, ns string) (result []*net.IPNet, err error) {
	result = make([]*net.IPNet, 0)
	var cidrs []*net.IPNet
	if nodeList, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, node := range nodeList.Items {
			if _, ip, err := net.ParseCIDR(node.Spec.PodCIDR); err == nil && ip != nil {
				ip.Mask = net.CIDRMask(16, 32)
				ip.IP = ip.IP.Mask(ip.Mask)
				cidrs = append(cidrs, ip)
				err = nil
			}
		}
	}
	if serviceList, err := clientset.CoreV1().Services(ns).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, service := range serviceList.Items {
			if ip := net.ParseIP(service.Spec.ClusterIP); ip != nil {
				mask := net.CIDRMask(16, 32)
				cidrs = append(cidrs, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
			}
		}
	}
	if podList, err := clientset.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, pod := range podList.Items {
			if ip := net.ParseIP(pod.Status.PodIP); ip != nil {
				mask := net.CIDRMask(16, 32)
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

func RenameNic() {
	interfaces, _ := net.Interfaces()
	for _, i := range interfaces {
		if strings.Contains(i.Name, " ") {
			cmd := exec.Command("netsh", []string{
				"interface",
				"set",
				"interface",
				fmt.Sprintf("name='%s'", i.Name),
				"newname=" + strings.ReplaceAll(i.Name, " ", ""),
			}...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				log.Warnf("rename %s --> %s failed, out: %s, error: %v, command: %s",
					i.Name, strings.ReplaceAll(i.Name, " ", ""), string(out), err, cmd.Args)
			}
		}
	}
}
