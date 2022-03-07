package pkg

import (
	"context"
	"errors"
	"fmt"
	errors2 "github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/config"
	"github.com/wencaiwulue/kubevpn/dns"
	"github.com/wencaiwulue/kubevpn/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"net"
	"os"
	"strings"
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
	// needs to give it back to dhcp
	usedIPs    []*net.IPNet
	routerIP   net.IP
	localTunIP *net.IPNet
}

func (c *ConnectOptions) createRemoteInboundPod() (err error) {
	c.localTunIP, err = c.dhcp.RentIPBaseNICAddress()
	if err != nil {
		return
	}

	tempIps := []*net.IPNet{c.localTunIP}
	//wg := &sync.WaitGroup{}
	//lock := &sync.Mutex{}
	for _, workload := range c.Workloads {
		if len(workload) > 0 {
			//wg.Add(1)
			/*go*/
			func(finalWorkload string) {
				//defer wg.Done()
				//lock.Lock()
				virtualShadowIp, _ := c.dhcp.RentIPRandom()
				tempIps = append(tempIps, virtualShadowIp)
				//lock.Unlock()
				config := util.PodRouteConfig{
					LocalTunIP:           c.localTunIP.IP.String(),
					InboundPodTunIP:      virtualShadowIp.String(),
					TrafficManagerRealIP: c.routerIP.String(),
					Route:                config.CIDR.String(),
				}
				// TODO OPTIMIZE CODE
				if c.Mode == Mesh {
					err = PatchSidecar(c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, finalWorkload, config, c.Headers)
				} else {
					err = CreateInboundPod(c.factory, c.Namespace, finalWorkload, config)
				}
				if err != nil {
					log.Error(err)
				}
			}(workload)
		}
	}
	//wg.Wait()
	c.usedIPs = tempIps
	return
}

func (c *ConnectOptions) DoConnect() (err error) {
	c.addCleanUpResourceHandler(c.clientset, c.Namespace)
	c.cidrs, err = getCIDR(c.clientset, c.Namespace)
	if err != nil {
		return
	}
	trafficMangerNet := net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}
	c.dhcp = NewDHCPManager(c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, &trafficMangerNet)
	if err = c.dhcp.InitDHCP(); err != nil {
		return
	}
	c.routerIP, err = CreateOutboundPod(c.clientset, c.Namespace, trafficMangerNet.String(), c.cidrs)
	if err != nil {
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
	//c.detectConflictDevice()
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
					readyChan = nil
				}
				err := util.PortForwardPod(
					c.config,
					c.restclient,
					config.PodTrafficManager,
					c.Namespace,
					"10800:10800",
					readyChan,
					ctx.Done(),
				)
				if first {
					errChan <- err
				}
				first = false
				// exit normal, let context.err to judge to exit or not
				if err == nil {
					return
				}
				if apierrors.IsNotFound(err) {
					log.Errorln("can not found outbound pod, try to create one")
					tm := net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}
					if _, err = CreateOutboundPod(c.clientset, c.Namespace, tm.String(), c.cidrs); err != nil {
						log.Errorf("error while create traffic manager, will retry after a snap, err: %v", err)
					}
				} else if strings.Contains(err.Error(), "unable to listen on any of the requested ports") ||
					strings.Contains(err.Error(), "address already in use") {
					log.Errorf("port 10800 already in use, needs to release it manually")
					time.Sleep(time.Second * 5)
				} else {
					log.Errorf("port-forward occurs error, err: %v, retrying", err)
				}
				time.Sleep(time.Second * 2)
			}()
		}
	}()
	select {
	case <-time.Tick(time.Second * 60):
		return errors.New("port forward timeout")
	case err := <-errChan:
		return err
	case <-readyChan:
		log.Info("port forward ready")
		return nil
	}
}

func (c *ConnectOptions) startLocalTunServe(ctx context.Context) (err error) {
	// todo fighter it out why
	if util.IsWindows() {
		c.localTunIP.Mask = net.CIDRMask(0, 32)
	}
	var list = []string{config.CIDR.String()}
	for _, ipNet := range c.cidrs {
		list = append(list, ipNet.String())
	}
	r := Route{
		ServeNodes: []string{
			fmt.Sprintf("tun://:8421/127.0.0.1:8422?net=%s&route=%s", c.localTunIP.String(), strings.Join(list, ",")),
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

func (c *ConnectOptions) detectConflictDevice() {
	tun := os.Getenv("tunName")
	if len(tun) == 0 {
		return
	}
	if err := DetectAndDisableConflictDevice(tun); err != nil {
		log.Warnf("error occours while disable conflict devices, err: %v", err)
	}
}

func (c *ConnectOptions) setupDNS() {
	relovConf, err := dns.GetDNSServiceIPFromPod(c.clientset, c.restclient, c.config, config.PodTrafficManager, c.Namespace)
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
	var CIDRList []*net.IPNet
	// get pod CIDR from node spec
	if nodeList, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{}); err == nil {
		var podCIDRs = sets.NewString()
		for _, node := range nodeList.Items {
			if node.Spec.PodCIDRs != nil {
				podCIDRs.Insert(node.Spec.PodCIDRs...)
			}
			if len(node.Spec.PodCIDR) != 0 {
				podCIDRs.Insert(node.Spec.PodCIDR)
			}
		}
		for _, podCIDR := range podCIDRs.List() {
			if _, CIDR, err := net.ParseCIDR(podCIDR); err == nil {
				CIDRList = append(CIDRList, CIDR)
			}
		}
	}
	// if node spec can not contain pod IP
	if podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, pod := range podList.Items {
			if ip := net.ParseIP(pod.Status.PodIP); ip != nil {
				var contains bool
				for _, CIDR := range CIDRList {
					if CIDR.Contains(ip) {
						contains = true
						break
					}
				}
				if !contains {
					mask := net.CIDRMask(24, 32)
					CIDRList = append(CIDRList, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
				}
			}
		}
	}
	// pod CIDR maybe is not same with service CIDR
	if serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, service := range serviceList.Items {
			if ip := net.ParseIP(service.Spec.ClusterIP); ip != nil {
				var contains bool
				for _, CIDR := range CIDRList {
					if CIDR.Contains(ip) {
						contains = true
						break
					}
				}
				if !contains {
					mask := net.CIDRMask(16, 32)
					CIDRList = append(CIDRList, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
				}
			}
		}
	}

	// remove duplicate CIDR
	result := make([]*net.IPNet, 0)
	set := sets.NewString()
	for _, cidr := range CIDRList {
		if !set.Has(cidr.String()) {
			set.Insert(cidr.String())
			result = append(result, cidr)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("can not found any CIDR")
	}
	return result, nil
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
	c.factory.ToRESTConfig()
	log.Infof("kubeconfig path: %s, namespace: %s, services: %v", c.KubeconfigPath, c.Namespace, c.Workloads)
	return
}

// PreCheckResource transform user parameter to normal, example:
// pod: productpage-7667dfcddb-cbsn5
// replicast: productpage-7667dfcddb
// deployment: productpage
// transform:
// pod/productpage-7667dfcddb-cbsn5 --> deployment/productpage
// service/productpage --> deployment/productpage
// replicaset/productpage-7667dfcddb --> deployment/productpage
//
// pods without controller
// pod/productpage-without-controller --> pod/productpage-without-controller
// service/productpage-without-pod --> controller/controllerName
func (c *ConnectOptions) PreCheckResource() {
	// normal workloads, like pod with controller, deployments, statefulset, replicaset etc...
	for i, workload := range c.Workloads {
		ownerReference, err := util.GetTopOwnerReference(c.factory, c.Namespace, workload)
		if err == nil {
			c.Workloads[i] = fmt.Sprintf("%s/%s", ownerReference.Mapping.GroupVersionKind.GroupKind().String(), ownerReference.Name)
		}
	}
	// service which associate with pod
	for i, workload := range c.Workloads {
		object, err := util.GetUnstructuredObject(c.factory, c.Namespace, workload)
		if err != nil {
			continue
		}
		if object.Mapping.Resource.Resource != "services" {
			continue
		}
		get, err := c.clientset.CoreV1().Services(c.Namespace).Get(context.TODO(), object.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if ns, selector, err := polymorphichelpers.SelectorsForObject(get); err == nil {
			list, err := c.clientset.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{
				LabelSelector: selector.String(),
			})
			// if pod is not empty, using pods to find top controller
			if err == nil && list != nil && len(list.Items) != 0 {
				ownerReference, err := util.GetTopOwnerReference(c.factory, c.Namespace, fmt.Sprintf("%s/%s", "pods", list.Items[0].Name))
				if err == nil {
					c.Workloads[i] = fmt.Sprintf("%s/%s", ownerReference.Mapping.GroupVersionKind.GroupKind().String(), ownerReference.Name)
				}
			} else
			// if list is empty, means not create pods, just controllers
			{
				controller, err := util.GetTopOwnerReferenceBySelector(c.factory, c.Namespace, selector.String())
				if err == nil {
					if len(controller) > 0 {
						c.Workloads[i] = controller.List()[0]
					}
				}
				// only a single service, not support it yet
				if controller.Len() == 0 {
					log.Fatalf("Not support resources: %s", workload)
				}
			}
		}
	}
}
