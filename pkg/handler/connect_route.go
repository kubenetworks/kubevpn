package handler

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/libp2p/go-netroute"
	v1 "k8s.io/api/core/v1"
	apinetworkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	v2 "k8s.io/client-go/kubernetes/typed/networking/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (c *ConnectOptions) addRouteDynamic(ctx context.Context) (cache.SharedIndexInformer, cache.SharedIndexInformer, error) {
	podNs, svcNs, err := util.GetNsForListPodAndSvc(ctx, c.clientset, []string{v1.NamespaceAll, c.OriginNamespace})
	if err != nil {
		return nil, nil, err
	}

	conf := rest.CopyConfig(c.config)
	conf.QPS = 1
	conf.Burst = 2
	clientSet, err := kubernetes.NewForConfig(conf)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create clientset: %v", err)
		return nil, nil, err
	}
	svcIndexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	svcInformer := informerv1.NewServiceInformer(clientSet, svcNs, 0, svcIndexers)
	if err = c.watchAndRoute(ctx, svcInformer, func(obj any) []string {
		svc, ok := obj.(*v1.Service)
		if !ok {
			return nil
		}
		return append([]string{svc.Spec.ClusterIP}, svc.Spec.ClusterIPs...)
	}); err != nil {
		return nil, nil, err
	}

	podIndexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	podInformer := informerv1.NewPodInformer(clientSet, podNs, 0, podIndexers)
	if err = c.watchAndRoute(ctx, podInformer, func(obj any) []string {
		p, ok := obj.(*v1.Pod)
		if !ok || p.Spec.HostNetwork {
			return nil
		}
		return util.GetPodIP(*p)
	}); err != nil {
		return nil, nil, err
	}

	return svcInformer, podInformer, nil
}

// watchAndRoute starts an informer and a goroutine that periodically extracts
// IPs from the cache and adds them to the route table.
func (c *ConnectOptions) watchAndRoute(ctx context.Context, informer cache.SharedIndexInformer, extractIPs func(any) []string) error {
	ticker := time.NewTicker(time.Second * 15)
	_, err := informer.AddEventHandler(newTickerResetHandler(ticker))
	if err != nil {
		return err
	}
	go informer.Run(ctx.Done())
	go func() {
		defer ticker.Stop()
		for ; ctx.Err() == nil; <-ticker.C {
			ticker.Reset(time.Second * 15)
			ips := sets.New[string]()
			for _, obj := range informer.GetIndexer().List() {
				ips.Insert(extractIPs(obj)...)
			}
			if ctx.Err() != nil {
				return
			}
			if ips.Len() == 0 {
				continue
			}
			if err := c.addRoute(ips.UnsortedList()...); err != nil {
				plog.G(ctx).Debugf("Add IP to route table failed: %v", err)
			}
		}
	}()
	return nil
}

func (c *ConnectOptions) addRoute(ipStrList ...string) error {
	if c.tunName == "" {
		return nil
	}
	var routes []types.Route
	r, _ := netroute.New()
	for _, ipStr := range ipStrList {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		var match bool
		for _, p := range c.apiServerIPs {
			// if pod ip or service ip is equal to apiServer ip, can not add it to route table
			if p.Equal(ip) {
				match = true
				break
			}
		}
		if match {
			continue
		}
		var mask net.IPMask
		if ip.To4() != nil {
			mask = net.CIDRMask(32, 32)
		} else {
			mask = net.CIDRMask(128, 128)
		}
		if r != nil {
			ifi, _, _, err := r.Route(ip)
			if err == nil && ifi.Name == c.tunName {
				continue
			}
		}
		routes = append(routes, types.Route{Dst: net.IPNet{IP: ip, Mask: mask}})
	}
	if len(routes) == 0 {
		return nil
	}
	err := tun.AddRoutes(c.tunName, routes...)
	return err
}

func (c *ConnectOptions) addExtraRoute(ctx context.Context, name string) error {
	if len(c.ExtraRouteInfo.ExtraDomain) == 0 {
		return nil
	}

	// parse cname
	//dig +short db-name.postgres.database.azure.com
	//1234567.privatelink.db-name.postgres.database.azure.com.
	//10.0.100.1
	var parseIP = func(cmdDigOutput string) net.IP {
		for _, s := range strings.Split(cmdDigOutput, "\n") {
			ip := net.ParseIP(strings.TrimSpace(s))
			if ip != nil {
				return ip
			}
		}
		return nil
	}

	// 1) use dig +short query, if ok, just return
	for _, domain := range c.ExtraRouteInfo.ExtraDomain {
		output, err := util.Shell(ctx, c.clientset, c.config, name, config.ContainerSidecarVPN, c.ManagerNamespace, []string{"dig", "+short", domain})
		if err != nil {
			return fmt.Errorf("failed to resolve DNS for domain by command dig: %w", err)
		}
		var ip string
		if parseIP(output) == nil {
			// try to get ingress record
			ip = getIngressRecord(ctx, c.clientset.NetworkingV1(), []string{v1.NamespaceAll, c.ManagerNamespace}, domain)
		} else {
			ip = parseIP(output).String()
		}
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("failed to resolve DNS for domain %s by command dig, output: %s", domain, output)
		}
		err = c.addRoute(ip)
		if err != nil {
			plog.G(ctx).Errorf("Failed to add IP: %s to route table: %v", ip, err)
			return err
		}
		c.extraHost = append(c.extraHost, dns.Entry{IP: net.ParseIP(ip).String(), Domain: domain})
	}
	return nil
}

func getIngressRecord(ctx context.Context, ingressInterface v2.NetworkingV1Interface, nsList []string, domain string) string {
	var ingressList []apinetworkingv1.Ingress
	for _, ns := range nsList {
		list, err := ingressInterface.Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			plog.G(ctx).Debugf("Failed to list ingresses in namespace %s: %v", ns, err)
			continue
		}
		ingressList = append(ingressList, list.Items...)
	}
	for _, item := range ingressList {
		for _, rule := range item.Spec.Rules {
			if rule.Host == domain {
				for _, ingress := range item.Status.LoadBalancer.Ingress {
					if ingress.IP != "" {
						return ingress.IP
					}
				}
			}
		}
		for _, tl := range item.Spec.TLS {
			if slices.Contains(tl.Hosts, domain) {
				for _, ingress := range item.Status.LoadBalancer.Ingress {
					if ingress.IP != "" {
						return ingress.IP
					}
				}
			}
		}
	}
	return ""
}

func (c *ConnectOptions) addExtraNodeIP(ctx context.Context) error {
	if !c.ExtraRouteInfo.ExtraNodeIP {
		return nil
	}
	list, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, item := range list.Items {
		for _, address := range item.Status.Addresses {
			ip := net.ParseIP(address.Address)
			if ip != nil {
				var mask net.IPMask
				if ip.To4() != nil {
					mask = net.CIDRMask(32, 32)
				} else {
					mask = net.CIDRMask(128, 128)
				}
				c.ExtraRouteInfo.ExtraCIDR = append(c.ExtraRouteInfo.ExtraCIDR, (&net.IPNet{
					IP:   ip,
					Mask: mask,
				}).String())
			}
		}
	}
	return nil
}

// newTickerResetHandler returns event handler funcs that reset the given ticker
// to 3 seconds on any add, update, or delete event. This debounces rapid
// informer events into a single route-table update.
func newTickerResetHandler(ticker *time.Ticker) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			ticker.Reset(time.Second * 3)
		},
		UpdateFunc: func(oldObj, newObj any) {
			ticker.Reset(time.Second * 3)
		},
		DeleteFunc: func(obj any) {
			ticker.Reset(time.Second * 3)
		},
	}
}
