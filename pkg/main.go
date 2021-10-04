package pkg

import (
	"context"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/dns"
	"github.com/wencaiwulue/kubevpn/driver"
	"github.com/wencaiwulue/kubevpn/remote"
	"github.com/wencaiwulue/kubevpn/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

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

	if err = remote.InitDHCP(clientset, namespace, &trafficManager); err != nil {
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
	for _, workload := range workloads {
		if len(workload) > 0 {
			wg.Add(1)
			go func(finalWorkload string) {
				defer wg.Done()
				lock.Lock()
				virtualShadowIp, _ := remote.GetRandomIpFromDHCP(clientset, namespace)
				tempIps = append(tempIps, virtualShadowIp)
				lock.Unlock()
				err = remote.CreateServerInbound(
					factory,
					clientset,
					namespace,
					finalWorkload,
					tunIp.IP.String(),
					pod.Status.PodIP,
					virtualShadowIp.String(),
					strings.Join(list, ","),
				)
				if err != nil {
					log.Error(err)
				}
			}(workload)
		}
	}
	wg.Wait()
	remote.AddCleanUpResourceHandler(clientset, namespace, workloads, tempIps...)
	if util.IsWindows() {
		tunIp.Mask = net.CIDRMask(0, 32)
	} else {
		tunIp.Mask = net.CIDRMask(24, 32)
	}

	list = append(list, trafficManager.String())

	nodeConfig.ChainNodes = "socks5://127.0.0.1:10800?notls=true"
	nodeConfig.ServeNodes = []string{fmt.Sprintf("tun://:8421/127.0.0.1:8421?net=%s&route=%s", tunIp.String(), strings.Join(list, ","))}

	log.Info("your ip is " + tunIp.String())

	if util.IsWindows() {
		driver.InstallWireGuardTunDriver()
	}
}

func Main() {
	prepare()
	var readyChanRef *chan struct{}
	ctx, cancelFunc := context.WithCancel(context.Background())
	remote.CancelFunctions = append(remote.CancelFunctions, cancelFunc)
	go func() {
		for ctx.Err() == nil {
			func() {
				defer func() {
					if err := recover(); err != nil {
						log.Warnf("recover error: %v, ignore", err)
					}
				}()
				readChan := make(chan struct{})
				readyChanRef = &readChan
				err := util.PortForwardPod(
					config,
					restclient,
					util.TrafficManager,
					namespace,
					"10800:10800",
					readChan,
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
	for readyChanRef == nil {
	}
	<-*readyChanRef
	log.Info("port forward ready")

	if err := start(); err != nil {
		log.Fatal(err)
	}

	if util.IsWindows() {
		if !util.FindRule() {
			util.AddFirewallRule()
		}
		util.DeleteWindowsFirewallRule()
	}
	log.Info("dns service ok")
	_ = exec.Command("ping", "-c", "4", "223.254.254.100").Run()

	dnsServiceIp := dns.GetDNSServiceIpFromPod(clientset, restclient, config, util.TrafficManager, namespace)
	if err := dns.DNS(dnsServiceIp, namespace); err != nil {
		log.Fatal(err)
	}
}

func start() error {
	routers, err := nodeConfig.GenRouters()
	if err != nil {
		return err
	}

	if len(routers) == 0 {
		return errors.New("invalid config")
	}

	for i := range routers {
		ctx, cancelFunc := context.WithCancel(context.Background())
		remote.CancelFunctions = append(remote.CancelFunctions, cancelFunc)
		go func(finalCtx context.Context, finalI int) {
			if err = routers[finalI].Serve(finalCtx); err != nil {
				log.Warn(err)
			}
		}(ctx, i)
	}

	return nil
}

func getCIDR(clientset *kubernetes.Clientset, namespace string) ([]*net.IPNet, error) {
	var cidrs []*net.IPNet
	if nodeList, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, node := range nodeList.Items {
			if _, ip, err := net.ParseCIDR(node.Spec.PodCIDR); err == nil && ip != nil {
				ip.Mask = net.CIDRMask(16, 32)
				ip.IP = ip.IP.Mask(ip.Mask)
				cidrs = append(cidrs, ip)
			}
		}
	}
	if serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, service := range serviceList.Items {
			if ip := net.ParseIP(service.Spec.ClusterIP); ip != nil {
				mask := net.CIDRMask(16, 32)
				cidrs = append(cidrs, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
			}
		}
	}
	if podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, pod := range podList.Items {
			if ip := net.ParseIP(pod.Status.PodIP); ip != nil {
				mask := net.CIDRMask(16, 32)
				cidrs = append(cidrs, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
			}
		}
	}
	result := make([]*net.IPNet, 0)
	tempMap := make(map[string]*net.IPNet)
	for _, cidr := range cidrs {
		if _, found := tempMap[cidr.String()]; !found {
			tempMap[cidr.String()] = cidr
			result = append(result, cidr)
		}
	}
	if len(result) != 0 {
		return result, nil
	}
	return nil, fmt.Errorf("can not found cidr")
}
