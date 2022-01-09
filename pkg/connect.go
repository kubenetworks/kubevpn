package pkg

import (
	"context"
	"errors"
	"fmt"
	errors2 "github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/dns"
	"github.com/wencaiwulue/kubevpn/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"net"
	"strings"
	"sync"
	"time"
)

type Mode string

const (
	Mesh    Mode = "mesh"
	Reverse Mode = "reverse"
)

type ConnectOptions struct {
	KubeconfigPath string
	Namespace      string
	Mode           Mode
	Headers        map[string]string
	Workloads      []string
	clientset      *kubernetes.Clientset
	restclient     *rest.RESTClient
	config         *rest.Config
	factory        cmdutil.Factory
	cidrs          []*net.IPNet
	dhcp           *DHCPManager
	routerIP       net.IP
	localTunIP     *net.IPNet
}

var trafficManager = net.IPNet{
	IP:   util.RouterIP,
	Mask: util.CIDR.Mask,
}

func (c *ConnectOptions) createRemoteInboundPod() (err error) {
	c.localTunIP, err = c.dhcp.RentIPBaseNICAddress()
	if err != nil {
		return
	}

	tempIps := []*net.IPNet{c.localTunIP}
	wg := sync.WaitGroup{}
	lock := sync.Mutex{}
	for _, workload := range c.Workloads {
		if len(workload) > 0 {
			wg.Add(1)
			/*go*/ func(finalWorkload string) {
				defer wg.Done()
				lock.Lock()
				virtualShadowIp, _ := c.dhcp.RentIPRandom()
				tempIps = append(tempIps, virtualShadowIp)
				lock.Unlock()
				config := util.PodRouteConfig{
					LocalTunIP:           c.localTunIP.IP.String(),
					InboundPodTunIP:      virtualShadowIp.String(),
					TrafficManagerRealIP: c.routerIP.String(),
					Route:                trafficManager.String(),
				}
				// TODO OPTIMIZE CODE
				if c.Mode == Mesh {
					err = PatchSidecar(c.factory, c.clientset, c.Namespace, finalWorkload, config, c.Headers)
				} else {
					err = CreateInboundPod(c.factory, c.Namespace, finalWorkload, config)
				}
				if err != nil {
					log.Error(err)
				}
			}(workload)
		}
	}
	wg.Wait()
	AddCleanUpResourceHandler(c.clientset, c.Namespace, c.dhcp, tempIps...)
	return
}

func (c *ConnectOptions) DoConnect() (err error) {
	c.cidrs, err = getCIDR(c.clientset, c.Namespace)
	if err != nil {
		return
	}
	c.routerIP, err = CreateOutboundRouterPod(c.clientset, c.Namespace, &trafficManager, c.cidrs)
	if err != nil {
		return
	}
	c.dhcp = NewDHCPManager(c.clientset, c.Namespace, &trafficManager)
	if err = c.dhcp.InitDHCP(); err != nil {
		return
	}
	if err = c.createRemoteInboundPod(); err != nil {
		return
	}
	if err = util.WaitPortToBeFree(10800, time.Minute*2); err != nil {
		return err
	}
	if err = c.portForward(ctx); err != nil {
		return err
	}
	if err = c.startLocalTunServe(ctx); err != nil {
		return err
	}
	c.deleteFirewallRuleAndSetupDNS(ctx)
	return
}

func (c *ConnectOptions) portForward(ctx context.Context) error {
	var readyChan = make(chan struct{}, 1)
	var errChan = make(chan error, 1)
	var first = true
	go func() {
		for ctx.Err() == nil {
			func() {
				if !first {
					readyChan = make(chan struct{}, 1)
				}
				err := util.PortForwardPod(
					c.config,
					c.restclient,
					util.TrafficManager,
					c.Namespace,
					"10800:10800",
					readyChan,
					ctx.Done(),
				)
				if first {
					errChan <- err
				}
				first = false
				if apierrors.IsNotFound(err) {
					log.Errorf("can not found port-forward resource, err: %v, exiting", err)
				} else if err != nil {
					log.Errorf("port-forward occurs error, err: %v, retrying", err)
				}
				time.Sleep(time.Second * 2)
			}()
		}
	}()
	select {
	case err := <-errChan:
		return err
	case <-readyChan:
		log.Info("port forward ready")
		return nil
	}
}

func (c *ConnectOptions) startLocalTunServe(ctx context.Context) (err error) {
	if util.IsWindows() {
		c.localTunIP.Mask = net.CIDRMask(0, 32)
	} else {
		c.localTunIP.Mask = net.CIDRMask(24, 32)
	}
	var list = []string{trafficManager.String()}
	for _, ipNet := range c.cidrs {
		list = append(list, ipNet.String())
	}
	r := Route{
		ServeNodes: []string{
			fmt.Sprintf("tun://:8421/127.0.0.1:8421?net=%s&route=%s", c.localTunIP.String(), strings.Join(list, ",")),
		},
		ChainNode: "tcp://127.0.0.1:10800",
		Retries:   5,
	}

	log.Info("your ip is " + c.localTunIP.IP.String())
	if err = Start(ctx, r); err != nil {
		log.Errorf("error while create tunnel, err: %v", err)
	} else {
		log.Info("tunnel connected")
	}
	return
}

func (c *ConnectOptions) deleteFirewallRuleAndSetupDNS(ctx context.Context) {
	if util.IsWindows() {
		if !util.FindRule() {
			util.AddFirewallRule()
		}
		go util.DeleteWindowsFirewallRule(ctx)
	}
	go util.Heartbeats(ctx)
	c.setupDNS()
	log.Info("dns service ok")
}

func (c *ConnectOptions) setupDNS() {
	relovConf, err := dns.GetDNSServiceIPFromPod(c.clientset, c.restclient, c.config, util.TrafficManager, c.Namespace)
	if err != nil {
		log.Fatal(err)
	}
	if err = dns.SetupDNS(relovConf); err != nil {
		log.Fatal(err)
	}
}

func Start(ctx context.Context, r Route) error {
	routers, err := r.GenRouters()
	if err != nil {
		return errors2.WithStack(err)
	}

	if len(routers) == 0 {
		return errors.New("invalid config")
	}
	for _, rr := range routers {
		go func(ctx context.Context, rr router) {
			if err = rr.Serve(ctx); err != nil {
				log.Debug(err)
			}
		}(ctx, rr)
	}
	return nil
}

func getCIDR(clientset *kubernetes.Clientset, namespace string) ([]*net.IPNet, error) {
	var cidrs []*net.IPNet
	if nodeList, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, node := range nodeList.Items {
			if _, ip, _ := net.ParseCIDR(node.Spec.PodCIDR); ip != nil {
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

func (c *ConnectOptions) InitClient() (err error) {
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &c.KubeconfigPath
	c.factory = cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	if c.config, err = c.factory.ToRESTConfig(); err != nil {
		return
	}
	if c.restclient, err = c.factory.RESTClient(); err != nil {
		return
	}
	if c.clientset, err = c.factory.KubernetesClientSet(); err != nil {
		return
	}
	if len(c.Namespace) == 0 {
		if c.Namespace, _, err = c.factory.ToRawKubeConfigLoader().Namespace(); err != nil {
			return
		}
	}
	log.Infof("kubeconfig path: %s, namespace: %s, serivces: %v", c.KubeconfigPath, c.Namespace, c.Workloads)
	return
}

func (c *ConnectOptions) PreCheckResource() {
	for i, workload := range c.Workloads {
		ownerReference, err := util.GetTopOwnerReference(c.factory, c.Namespace, workload)
		if err == nil {
			c.Workloads[i] = fmt.Sprintf("%s/%s", ownerReference.Mapping.Resource.Resource, ownerReference.Name)
		}
	}
}
