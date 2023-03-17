package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	netroute "github.com/libp2p/go-netroute"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/dns"
	"github.com/wencaiwulue/kubevpn/pkg/driver"
	"github.com/wencaiwulue/kubevpn/pkg/tun"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type ConnectOptions struct {
	Namespace string
	Headers   map[string]string
	Workloads []string
	ExtraCIDR []string

	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
	factory    cmdutil.Factory
	cidrs      []*net.IPNet
	dhcp       *DHCPManager
	// needs to give it back to dhcp
	usedIPs    []*net.IPNet
	routerIP   net.IP
	localTunIP *net.IPNet
}

func (c *ConnectOptions) createRemoteInboundPod(ctx1 context.Context) (err error) {
	c.localTunIP, err = c.dhcp.RentIPBaseNICAddress()
	if err != nil {
		return
	}

	for _, workload := range c.Workloads {
		configInfo := util.PodRouteConfig{
			LocalTunIP:           c.localTunIP.IP.String(),
			TrafficManagerRealIP: c.routerIP.String(),
		}
		// means mesh mode
		if len(c.Headers) != 0 {
			err = InjectVPNAndEnvoySidecar(ctx1, c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, workload, configInfo, c.Headers)
		} else {
			err = InjectVPNSidecar(ctx1, c.factory, c.Namespace, workload, configInfo)
		}
		if err != nil {
			return err
		}
	}
	return
}

func Rollback(f cmdutil.Factory, ns, workload string) {
	r := f.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(ns).DefaultNamespace().
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
			rollbacker, err := polymorphichelpers.RollbackerFn(f, info.ResourceMapping())
			if err != nil {
				return err
			}
			_, err = rollbacker.Rollback(info.Object, nil, 0, cmdutil.DryRunNone)
			return err
		})
	}
}

func (c *ConnectOptions) DoConnect() (err error) {
	c.addCleanUpResourceHandler()
	trafficMangerNet := net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}
	c.dhcp = NewDHCPManager(c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, &trafficMangerNet)
	if err = c.dhcp.InitDHCP(ctx); err != nil {
		return
	}
	err = c.GetCIDR(ctx)
	if err != nil {
		return
	}
	c.routerIP, err = CreateOutboundPod(ctx, c.factory, c.clientset, c.Namespace, trafficMangerNet.String())
	if err != nil {
		return
	}
	if err = c.createRemoteInboundPod(ctx); err != nil {
		return
	}
	port := util.GetAvailableTCPPortOrDie()
	err = c.portForward(ctx, fmt.Sprintf("%d:10800", port))
	if err != nil {
		return err
	}
	if util.IsWindows() {
		driver.InstallWireGuardTunDriver()
	}
	err = c.startLocalTunServe(ctx, fmt.Sprintf("tcp://127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	c.addRouteDynamic(ctx)
	c.deleteFirewallRule(ctx)
	err = c.setupDNS()
	if err != nil {
		return err
	}
	log.Info("dns service ok")
	return
}

// detect pod is delete event, if pod is deleted, needs to redo port-forward immediately
func (c *ConnectOptions) portForward(ctx context.Context, port string) error {
	var readyChan = make(chan struct{}, 1)
	var errChan = make(chan error, 1)
	podInterface := c.clientset.CoreV1().Pods(c.Namespace)
	go func() {
		var first = pointer.Bool(true)
		for {
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
					log.Debugf("port-forward occurs error, err: %v, retrying", err)
					cancelFunc()
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
					log.Debugf("port-forward occurs error, err: %v, retrying", err)
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
	var list = sets.New[string](config.CIDR.String())
	for _, ipNet := range c.cidrs {
		list.Insert(ipNet.String())
	}
	// add extra-cidr
	for _, s := range c.ExtraCIDR {
		_, _, err = net.ParseCIDR(s)
		if err != nil {
			return fmt.Errorf("invalid extra-cidr %s, err: %v", s, err)
		}
		list.Insert(s)
	}
	r := core.Route{
		ServeNodes: []string{
			fmt.Sprintf("tun:/127.0.0.1:8422?net=%s&route=%s", c.localTunIP.String(), strings.Join(list.UnsortedList(), ",")),
		},
		ChainNode: forwardAddress,
		Retries:   5,
	}

	log.Debugf("your ip is %s", c.localTunIP.IP.String())
	if err = Start(ctx, r); err != nil {
		log.Errorf("error while create tunnel, err: %v", errors.WithStack(err))
	} else {
		log.Info("tunnel connected")
	}
	return
}

// Listen all pod, add route if needed
func (c *ConnectOptions) addRouteDynamic(ctx context.Context) {
	r, err := netroute.New()
	if err != nil {
		return
	}

	tunIface, err := tun.GetInterface()
	if err != nil {
		return
	}

	addRouteFunc := func(resource, ip string) {
		if ip == "" || net.ParseIP(ip) == nil {
			return
		}
		// if route is right, not need add route
		iface, _, _, err := r.Route(net.ParseIP(ip))
		if err == nil && tunIface.Name == iface.Name {
			return
		}
		err = tun.AddRoutes(types.Route{Dst: net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(32, 32)}})
		if err != nil {
			log.Debugf("[route] add route failed, pod: %s, ip: %s,err: %v", resource, ip, err)
		}
	}

	// add pod route
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				func() {
					defer func() {
						if er := recover(); er != nil {
							log.Errorln(er)
						}
					}()
					w, err := c.clientset.CoreV1().Pods(v1.NamespaceAll).Watch(ctx, metav1.ListOptions{Watch: true, TimeoutSeconds: pointer.Int64(30)})
					if err != nil {
						time.Sleep(time.Second * 5)
						log.Debugf("wait pod failed, err: %v", err)
						return
					}
					defer w.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case e, ok := <-w.ResultChan():
							if !ok {
								return
							}
							if e.Type != watch.Added {
								continue
							}
							var pod *v1.Pod
							pod, ok = e.Object.(*v1.Pod)
							if !ok {
								continue
							}
							if pod.Spec.HostNetwork {
								continue
							}
							if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
								continue
							}
							addRouteFunc(pod.Name, pod.Status.PodIP)
						}
					}
				}()
			}
		}
	}()

	// add service route
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				func() {
					defer func() {
						if er := recover(); er != nil {
							log.Errorln(er)
						}
					}()
					w, err := c.clientset.CoreV1().Services(v1.NamespaceAll).Watch(ctx, metav1.ListOptions{Watch: true, TimeoutSeconds: pointer.Int64(30)})
					if err != nil {
						log.Debugf("wait service failed, err: %v", err)
						time.Sleep(time.Second * 5)
						return
					}
					defer w.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case e, ok := <-w.ResultChan():
							if !ok {
								return
							}
							if e.Type != watch.Added {
								continue
							}
							var pod *v1.Service
							pod, ok = e.Object.(*v1.Service)
							if !ok {
								continue
							}
							ip := pod.Spec.ClusterIP
							addRouteFunc(pod.Name, ip)
						}
					}
				}()
			}
		}
	}()
}

func (c *ConnectOptions) deleteFirewallRule(ctx context.Context) {
	if !util.FindAllowFirewallRule() {
		util.AddAllowFirewallRule()
	}
	RollbackFuncList = append(RollbackFuncList, util.DeleteAllowFirewallRule)
	go util.DeleteBlockFirewallRule(ctx)
}

func (c *ConnectOptions) setupDNS() error {
	const port = 53
	pod, err := c.GetRunningPodList()
	if err != nil {
		log.Errorln(err)
		return err
	}
	relovConf, err := dns.GetDNSServiceIPFromPod(c.clientset, c.restclient, c.config, pod[0].GetName(), c.Namespace)
	if err != nil {
		log.Errorln(err)
		return err
	}
	if relovConf.Port == "" {
		relovConf.Port = strconv.Itoa(port)
	}
	ns := sets.New[string]()
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
	if err = dns.SetupDNS(relovConf, ns.UnsortedList()); err != nil {
		return err
	}
	// dump service in current namespace for support DNS resolve service:port
	go dns.AddServiceNameToHosts(ctx, c.clientset.CoreV1().Services(c.Namespace))
	return nil
}

func Start(ctx context.Context, r core.Route) error {
	servers, err := r.GenerateServers()
	if err != nil {
		return errors.WithStack(err)
	}
	if len(servers) == 0 {
		return errors.New("invalid config")
	}
	for _, server := range servers {
		go func(ctx context.Context, server core.Server) {
			l := server.Listener
			defer l.Close()
			for {
				conn, err := l.Accept()
				if err != nil {
					log.Warnf("server: accept error: %v", err)
					continue
				}
				go server.Handler.Handle(ctx, conn)
			}
		}(ctx, server)
	}
	return nil
}

func (c *ConnectOptions) InitClient(f cmdutil.Factory) (err error) {
	c.factory = f
	if c.config, err = c.factory.ToRESTConfig(); err != nil {
		return
	}
	if c.restclient, err = c.factory.RESTClient(); err != nil {
		return
	}
	if c.clientset, err = c.factory.KubernetesClientSet(); err != nil {
		return
	}
	if c.Namespace, _, err = c.factory.ToRawKubeConfigLoader().Namespace(); err != nil {
		return
	}
	return
}

func SshJump(conf *util.SshConfig, flags *pflag.FlagSet) (err error) {
	if conf.Addr == "" && conf.ConfigAlias == "" {
		return
	}
	defer func() {
		if er := recover(); er != nil {
			err = er.(error)
		}
	}()
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	if flags != nil {
		lookup := flags.Lookup("kubeconfig")
		if lookup != nil && lookup.Value != nil && lookup.Value.String() != "" {
			configFlags.KubeConfig = pointer.String(lookup.Value.String())
		}
	}
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	rawConfig, err := matchVersionFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return err
	}
	err = api.FlattenConfig(&rawConfig)
	server := rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].Server
	u, err := url.Parse(server)
	if err != nil {
		return err
	}
	remote, err := netip.ParseAddrPort(u.Host)
	if err != nil {
		return err
	}

	var local = &netip.AddrPort{}
	errChan := make(chan error, 1)
	readyChan := make(chan struct{}, 1)
	go func() {
		err := util.Main(&remote, local, conf, readyChan)
		if err != nil {
			errChan <- err
			return
		}
	}()
	log.Infof("wait jump to bastion host...")
	select {
	case <-readyChan:
	case err = <-errChan:
		return err
	}

	rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].Server = fmt.Sprintf("%s://%s", u.Scheme, local.String())
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: clientcmdlatest.Version, Kind: "Config"})

	convertedObj, err := latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return err
	}
	marshal, err := json.Marshal(convertedObj)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp("", "*.kubeconfig")
	if err != nil {
		return err
	}
	_ = temp.Close()
	err = os.WriteFile(temp.Name(), marshal, 0644)
	if err != nil {
		return err
	}
	_ = os.Chmod(temp.Name(), 0644)
	log.Infof("using temp kubeconfig %s", temp.Name())
	err = os.Setenv(clientcmd.RecommendedConfigPathEnvVar, temp.Name())
	return err
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
func (c *ConnectOptions) PreCheckResource() error {
	list, err := util.GetUnstructuredObjectList(c.factory, c.Namespace, c.Workloads)
	if err != nil {
		return err
	}
	var resources []string
	for _, info := range list {
		resources = append(resources, fmt.Sprintf("%s/%s", info.Mapping.GroupVersionKind.GroupKind().String(), info.Name))
	}
	c.Workloads = resources

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
			return err
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
						c.Workloads[i] = controller.UnsortedList()[0]
					}
				}
				// only a single service, not support it yet
				if controller.Len() == 0 {
					return fmt.Errorf("not support resources: %s", workload)
				}
			}
		}
	}
	return nil
}

func (c *ConnectOptions) GetRunningPodList() ([]v1.Pod, error) {
	list, err := c.clientset.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String(),
	})
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(list.Items); i++ {
		if list.Items[i].GetDeletionTimestamp() != nil || !util.AllContainerIsRunning(&list.Items[i]) {
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
func (c *ConnectOptions) GetCIDR(ctx context.Context) (err error) {
	// (1) get cidr from cache
	var value string
	value, err = c.dhcp.Get(ctx, config.KeyClusterIPv4POOLS)
	if err == nil {
		for _, s := range strings.Split(value, " ") {
			_, cidr, _ := net.ParseCIDR(s)
			if cidr != nil {
				c.cidrs = util.Deduplicate(append(c.cidrs, cidr))
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
		s := sets.New[string]()
		for _, cidr := range c.cidrs {
			s.Insert(cidr.String())
		}
		cidrs, _ := util.GetCIDRFromResourceUgly(c.clientset, c.Namespace)
		for _, cidr := range cidrs {
			s.Insert(cidr.String())
		}
		c.cidrs = util.Deduplicate(append(c.cidrs, cidrs...))
		_ = c.dhcp.Set(config.KeyClusterIPv4POOLS, strings.Join(s.UnsortedList(), " "))
		return
	}

	// (3) fallback to get cidr from node/pod/service
	c.cidrs, err = util.GetCIDRFromResourceUgly(c.clientset, c.Namespace)
	return
}
