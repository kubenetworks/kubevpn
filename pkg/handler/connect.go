package handler

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
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
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/dns"
	"github.com/wencaiwulue/kubevpn/pkg/route"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type ConnectOptions struct {
	KubeconfigPath string
	Namespace      string
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

	for _, workload := range c.Workloads {
		if len(workload) > 0 {
			//virtualShadowIp, _ := c.dhcp.RentIPRandom()
			//c.usedIPs = append(c.usedIPs, virtualShadowIp)
			configInfo := util.PodRouteConfig{
				LocalTunIP: c.localTunIP.IP.String(),
				//InboundPodTunIP:      virtualShadowIp.String(),
				TrafficManagerRealIP: c.routerIP.String(),
				Route:                config.CIDR.String(),
			}
			RollbackFuncList = append(RollbackFuncList, func() {
				r := c.factory.NewBuilder().
					WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
					NamespaceParam(c.Namespace).DefaultNamespace().
					ResourceTypeOrNameArgs(true, workload).
					ContinueOnError().
					Latest().
					Flatten().
					Do()
				if r.Err() == nil {
					_ = r.Visit(func(info *resource.Info, err error) error {
						if err != nil {
							return err
						}
						rollbacker, err := polymorphichelpers.RollbackerFn(c.factory, info.ResourceMapping())
						if err != nil {
							return err
						}
						_, err = rollbacker.Rollback(info.Object, nil, 0, cmdutil.DryRunNone)
						return err
					})
				}
			})
			// means mesh mode
			if len(c.Headers) != 0 {
				err = InjectVPNAndEnvoySidecar(c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, workload, configInfo, c.Headers)
			} else {
				err = InjectVPNSidecar(c.factory, c.Namespace, workload, configInfo)
			}
			if err != nil {
				log.Error(err)
				return err
			}
			RollbackFuncList = RollbackFuncList[0 : len(RollbackFuncList)-1]
		}
	}
	return
}

func (c *ConnectOptions) DoConnect() (err error) {
	c.addCleanUpResourceHandler(c.clientset, c.Namespace)
	trafficMangerNet := net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}
	c.dhcp = NewDHCPManager(c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, &trafficMangerNet)
	if err = c.dhcp.InitDHCP(); err != nil {
		return
	}
	err = c.GetCIDR()
	if err != nil {
		return
	}
	c.routerIP, err = CreateOutboundPod(c.factory, c.clientset, c.Namespace, trafficMangerNet.String(), c.cidrs)
	if err != nil {
		return
	}
	if err = c.createRemoteInboundPod(); err != nil {
		return
	}
	port := util.GetAvailableTCPPortOrDie()
	err = c.portForward(ctx, fmt.Sprintf("%d:10800", port))
	if err != nil {
		return err
	}
	err = c.startLocalTunServe(ctx, fmt.Sprintf("tcp://127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	c.deleteFirewallRuleAndSetupDNS(ctx)
	//c.detectConflictDevice()
	return
}

// detect pod is delete event, if pod is deleted, needs to redo port-forward immediately
func (c *ConnectOptions) portForward(ctx context.Context, port string) error {
	var readyChan = make(chan struct{}, 1)
	var errChan = make(chan error, 1)
	podInterface := c.clientset.CoreV1().Pods(c.Namespace)
	go func() {
		var first = pointer.Bool(true)
		for ctx.Err() == nil {
			func() {
				podList, err := c.GetRunningPodList()
				if err != nil {
					time.Sleep(time.Second * 3)
					return
				}
				childCtx, cancelFunc := context.WithCancel(ctx)
				defer cancelFunc()
				if !*first {
					readyChan = nil
				}
				podName := podList[0].GetName()
				// if port-forward occurs error, check pod is deleted or not, speed up fail
				runtime.ErrorHandlers = []func(error){func(err error) {
					pod, err := podInterface.Get(childCtx, podName, metav1.GetOptions{})
					if apierrors.IsNotFound(err) || (pod != nil && pod.GetDeletionTimestamp() != nil) {
						cancelFunc()
					}
				}}
				// try to detect pod is delete event, if pod is deleted, needs to redo port-forward
				go checkPodStatus(childCtx, cancelFunc, podName, podInterface)
				err = util.PortForwardPod(
					c.config,
					c.restclient,
					podName,
					c.Namespace,
					port,
					readyChan,
					childCtx.Done(),
				)
				if *first {
					errChan <- err
				}
				first = pointer.Bool(false)
				// exit normal, let context.err to judge to exit or not
				if err == nil {
					return
				}
				if strings.Contains(err.Error(), "unable to listen on any of the requested ports") ||
					strings.Contains(err.Error(), "address already in use") {
					log.Errorf("port %s already in use, needs to release it manually", port)
					time.Sleep(time.Second * 5)
				} else {
					log.Errorf("port-forward occurs error, err: %v, retrying", err)
					time.Sleep(time.Second * 2)
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

func checkPodStatus(cCtx context.Context, cFunc context.CancelFunc, podName string, podInterface v12.PodInterface) {
	w, err := podInterface.Watch(cCtx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", podName).String(),
	})
	if err != nil {
		return
	}
	defer w.Stop()
	for {
		select {
		case e := <-w.ResultChan():
			switch e.Type {
			case watch.Deleted:
				cFunc()
				return
			case watch.Error:
				return
			case watch.Added, watch.Modified, watch.Bookmark:
				// do nothing
			default:
				return
			}
		case <-cCtx.Done():
			return
		}
	}
}

func (c *ConnectOptions) startLocalTunServe(ctx context.Context, forwardAddress string) (err error) {
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
		ChainNode: forwardAddress,
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
	const port = 53
	pod, err := c.GetRunningPodList()
	if err != nil {
		log.Fatal(err)
	}
	relovConf, err := dns.GetDNSServiceIPFromPod(c.clientset, c.restclient, c.config, pod[0].GetName(), c.Namespace)
	if err != nil {
		log.Fatal(err)
	}
	if relovConf.Port == "" {
		relovConf.Port = strconv.Itoa(port)
	}
	ns := sets.NewString()
	list, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, item := range list.Items {
			ns.Insert(item.Name)
		}
	}
	svc, err := c.clientset.CoreV1().Services(c.Namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, item := range svc.Items {
			ns.Insert(item.Name)
		}
	}
	if err = dns.SetupDNS(relovConf, ns.List()); err != nil {
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
		get, err := c.clientset.CoreV1().Services(c.Namespace).Get(context.Background(), object.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if ns, selector, err := polymorphichelpers.SelectorsForObject(get); err == nil {
			list, err := c.clientset.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
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
		LabelSelector: fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String(),
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

// GetCIDR
// 1: get pod cidr
// 2: get service cidr
// todo optimize code should distinguish service cidr and pod cidr
// https://stackoverflow.com/questions/45903123/kubernetes-set-service-cidr-and-pod-cidr-the-same
// https://stackoverflow.com/questions/44190607/how-do-you-find-the-cluster-service-cidr-of-a-kubernetes-cluster/54183373#54183373
// https://stackoverflow.com/questions/44190607/how-do-you-find-the-cluster-service-cidr-of-a-kubernetes-cluster
func (c *ConnectOptions) GetCIDR() (err error) {
	// (1) get cidr from cache
	var value string
	value, err = c.dhcp.Get(config.KeyClusterIPv4POOLS)
	if err == nil {
		for _, s := range strings.Split(value, " ") {
			_, cidr, _ := net.ParseCIDR(s)
			if cidr != nil {
				c.cidrs = append(c.cidrs, cidr)
			}
		}
	}
	if len(c.cidrs) != 0 {
		log.Infoln("got cidr from cache")
		return
	}

	// (2) get cidr from cni
	c.cidrs, err = util.GetCIDRElegant(c.clientset, c.restclient, c.config, c.Namespace)
	if err == nil {
		s := sets.NewString()
		for _, cidr := range c.cidrs {
			s.Insert(cidr.String())
		}
		_ = c.dhcp.Set(config.KeyClusterIPv4POOLS, strings.Join(s.List(), " "))
		return
	}

	// (3) fallback to get cidr from node/pod/service
	c.cidrs, err = util.GetCIDRFromResourceUgly(c.clientset, c.Namespace)
	return
}
