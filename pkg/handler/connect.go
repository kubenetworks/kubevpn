package handler

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/dns"
	"github.com/wencaiwulue/kubevpn/pkg/route"
	"github.com/wencaiwulue/kubevpn/pkg/util"
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
				configInfo := util.PodRouteConfig{
					LocalTunIP:           c.localTunIP.IP.String(),
					InboundPodTunIP:      virtualShadowIp.String(),
					TrafficManagerRealIP: c.routerIP.String(),
					Route:                config.CIDR.String(),
				}
				// TODO OPTIMIZE CODE
				if c.Mode == Mesh {
					err = InjectVPNAndEnvoySidecar(c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, finalWorkload, configInfo, c.Headers)
				} else {
					err = InjectVPNSidecar(c.factory, c.Namespace, finalWorkload, configInfo)
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
	subCtx, cancelFunc := context.WithTimeout(ctx, time.Minute*2)
	defer cancelFunc()
	if err = util.WaitPortToBeFree(subCtx, 10800); err != nil {
		return err
	}
	if err = c.portForward(ctx, 10800); err != nil {
		return err
	}
	if err = c.startLocalTunServe(ctx); err != nil {
		return err
	}
	c.deleteFirewallRuleAndSetupDNS(ctx)
	//c.detectConflictDevice()
	return
}

// detect pod is delete event, if pod is deleted, needs to redo port-forward immediately
func (c *ConnectOptions) portForward(ctx context.Context, port int) error {
	var childCtx context.Context
	var cancelFunc context.CancelFunc
	var curPodName = &atomic.Value{}
	var readyChan = make(chan struct{}, 1)
	var errChan = make(chan error, 1)
	var first = true
	podInterface := c.clientset.CoreV1().Pods(c.Namespace)
	go func() {
		for ctx.Err() == nil {
			func() {
				podList, err := c.GetRunningPodList()
				if err != nil {
					time.Sleep(time.Second * 1)
					return
				}
				childCtx, cancelFunc = context.WithCancel(ctx)
				defer cancelFunc()
				if !first {
					readyChan = nil
				}
				podName := podList[0].GetName()
				// if port-forward occurs error, check pod is deleted or not, speed up fail
				runtime.ErrorHandlers = []func(error){func(err error) {
					pod, err := podInterface.Get(childCtx, podName, metav1.GetOptions{})
					if apierrors.IsNotFound(err) || pod.GetDeletionTimestamp() != nil {
						cancelFunc()
					}
				}}
				curPodName.Store(podName)
				err = util.PortForwardPod(
					c.config,
					c.restclient,
					podName,
					c.Namespace,
					strconv.Itoa(port),
					readyChan,
					childCtx.Done(),
				)
				if first {
					errChan <- err
				}
				first = false
				// exit normal, let context.err to judge to exit or not
				if err == nil {
					return
				}
				if strings.Contains(err.Error(), "unable to listen on any of the requested ports") ||
					strings.Contains(err.Error(), "address already in use") {
					log.Errorf("port %d already in use, needs to release it manually", port)
					time.Sleep(time.Second * 5)
				} else {
					log.Errorf("port-forward occurs error, err: %v, retrying", err)
					time.Sleep(time.Second * 2)
				}
			}()
		}
	}()

	// try to detect pod is delete event, if pod is deleted, needs to redo port-forward
	go func() {
		for ctx.Err() == nil {
			func() {
				podName := curPodName.Load()
				if podName == nil || childCtx == nil || cancelFunc == nil {
					time.Sleep(2 * time.Second)
					return
				}
				stream, err := podInterface.Watch(childCtx, metav1.ListOptions{
					FieldSelector: fields.OneTermEqualSelector("metadata.name", podName.(string)).String(),
				})
				if apierrors.IsForbidden(err) {
					time.Sleep(30 * time.Second)
					return
				}
				if err != nil {
					return
				}
				defer stream.Stop()
				for childCtx.Err() == nil {
					select {
					case e := <-stream.ResultChan():
						if e.Type == watch.Deleted {
							cancelFunc()
							return
						}
					}
				}
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
	// todo figure it out why
	if util.IsWindows() {
		c.localTunIP.Mask = net.CIDRMask(0, 32)
	}
	var list = []string{config.CIDR.String()}
	for _, ipNet := range c.cidrs {
		list = append(list, ipNet.String())
	}
	r := Route{
		ServeNodes: []string{
			fmt.Sprintf("tun:/127.0.0.1:8422?net=%s&route=%s", c.localTunIP.String(), strings.Join(list, ",")),
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
	if err := route.DetectAndDisableConflictDevice(tun); err != nil {
		log.Warnf("error occours while disable conflict devices, err: %v", err)
	}
}

func (c *ConnectOptions) setupDNS() {
	pod, err := c.GetRunningPodList()
	if err != nil {
		log.Fatal(err)
	}
	relovConf, err := dns.GetDNSServiceIPFromPod(c.clientset, c.restclient, c.config, pod[0].GetName(), c.Namespace)
	if err != nil {
		log.Fatal(err)
	}
	if err = dns.SetupDNS(relovConf); err != nil {
		log.Fatal(err)
	}
}

func Start(ctx context.Context, r Route) error {
	servers, err := r.GenerateServers()
	if err != nil {
		return errors.WithStack(err)
	}
	if len(servers) == 0 {
		return errors.New("invalid config")
	}
	for _, rr := range servers {
		go func(ctx context.Context, server core.Server) {
			if err = server.Serve(ctx); err != nil {
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
	// get pod CIDR from pod ip, why doing this: notice that node's pod cidr is not correct in minikube
	// ➜  ~ kubectl get nodes -o jsonpath='{.items[*].spec.podCIDR}'
	//10.244.0.0/24%
	// ➜  ~  kubectl get pods -o=custom-columns=podIP:.status.podIP
	//podIP
	//172.17.0.5
	//172.17.0.4
	//172.17.0.4
	//172.17.0.3
	//172.17.0.3
	//172.17.0.6
	//172.17.0.8
	//172.17.0.3
	//172.17.0.7
	//172.17.0.2
	if podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{}); err == nil {
		for _, pod := range podList.Items {
			if ip := net.ParseIP(pod.Status.PodIP); ip != nil {
				var contain bool
				for _, CIDR := range CIDRList {
					if CIDR.Contains(ip) {
						contain = true
						break
					}
				}
				if !contain {
					mask := net.CIDRMask(24, 32)
					CIDRList = append(CIDRList, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
				}
			}
		}
	}

	// get service CIDR
	defaultCIDRIndex := "The range of valid IPs is"
	if _, err := clientset.CoreV1().Services(namespace).Create(context.TODO(), &v1.Service{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "foo-svc-"},
		Spec:       v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 80}}, ClusterIP: "0.0.0.0"},
	}, metav1.CreateOptions{}); err != nil {
		idx := strings.LastIndex(err.Error(), defaultCIDRIndex)
		if idx != -1 {
			if _, cidr, err := net.ParseCIDR(strings.TrimSpace(err.Error()[idx+len(defaultCIDRIndex):])); err == nil {
				CIDRList = append(CIDRList, cidr)
			}
		}
	} else {
		if serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{}); err == nil {
			for _, service := range serviceList.Items {
				if ip := net.ParseIP(service.Spec.ClusterIP); ip != nil {
					var contain bool
					for _, CIDR := range CIDRList {
						if CIDR.Contains(ip) {
							contain = true
							break
						}
					}
					if !contain {
						mask := net.CIDRMask(16, 32)
						CIDRList = append(CIDRList, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
					}
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
	if _, err = os.Stat(c.KubeconfigPath); err == nil {
		configFlags.KubeConfig = &c.KubeconfigPath
	}

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

func (c *ConnectOptions) GetRunningPodList() ([]v1.Pod, error) {
	list, err := c.clientset.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", config.PodTrafficManager).String(),
	})
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(list.Items); i++ {
		if list.Items[i].GetDeletionTimestamp() != nil || list.Items[i].Status.Phase != v1.PodRunning {
			list.Items = append(list.Items[:i], list.Items[i+1:]...)
			i--
		}
	}
	if len(list.Items) == 0 {
		return nil, errors.New("can not found any running pod")
	}
	return list.Items, nil
}
