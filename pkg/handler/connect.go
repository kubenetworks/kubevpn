package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/docker/distribution/reference"
	"github.com/google/gopacket/routing"
	goversion "github.com/hashicorp/go-version"
	netroute "github.com/libp2p/go-netroute"
	miekgdns "github.com/miekg/dns"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	pkgruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	pkgtypes "k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
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
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/set"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/utils/clock"
	"k8s.io/utils/pointer"
	pkgclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/dns"
	"github.com/wencaiwulue/kubevpn/pkg/driver"
	"github.com/wencaiwulue/kubevpn/pkg/tun"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type ConnectOptions struct {
	Namespace   string
	Headers     map[string]string
	Workloads   []string
	ExtraCIDR   []string
	ExtraDomain []string
	UseLocalDNS bool

	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
	factory    cmdutil.Factory
	cidrs      []*net.IPNet
	dhcp       *DHCPManager
	// needs to give it back to dhcp
	localTunIPv4 *net.IPNet
	localTunIPv6 *net.IPNet

	apiServerIPs []net.IP
}

func (c *ConnectOptions) createRemoteInboundPod(ctx context.Context) (err error) {
	c.localTunIPv4, c.localTunIPv6, err = c.dhcp.RentIPBaseNICAddress(ctx)
	if err != nil {
		return
	}

	for _, workload := range c.Workloads {
		configInfo := util.PodRouteConfig{
			LocalTunIPv4: c.localTunIPv4.IP.String(),
			LocalTunIPv6: c.localTunIPv6.IP.String(),
		}
		// means mesh mode
		if len(c.Headers) != 0 {
			err = InjectVPNAndEnvoySidecar(ctx, c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, workload, configInfo, c.Headers)
		} else {
			err = InjectVPNSidecar(ctx, c.factory, c.Namespace, workload, configInfo)
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
	c.dhcp = NewDHCPManager(c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace)
	if err = c.dhcp.initDHCP(ctx); err != nil {
		return
	}
	c.addCleanUpResourceHandler()
	if err = c.getCIDR(ctx); err != nil {
		return
	}
	if err = createOutboundPod(ctx, c.factory, c.clientset, c.Namespace); err != nil {
		return
	}
	if err = c.setImage(ctx); err != nil {
		return
	}
	if err = c.createRemoteInboundPod(ctx); err != nil {
		return
	}
	port := util.GetAvailableTCPPortOrDie()
	if err = c.portForward(ctx, fmt.Sprintf("%d:10800", port)); err != nil {
		return
	}
	if util.IsWindows() {
		driver.InstallWireGuardTunDriver()
	}
	forward := fmt.Sprintf("tcp://127.0.0.1:%d", port)
	if err = c.startLocalTunServe(ctx, forward); err != nil {
		return
	}
	if err = c.addRouteDynamic(ctx); err != nil {
		return
	}
	c.deleteFirewallRule(ctx)
	if err = c.setupDNS(); err != nil {
		return
	}
	if err = c.addExtraRoute(ctx); err != nil {
		return
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
		c.localTunIPv4.Mask = net.CIDRMask(0, 32)
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
	if err = os.Setenv(config.EnvInboundPodTunIPv6, c.localTunIPv6.String()); err != nil {
		return err
	}

	r := core.Route{
		ServeNodes: []string{
			fmt.Sprintf("tun:/127.0.0.1:8422?net=%s&route=%s", c.localTunIPv4.String(), strings.Join(list.UnsortedList(), ",")),
		},
		ChainNode: forwardAddress,
		Retries:   5,
	}

	log.Debugf("ipv4: %s, ipv6: %s", c.localTunIPv4.IP.String(), c.localTunIPv6.IP.String())
	servers, err := Parse(r)
	if err != nil {
		return errors.Wrap(err, "error while create tunnel")
	}
	go func() {
		log.Error(Run(ctx, servers))
		Cleanup(syscall.SIGQUIT)
	}()
	log.Info("tunnel connected")
	return
}

// Listen all pod, add route if needed
func (c *ConnectOptions) addRouteDynamic(ctx context.Context) (err error) {
	var r routing.Router
	r, err = netroute.New()
	if err != nil {
		return
	}

	var tunIface *net.Interface
	tunIface, err = tun.GetInterface()
	if err != nil {
		return
	}

	addRouteFunc := func(resource, ip string) {
		if net.ParseIP(ip) == nil {
			return
		}
		apiServer := sets.New[string]()
		for _, p := range c.apiServerIPs {
			apiServer.Insert(p.String())
		}
		// if pod ip or service ip is equal to apiServer ip, can not add it to route table
		if apiServer.Has(ip) {
			return
		}
		// if route is right, not need add route
		iface, _, _, errs := r.Route(net.ParseIP(ip))
		if errs == nil && tunIface.Name == iface.Name {
			return
		}
		var mask net.IPMask
		if net.ParseIP(ip).To4() != nil {
			mask = net.CIDRMask(32, 32)
		} else {
			mask = net.CIDRMask(128, 128)
		}
		errs = tun.AddRoutes(types.Route{Dst: net.IPNet{IP: net.ParseIP(ip), Mask: mask}})
		if errs != nil {
			log.Debugf("[route] add route failed, resource: %s, ip: %s,err: %v", resource, ip, err)
		}
	}

	manager := wait.NewExponentialBackoffManager(800*time.Millisecond, 30*time.Second, 2*time.Minute, 2.0, 1.0, clock.RealClock{})

	var podList *v1.PodList
	podList, err = c.clientset.CoreV1().Pods(v1.NamespaceAll).List(ctx, metav1.ListOptions{TimeoutSeconds: pointer.Int64(30)})
	if err != nil {
		log.Debugf("list pod failed, err: %v", err)
		return
	}

	for _, pod := range podList.Items {
		if pod.Spec.HostNetwork {
			continue
		}
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			continue
		}
		addRouteFunc(pod.Name, pod.Status.PodIP)
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
					w, errs := c.clientset.CoreV1().Pods(v1.NamespaceAll).Watch(ctx, metav1.ListOptions{
						Watch: true, TimeoutSeconds: pointer.Int64(30), ResourceVersion: podList.ResourceVersion,
					})
					if errs != nil {
						if utilnet.IsConnectionRefused(errs) || apierrors.IsTooManyRequests(errs) {
							<-manager.Backoff().C()
							return
						}
						time.Sleep(time.Second * 5)
						log.Debugf("wait pod failed, err: %v", errs)
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

	var serviceList *v1.ServiceList
	serviceList, err = c.clientset.CoreV1().Services(v1.NamespaceAll).List(ctx, metav1.ListOptions{
		TimeoutSeconds: pointer.Int64(30),
	})
	if err != nil {
		err = fmt.Errorf("can not list service to add it to route table, err: %v", err)
		return
	}
	for _, item := range serviceList.Items {
		addRouteFunc(item.Name, item.Spec.ClusterIP)
	}

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
					w, errs := c.clientset.CoreV1().Services(v1.NamespaceAll).Watch(ctx, metav1.ListOptions{
						Watch: true, TimeoutSeconds: pointer.Int64(30), ResourceVersion: serviceList.ResourceVersion,
					})
					if errs != nil {
						if utilnet.IsConnectionRefused(errs) || apierrors.IsTooManyRequests(errs) {
							<-manager.Backoff().C()
							return
						}
						log.Debugf("wait service failed, err: %v", errs)
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
							var svc *v1.Service
							svc, ok = e.Object.(*v1.Service)
							if !ok {
								continue
							}
							ip := svc.Spec.ClusterIP
							addRouteFunc(svc.Name, ip)
						}
					}
				}()
			}
		}
	}()

	return
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
	if err = dns.SetupDNS(relovConf, ns.UnsortedList(), c.UseLocalDNS); err != nil {
		return err
	}
	// dump service in current namespace for support DNS resolve service:port
	go dns.AddServiceNameToHosts(ctx, c.clientset.CoreV1().Services(c.Namespace))
	return nil
}

func Run(ctx context.Context, servers []core.Server) error {
	group, _ := errgroup.WithContext(ctx)
	for i := range servers {
		i := i
		group.Go(func() error {
			l := servers[i].Listener
			defer l.Close()
			for {
				select {
				case <-ctx.Done():
					return nil
				default:
				}

				conn, errs := l.Accept()
				if errs != nil {
					log.Debugf("server accept connect error: %v", errs)
					continue
				}
				go servers[i].Handler.Handle(ctx, conn)
			}
		})
	}
	return group.Wait()
}

func Parse(r core.Route) ([]core.Server, error) {
	servers, err := r.GenerateServers()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("server is empty, server config: %s", strings.Join(r.ServeNodes, ","))
	}
	return servers, nil
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

	if conf.RemoteKubeconfig != "" || flags.Changed("remote-kubeconfig") {
		var stdOut []byte
		var errOut []byte
		if len(conf.RemoteKubeconfig) != 0 && conf.RemoteKubeconfig[0] == '~' {
			conf.RemoteKubeconfig = filepath.Join("/", conf.User, conf.RemoteKubeconfig[1:])
		}
		if conf.RemoteKubeconfig == "" {
			// if `--remote-kubeconfig` is parsed then Entrypoint is reset
			conf.RemoteKubeconfig = filepath.Join("/", conf.User, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)
		}
		stdOut, errOut, err = util.Run(conf,
			fmt.Sprintf("sh -c 'kubectl config view --flatten --raw --kubeconfig %s || minikube kubectl -- config view --flatten --raw --kubeconfig %s'",
				conf.RemoteKubeconfig,
				conf.RemoteKubeconfig),
			[]string{clientcmd.RecommendedConfigPathEnvVar, conf.RemoteKubeconfig},
		)
		if err != nil {
			return errors.Wrap(err, string(errOut))
		}
		if len(stdOut) == 0 {
			return errors.Errorf("can not get kubeconfig %s from remote ssh server: %s", conf.RemoteKubeconfig, string(errOut))
		}

		var temp *os.File
		if temp, err = os.CreateTemp("", "kubevpn"); err != nil {
			return err
		}
		if err = temp.Close(); err != nil {
			return err
		}
		if err = os.WriteFile(temp.Name(), stdOut, 0644); err != nil {
			return err
		}
		if err = os.Chmod(temp.Name(), 0644); err != nil {
			return err
		}
		configFlags.KubeConfig = pointer.String(temp.Name())
	} else {
		if flags != nil {
			lookup := flags.Lookup("kubeconfig")
			if lookup != nil && lookup.Value != nil && lookup.Value.String() != "" {
				configFlags.KubeConfig = pointer.String(lookup.Value.String())
			}
		}
	}
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	rawConfig, err := matchVersionFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return err
	}
	if err = api.FlattenConfig(&rawConfig); err != nil {
		return err
	}
	if rawConfig.Contexts == nil {
		return errors.New("kubeconfig is invalid")
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		return errors.New("kubeconfig is invalid")
	}
	cluster := rawConfig.Clusters[kubeContext.Cluster]
	if cluster == nil {
		return errors.New("kubeconfig is invalid")
	}
	u, err := url.Parse(cluster.Server)
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
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.WriteFile(temp.Name(), marshal, 0644); err != nil {
		return err
	}
	if err = os.Chmod(temp.Name(), 0644); err != nil {
		return err
	}
	log.Infof("using temp kubeconfig %s", temp.Name())
	err = os.Setenv(clientcmd.RecommendedConfigPathEnvVar, temp.Name())
	if err != nil {
		return err
	}
	return os.Setenv(config.EnvSSHJump, temp.Name())
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

// getCIDR
// 1: get pod cidr
// 2: get service cidr
// todo optimize code should distinguish service cidr and pod cidr
// https://stackoverflow.com/questions/45903123/kubernetes-set-service-cidr-and-pod-cidr-the-same
// https://stackoverflow.com/questions/44190607/how-do-you-find-the-cluster-service-cidr-of-a-kubernetes-cluster/54183373#54183373
// https://stackoverflow.com/questions/44190607/how-do-you-find-the-cluster-service-cidr-of-a-kubernetes-cluster
func (c *ConnectOptions) getCIDR(ctx context.Context) (err error) {
	defer func() {
		if err == nil {
			u, err2 := url.Parse(c.config.Host)
			if err2 != nil {
				return
			}
			host, _, err3 := net.SplitHostPort(u.Host)
			if err3 != nil {
				return
			}
			var ipList []net.IP
			if ip := net.ParseIP(host); ip != nil {
				ipList = append(ipList, ip)
			}
			ips, _ := net.LookupIP(host)
			if ips != nil {
				ipList = append(ipList, ips...)
			}
			c.apiServerIPs = ipList
			for i := 0; i < len(c.cidrs); i++ {
				for _, ip := range ipList {
					if c.cidrs[i].Contains(ip) {
						c.cidrs = append(c.cidrs[:i], c.cidrs[i+1:]...)
						i--
					}
				}
			}
		}
	}()

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

func (c *ConnectOptions) addExtraRoute(ctx context.Context) (err error) {
	if len(c.ExtraDomain) == 0 {
		return
	}
	var ips []string
	ips, err = dns.GetDNSIPFromDnsPod(c.clientset)
	if err != nil {
		return
	}
	if len(ips) == 0 {
		err = fmt.Errorf("can't found any dns server")
		return
	}

	var r routing.Router
	r, err = netroute.New()
	if err != nil {
		return
	}

	var tunIface *net.Interface
	tunIface, err = tun.GetInterface()
	if err != nil {
		return
	}

	addRouteFunc := func(resource, ip string) {
		if net.ParseIP(ip) == nil {
			return
		}
		// if route is right, not need add route
		iface, _, _, errs := r.Route(net.ParseIP(ip))
		if errs == nil && tunIface.Name == iface.Name {
			return
		}
		var mask net.IPMask
		if net.ParseIP(ip).To4() != nil {
			mask = net.CIDRMask(32, 32)
		} else {
			mask = net.CIDRMask(128, 128)
		}
		errs = tun.AddRoutes(types.Route{Dst: net.IPNet{IP: net.ParseIP(ip), Mask: mask}})
		if errs != nil {
			log.Debugf("[route] add route failed, domain: %s, ip: %s,err: %v", resource, ip, err)
		}
	}

	client := &miekgdns.Client{Net: "udp", SingleInflight: true, DialTimeout: time.Second * 30}
	for _, domain := range c.ExtraDomain {
		for _, qType := range []uint16{miekgdns.TypeA, miekgdns.TypeAAAA} {
			err = retry.OnError(
				retry.DefaultRetry,
				func(err error) bool {
					return err != nil
				},
				func() error {
					var answer *miekgdns.Msg
					answer, _, err = client.ExchangeContext(ctx, &miekgdns.Msg{
						MsgHdr: miekgdns.MsgHdr{
							Id: uint16(rand.Intn(math.MaxUint16 + 1)),
						},
						Question: []miekgdns.Question{
							{
								Name:  domain + ".",
								Qtype: qType,
							},
						},
					}, fmt.Sprintf("%s:%d", ips[0], 53))
					if err != nil {
						return err
					}
					for _, rr := range answer.Answer {
						switch a := rr.(type) {
						case *miekgdns.A:
							addRouteFunc(domain, a.A.String())
						case *miekgdns.AAAA:
							addRouteFunc(domain, a.AAAA.String())
						}
					}
					return nil
				})
			if err != nil {
				return err
			}
		}
	}
	return
}

func (c *ConnectOptions) GetKubeconfigPath() (string, error) {
	rawConfig, err := c.factory.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return "", err
	}
	err = api.FlattenConfig(&rawConfig)
	if err != nil {
		return "", err
	}
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: clientcmdlatest.Version, Kind: "Config"})
	var convertedObj pkgruntime.Object
	convertedObj, err = latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return "", err
	}
	var kubeconfigJsonBytes []byte
	kubeconfigJsonBytes, err = json.Marshal(convertedObj)
	if err != nil {
		return "", err
	}

	temp, err := os.CreateTemp("", "*.kubeconfig")
	if err != nil {
		return "", err
	}
	temp.Close()
	err = os.WriteFile(temp.Name(), kubeconfigJsonBytes, 0644)
	if err != nil {
		return "", err
	}
	err = os.Chmod(temp.Name(), 0644)
	if err != nil {
		return "", err
	}

	return temp.Name(), nil
}

func (c ConnectOptions) GetClientset() *kubernetes.Clientset {
	return c.clientset
}

// update to newer image
func (c *ConnectOptions) UpdateImage(ctx context.Context) error {
	deployment, err := c.clientset.AppsV1().Deployments(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	origin := deployment.DeepCopy()
	newImg, err := reference.ParseNormalizedNamed(config.Image)
	if err != nil {
		return err
	}
	newTag, ok := newImg.(reference.NamedTagged)
	if !ok {
		return nil
	}
	oldImg, err := reference.ParseNormalizedNamed(deployment.Spec.Template.Spec.Containers[0].Image)
	if err != nil {
		return err
	}
	var oldTag reference.NamedTagged
	oldTag, ok = oldImg.(reference.NamedTagged)
	if !ok {
		return nil
	}
	if reference.Domain(newImg) != reference.Domain(oldImg) {
		return nil
	}
	var oldVersion, newVersion *goversion.Version
	oldVersion, err = goversion.NewVersion(oldTag.Tag())
	if err != nil {
		return nil
	}
	newVersion, err = goversion.NewVersion(newTag.Tag())
	if err != nil {
		return nil
	}
	if oldVersion.GreaterThanOrEqual(newVersion) {
		return nil
	}

	log.Infof("found newer image %s, set image from %s to it...", config.Image, deployment.Spec.Template.Spec.Containers[0].Image)
	for i := range deployment.Spec.Template.Spec.Containers {
		deployment.Spec.Template.Spec.Containers[i].Image = config.Image
	}
	p := pkgclient.MergeFrom(deployment)
	data, err := pkgclient.MergeFrom(origin).Data(deployment)
	if err != nil {
		return err
	}
	_, err = c.clientset.AppsV1().Deployments(c.Namespace).Patch(ctx, config.ConfigMapPodTrafficManager, p.Type(), data, metav1.PatchOptions{})
	if err != nil {
		return err
	}
	err = util.RolloutStatus(ctx, c.factory, c.Namespace, fmt.Sprintf("deployments/%s", config.ConfigMapPodTrafficManager), time.Minute*60)
	return err
}

func (c *ConnectOptions) setImage(ctx context.Context) error {
	deployment, err := c.clientset.AppsV1().Deployments(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	newImg, err := reference.ParseNormalizedNamed(config.Image)
	if err != nil {
		return err
	}
	newTag, ok := newImg.(reference.NamedTagged)
	if !ok {
		return nil
	}

	oldImg, err := reference.ParseNormalizedNamed(deployment.Spec.Template.Spec.Containers[0].Image)
	if err != nil {
		return err
	}
	var oldTag reference.NamedTagged
	oldTag, ok = oldImg.(reference.NamedTagged)
	if !ok {
		return nil
	}
	if reference.Domain(newImg) != reference.Domain(oldImg) {
		return nil
	}
	var oldVersion, newVersion *goversion.Version
	oldVersion, err = goversion.NewVersion(oldTag.Tag())
	if err != nil {
		return nil
	}
	newVersion, err = goversion.NewVersion(newTag.Tag())
	if err != nil {
		return nil
	}
	if oldVersion.GreaterThanOrEqual(newVersion) {
		return nil
	}
	log.Infof("found newer image %s, set image from %s to it...", config.Image, deployment.Spec.Template.Spec.Containers[0].Image)

	r := c.factory.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(c.Namespace).DefaultNamespace().
		ResourceNames("deployments", deployment.Name).
		ContinueOnError().
		Latest().
		Flatten().
		Do()
	if err = r.Err(); err != nil {
		return err
	}
	infos, err := r.Infos()
	if err != nil {
		return err
	}
	patches := set.CalculatePatches(infos, scheme.DefaultJSONEncoder(), func(obj pkgruntime.Object) ([]byte, error) {
		_, err = polymorphichelpers.UpdatePodSpecForObjectFn(obj, func(spec *v1.PodSpec) error {
			for i := range spec.Containers {
				spec.Containers[i].Image = config.Image
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return pkgruntime.Encode(scheme.DefaultJSONEncoder(), obj)
	})

	if err != nil {
		return err
	}
	for _, p := range patches {
		_, err = resource.
			NewHelper(p.Info.Client, p.Info.Mapping).
			DryRun(false).
			Patch(p.Info.Namespace, p.Info.Name, pkgtypes.StrategicMergePatchType, p.Patch, nil)
		if err != nil {
			return fmt.Errorf("failed to patch image update to pod template: %v", err)
		}
		err = util.RolloutStatus(ctx, c.factory, c.Namespace, fmt.Sprintf("%s/%s", p.Info.Mapping.Resource.GroupResource().String(), p.Info.Name), time.Minute*60)
		if err != nil {
			return err
		}
	}
	return nil
}
