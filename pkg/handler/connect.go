package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
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
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/metadata"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	pkgruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	pkgtypes "k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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
	Namespace            string
	Headers              map[string]string
	Workloads            []string
	ExtraCIDR            []string
	ExtraDomain          []string
	UseLocalDNS          bool
	Engine               config.Engine
	Foreground           bool
	OriginKubeconfigPath string

	ctx    context.Context
	cancel context.CancelFunc

	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
	factory    cmdutil.Factory
	cidrs      []*net.IPNet
	dhcp       *DHCPManager
	// needs to give it back to dhcp
	localTunIPv4     *net.IPNet
	localTunIPv6     *net.IPNet
	rollbackFuncList []func() error
	dnsConfig        *dns.Config

	apiServerIPs []net.IP
	extraHost    []dns.Entry
}

func (c *ConnectOptions) Context() context.Context {
	if c.ctx == nil {
		c.ctx, c.cancel = context.WithCancel(context.Background())
	}
	return c.ctx
}

func (c *ConnectOptions) InitDHCP(ctx context.Context) error {
	c.dhcp = NewDHCPManager(c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace)
	err := c.dhcp.initDHCP(ctx)
	return err
}

func (c *ConnectOptions) RentInnerIP(ctx context.Context) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		ipv4s := md.Get(config.HeaderIPv4)
		if len(ipv4s) != 0 {
			ip, ipNet, err := net.ParseCIDR(ipv4s[0])
			if err == nil {
				c.localTunIPv4 = &net.IPNet{IP: ip, Mask: ipNet.Mask}
				log.Debugf("get ipv4 %s from context", c.localTunIPv4.String())
			}
		}
		ipv6s := md.Get(config.HeaderIPv6)
		if len(ipv6s) != 0 {
			ip, ipNet, err := net.ParseCIDR(ipv6s[0])
			if err == nil {
				c.localTunIPv6 = &net.IPNet{IP: ip, Mask: ipNet.Mask}
				log.Debugf("get ipv6 %s from context", c.localTunIPv6.String())
			}
		}
	}
	if c.dhcp == nil {
		if err := c.InitDHCP(ctx); err != nil {
			return nil, err
		}
	}

	var err error
	if c.localTunIPv4 == nil || c.localTunIPv6 == nil {
		c.localTunIPv4, c.localTunIPv6, err = c.dhcp.RentIPBaseNICAddress(ctx)
		if err != nil {
			return nil, err
		}
		ctx = metadata.AppendToOutgoingContext(ctx,
			config.HeaderIPv4, c.localTunIPv4.String(),
			config.HeaderIPv6, c.localTunIPv6.String(),
		)
	}
	return ctx, nil
}

func (c *ConnectOptions) CreateRemoteInboundPod(ctx context.Context) (err error) {
	if c.localTunIPv4 == nil || c.localTunIPv6 == nil {
		return fmt.Errorf("local tun ip is invalid")
	}

	for _, workload := range c.Workloads {
		log.Infof("start to create remote inbound pod for %s", workload)
		configInfo := util.PodRouteConfig{
			LocalTunIPv4: c.localTunIPv4.IP.String(),
			LocalTunIPv6: c.localTunIPv6.IP.String(),
		}
		// todo consider to use ephemeral container
		// https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/
		// means mesh mode
		if len(c.Headers) != 0 {
			err = InjectVPNAndEnvoySidecar(ctx, c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, workload, configInfo, c.Headers)
		} else {
			err = InjectVPNSidecar(ctx, c.factory, c.Namespace, workload, configInfo)
		}
		if err != nil {
			log.Errorf("create remote inbound pod for %s failed: %s", workload, err.Error())
			return err
		}
		log.Infof("create remote inbound pod for %s successfully", workload)
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

func (c *ConnectOptions) DoConnect(ctx context.Context, isLite bool) (err error) {
	c.ctx, c.cancel = context.WithCancel(ctx)

	log.Info("start to connect")
	if err = c.InitDHCP(c.ctx); err != nil {
		log.Errorf("init dhcp failed: %s", err.Error())
		return
	}
	c.addCleanUpResourceHandler()
	if err = c.getCIDR(c.ctx); err != nil {
		log.Errorf("get cidr failed: %s", err.Error())
		return
	}
	log.Info("get cidr successfully")
	if err = createOutboundPod(c.ctx, c.factory, c.clientset, c.Namespace); err != nil {
		return
	}
	if err = c.setImage(c.ctx); err != nil {
		return
	}
	//if err = c.CreateRemoteInboundPod(c.ctx); err != nil {
	//	return
	//}
	var rawTCPForwardPort, gvisorTCPForwardPort, gvisorUDPForwardPort int
	rawTCPForwardPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return err
	}
	gvisorTCPForwardPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return err
	}
	gvisorUDPForwardPort, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return err
	}
	if err = c.portForward(c.ctx, []string{
		fmt.Sprintf("%d:10800", rawTCPForwardPort),
		fmt.Sprintf("%d:10801", gvisorTCPForwardPort),
		fmt.Sprintf("%d:10802", gvisorUDPForwardPort),
	}); err != nil {
		return
	}
	if util.IsWindows() {
		driver.InstallWireGuardTunDriver()
	}
	forward := fmt.Sprintf("tcp://127.0.0.1:%d", rawTCPForwardPort)
	core.GvisorTCPForwardAddr = fmt.Sprintf("tcp://127.0.0.1:%d", gvisorTCPForwardPort)
	core.GvisorUDPForwardAddr = fmt.Sprintf("tcp://127.0.0.1:%d", gvisorUDPForwardPort)
	if err = c.startLocalTunServe(c.ctx, forward, isLite); err != nil {
		log.Errorf("start local tun service failed: %v", err)
		return
	}
	log.Infof("adding route...")
	if err = c.addRouteDynamic(c.ctx); err != nil {
		log.Errorf("add route dynamic failed: %v", err)
		return
	}
	c.deleteFirewallRule(c.ctx)
	if err = c.addExtraRoute(c.ctx); err != nil {
		log.Errorf("add extra route failed: %v", err)
		return
	}
	if err = c.setupDNS(c.ctx, isLite); err != nil {
		log.Errorf("set up dns failed: %v", err)
		return
	}
	go c.heartbeats(c.ctx)
	log.Info("dns service ok")
	return
}

// detect pod is delete event, if pod is deleted, needs to redo port-forward immediately
func (c *ConnectOptions) portForward(ctx context.Context, portPair []string) error {
	var readyChan = make(chan struct{}, 1)
	var errChan = make(chan error, 1)
	podInterface := c.clientset.CoreV1().Pods(c.Namespace)
	go func() {
		var first = pointer.Bool(true)
		for {
			func() {
				podList, err := c.GetRunningPodList(ctx)
				if err != nil {
					time.Sleep(time.Second * 2)
					return
				}
				childCtx, cancelFunc := context.WithCancel(ctx)
				defer cancelFunc()
				if !*first {
					readyChan = nil
				}
				podName := podList[0].GetName()
				// if port-forward occurs error, check pod is deleted or not, speed up fail
				utilruntime.ErrorHandlers = []func(error){func(err error) {
					if !strings.Contains(err.Error(), "an error occurred forwarding") {
						log.Debugf("port-forward occurs error, err: %v, retrying", err)
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
					portPair,
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
					log.Errorf("port %s already in use, needs to release it manually", portPair)
					time.Sleep(time.Second * 1)
				} else {
					log.Debugf("port-forward occurs error, err: %v, retrying", err)
					time.Sleep(time.Millisecond * 500)
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

func (c *ConnectOptions) startLocalTunServe(ctx context.Context, forwardAddress string, lite bool) (err error) {
	// todo figure it out why
	if util.IsWindows() {
		c.localTunIPv4.Mask = net.CIDRMask(0, 32)
	}
	var list = sets.New[string]()
	if !lite {
		list.Insert(config.CIDR.String())
	}
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
			fmt.Sprintf("tun:/127.0.0.1:8422?net=%s&route=%s&%s=%s",
				c.localTunIPv4.String(),
				strings.Join(list.UnsortedList(), ","),
				config.ConfigKubeVPNTransportEngine,
				string(c.Engine),
			),
		},
		ChainNode: forwardAddress,
		Retries:   5,
	}

	log.Debugf("ipv4: %s, ipv6: %s", c.localTunIPv4.IP.String(), c.localTunIPv6.IP.String())
	servers, err := Parse(r)
	if err != nil {
		log.Errorf("parse route error: %v", err)
		return err
	}
	go func() {
		log.Error(Run(ctx, servers))
		c.Cleanup()
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

	var tunName string
	tunName, err = c.GetTunDeviceName()
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
		if errs == nil && tunName == iface.Name {
			return
		}
		var mask net.IPMask
		if net.ParseIP(ip).To4() != nil {
			mask = net.CIDRMask(32, 32)
		} else {
			mask = net.CIDRMask(128, 128)
		}
		errs = tun.AddRoutes(tunName, types.Route{Dst: net.IPNet{IP: net.ParseIP(ip), Mask: mask}})
		if errs != nil {
			log.Debugf("[route] add route failed, resource: %s, ip: %s,err: %v", resource, ip, err)
		}
	}

	manager := wait.NewExponentialBackoffManager(800*time.Millisecond, 30*time.Second, 2*time.Minute, 2.0, 1.0, clock.RealClock{})

	var podNs string
	var podList *v1.PodList
	for _, n := range []string{v1.NamespaceAll, c.Namespace} {
		podList, err = c.clientset.CoreV1().Pods(n).List(ctx, metav1.ListOptions{TimeoutSeconds: pointer.Int64(30)})
		if err != nil {
			continue
		}
		podNs = n
		break
	}
	if err != nil {
		log.Debugf("list pod failed, err: %v", err)
		return
	}

	for _, pod := range podList.Items {
		if pod.Spec.HostNetwork {
			continue
		}
		go addRouteFunc(pod.Name, pod.Status.PodIP)
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
							log.Error(er)
						}
					}()
					w, errs := c.clientset.CoreV1().Pods(podNs).Watch(ctx, metav1.ListOptions{
						Watch: true, ResourceVersion: podList.ResourceVersion,
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
							var pod *v1.Pod
							pod, ok = e.Object.(*v1.Pod)
							if !ok {
								continue
							}
							if pod.Spec.HostNetwork {
								continue
							}
							go addRouteFunc(pod.Name, pod.Status.PodIP)
						}
					}
				}()
			}
		}
	}()

	var svcNs string
	var serviceList *v1.ServiceList
	for _, n := range []string{v1.NamespaceAll, c.Namespace} {
		serviceList, err = c.clientset.CoreV1().Services(n).List(ctx, metav1.ListOptions{
			TimeoutSeconds: pointer.Int64(30),
		})
		if err != nil {
			continue
		}
		svcNs = n
		break
	}
	if err != nil {
		err = fmt.Errorf("can not list service to add it to route table, err: %v", err)
		return
	}
	for _, item := range serviceList.Items {
		go addRouteFunc(item.Name, item.Spec.ClusterIP)
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
					w, errs := c.clientset.CoreV1().Services(svcNs).Watch(ctx, metav1.ListOptions{
						Watch: true, ResourceVersion: serviceList.ResourceVersion,
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
							var svc *v1.Service
							svc, ok = e.Object.(*v1.Service)
							if !ok {
								continue
							}
							ip := svc.Spec.ClusterIP
							go addRouteFunc(svc.Name, ip)
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
	c.AddRolloutFunc(func() error {
		util.DeleteAllowFirewallRule()
		return nil
	})
	go util.DeleteBlockFirewallRule(ctx)
}

func (c *ConnectOptions) setupDNS(ctx context.Context, lite bool) error {
	const port = 53
	pod, err := c.GetRunningPodList(ctx)
	if err != nil {
		log.Errorf("get running pod list failed, err: %v", err)
		return err
	}
	relovConf, err := util.GetDNSServiceIPFromPod(c.clientset, c.restclient, c.config, pod[0].GetName(), c.Namespace)
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
	tunName, err := c.GetTunDeviceName()
	if err != nil {
		return err
	}
	c.dnsConfig = &dns.Config{
		Config:      relovConf,
		Ns:          ns.UnsortedList(),
		UseLocalDNS: c.UseLocalDNS,
		TunName:     tunName,
		Lite:        lite,
		Hosts:       c.extraHost,
	}
	if err = c.dnsConfig.SetupDNS(); err != nil {
		return err
	}
	// dump service in current namespace for support DNS resolve service:port
	c.dnsConfig.AddServiceNameToHosts(ctx, c.clientset.CoreV1().Services(c.Namespace), c.extraHost...)
	return nil
}

func Run(ctx context.Context, servers []core.Server) error {
	group, ctx := errgroup.WithContext(ctx)
	for i := range servers {
		i := i
		group.Go(func() error {
			l := servers[i].Listener
			defer l.Close()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
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
		return nil, err
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

func SshJump(ctx context.Context, conf *util.SshConfig, flags *pflag.FlagSet, print bool) (path string, err error) {
	if conf.Addr == "" && conf.ConfigAlias == "" {
		if flags != nil {
			lookup := flags.Lookup("kubeconfig")
			if lookup != nil {
				if lookup.Value != nil && lookup.Value.String() != "" {
					path = lookup.Value.String()
				} else if lookup.DefValue != "" {
					path = lookup.DefValue
				} else {
					path = lookup.NoOptDefVal
				}
			}
		}
		return
	}
	defer func() {
		if er := recover(); er != nil {
			err = er.(error)
		}
	}()

	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()

	if conf.RemoteKubeconfig != "" || (flags != nil && flags.Changed("remote-kubeconfig")) {
		var stdOut []byte
		var errOut []byte
		if len(conf.RemoteKubeconfig) != 0 && conf.RemoteKubeconfig[0] == '~' {
			conf.RemoteKubeconfig = filepath.Join("/", conf.User, conf.RemoteKubeconfig[1:])
		}
		if conf.RemoteKubeconfig == "" {
			// if `--remote-kubeconfig` is parsed then Entrypoint is reset
			conf.RemoteKubeconfig = filepath.Join("/", conf.User, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)
		}
		stdOut, errOut, err = util.RemoteRun(conf,
			fmt.Sprintf("sh -c 'kubectl config view --flatten --raw --kubeconfig %s || minikube kubectl -- config view --flatten --raw --kubeconfig %s'",
				conf.RemoteKubeconfig,
				conf.RemoteKubeconfig),
			map[string]string{clientcmd.RecommendedConfigPathEnvVar: conf.RemoteKubeconfig},
		)
		if err != nil {
			err = errors.Wrap(err, string(errOut))
			return
		}
		if len(stdOut) == 0 {
			err = errors.Errorf("can not get kubeconfig %s from remote ssh server: %s", conf.RemoteKubeconfig, string(errOut))
			return
		}

		var temp *os.File
		if temp, err = os.CreateTemp("", "kubevpn"); err != nil {
			return
		}
		if err = temp.Close(); err != nil {
			return
		}
		if err = os.WriteFile(temp.Name(), stdOut, 0644); err != nil {
			return
		}
		if err = os.Chmod(temp.Name(), 0644); err != nil {
			return
		}
		configFlags.KubeConfig = pointer.String(temp.Name())
	} else {
		if flags != nil {
			lookup := flags.Lookup("kubeconfig")
			if lookup != nil {
				if lookup.Value != nil && lookup.Value.String() != "" {
					configFlags.KubeConfig = pointer.String(lookup.Value.String())
				} else if lookup.DefValue != "" {
					configFlags.KubeConfig = pointer.String(lookup.DefValue)
				}
			}
		}
	}
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	var rawConfig api.Config
	rawConfig, err = matchVersionFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return
	}
	if err = api.FlattenConfig(&rawConfig); err != nil {
		return
	}
	if rawConfig.Contexts == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	cluster := rawConfig.Clusters[kubeContext.Cluster]
	if cluster == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	var u *url.URL
	u, err = url.Parse(cluster.Server)
	if err != nil {
		return
	}

	serverHost := u.Hostname()
	serverPort := u.Port()
	if serverPort == "" {
		if u.Scheme == "https" {
			serverPort = "443"
		} else if u.Scheme == "http" {
			serverPort = "80"
		} else {
			// handle other schemes if necessary
			err = errors.New("kubeconfig is invalid: wrong protocol")
			return
		}
	}
	ips, err := net.LookupHost(serverHost)
	if err != nil {
		return
	}

	if len(ips) == 0 {
		// handle error: no IP associated with the hostname
		err = fmt.Errorf("kubeconfig: no IP associated with the hostname %s", serverHost)
		return
	}

	var remote netip.AddrPort
	// Use the first IP address
	remote, err = netip.ParseAddrPort(net.JoinHostPort(ips[0], serverPort))
	if err != nil {
		return
	}

	var port int
	port, err = util.GetAvailableTCPPortOrDie()
	if err != nil {
		return
	}
	var local netip.AddrPort
	local, err = netip.ParseAddrPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return
	}

	// pre-check network ip connect
	var cli *ssh.Client
	cli, err = util.DialSshRemote(conf)
	if err != nil {
		return
	} else {
		_ = cli.Close()
	}
	errChan := make(chan error, 1)
	readyChan := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			err := util.Main(ctx, remote, local, conf, readyChan)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Errorf("ssh forward failed err: %v", err)
				}
				select {
				case errChan <- err:
				default:
				}
			}
		}
	}()
	if print {
		log.Infof("wait jump to bastion host...")
	}
	select {
	case <-readyChan:
	case err = <-errChan:
		log.Errorf("ssh proxy err: %v", err)
		return
	}

	rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].Server = fmt.Sprintf("%s://%s", u.Scheme, local.String())
	rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].TLSServerName = serverHost
	// To Do: add cli option to skip tls verify
	// rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].CertificateAuthorityData = nil
	// rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].InsecureSkipTLSVerify = true
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: clientcmdlatest.Version, Kind: "Config"})

	var convertedObj runtime.Object
	convertedObj, err = latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return
	}
	var marshal []byte
	marshal, err = json.Marshal(convertedObj)
	if err != nil {
		return
	}
	var temp *os.File
	temp, err = os.CreateTemp("", "*.kubeconfig")
	if err != nil {
		return
	}
	if err = temp.Close(); err != nil {
		return
	}
	if err = os.WriteFile(temp.Name(), marshal, 0644); err != nil {
		return
	}
	if err = os.Chmod(temp.Name(), 0644); err != nil {
		return
	}
	if print {
		msg := fmt.Sprintf("| To use: export KUBECONFIG=%s |", temp.Name())
		printLine(msg)
		log.Infof(msg)
		printLine(msg)
	}
	path = temp.Name()
	return
}
func printLine(msg string) {
	line := "+" + strings.Repeat("-", len(msg)-2) + "+"
	log.Infof(line)
}

func SshJumpAndSetEnv(ctx context.Context, conf *util.SshConfig, flags *pflag.FlagSet, print bool) error {
	if conf.Addr == "" && conf.ConfigAlias == "" {
		return nil
	}
	path, err := SshJump(ctx, conf, flags, print)
	if err != nil {
		return err
	}
	err = os.Setenv(clientcmd.RecommendedConfigPathEnvVar, path)
	if err != nil {
		return err
	}
	return os.Setenv(config.EnvSSHJump, path)
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
	if len(c.Workloads) == 0 {
		return nil
	}

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

func (c *ConnectOptions) GetRunningPodList(ctx context.Context) ([]v1.Pod, error) {
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

func (c *ConnectOptions) addExtraRoute(ctx context.Context) error {
	if len(c.ExtraDomain) == 0 {
		return nil
	}
	ips, err := util.GetDNSIPFromDnsPod(c.clientset)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		err = fmt.Errorf("can't found any dns server")
		return err
	}

	var r routing.Router
	r, err = netroute.New()
	if err != nil {
		return err
	}

	var tunName string
	tunName, err = c.GetTunDeviceName()
	if err != nil {
		log.Errorf("get tun interface failed: %s", err.Error())
		return err
	}

	addRouteFunc := func(resource, ip string) {
		if net.ParseIP(ip) == nil {
			return
		}
		// if route is right, not need add route
		iface, _, _, errs := r.Route(net.ParseIP(ip))
		if errs == nil && tunName == iface.Name {
			return
		}
		var mask net.IPMask
		if net.ParseIP(ip).To4() != nil {
			mask = net.CIDRMask(32, 32)
		} else {
			mask = net.CIDRMask(128, 128)
		}
		errs = tun.AddRoutes(tunName, types.Route{Dst: net.IPNet{IP: net.ParseIP(ip), Mask: mask}})
		if errs != nil {
			log.Debugf("[route] add route failed, domain: %s, ip: %s,err: %v", resource, ip, err)
		}
	}

	// add dns pod ip to route
	for _, ip := range ips {
		addRouteFunc("dns-pod", ip)
	}

	// 1) use dig +short query, if ok, just return
	podList, err := c.GetRunningPodList(ctx)
	if err != nil {
		return err
	}
	for _, domain := range c.ExtraDomain {
		ip, err := util.Shell(c.clientset, c.restclient, c.config, podList[0].Name, config.ContainerSidecarVPN, c.Namespace, []string{"dig", "+short", domain})
		if err != nil || net.ParseIP(ip) == nil {
			goto RetryWithDNSClient
		}
		addRouteFunc(domain, ip)
		c.extraHost = append(c.extraHost, dns.Entry{IP: net.ParseIP(ip).String(), Domain: domain})
	}
	return nil

RetryWithDNSClient:
	// 2) wait until can ping dns server ip ok
	ctx2, cancelFunc := context.WithTimeout(ctx, time.Second*10)
	wait.UntilWithContext(ctx2, func(context.Context) {
		for _, ip := range ips {
			pong, err2 := util.Ping(ip)
			if err2 == nil && pong {
				ips = []string{ip}
				cancelFunc()
				return
			}
		}
	}, time.Millisecond*50)

	// 3) use nslookup to query dns at first, it will speed up mikdns query process
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	go func() {
		for _, domain := range c.ExtraDomain {
			go func(domain string) {
				for ; true; <-ticker.C {
					func() {
						subCtx, c2 := context.WithTimeout(ctx, time.Second*2)
						defer c2()
						cmd := exec.CommandContext(subCtx, "nslookup", domain, ips[0])
						cmd.Stderr = io.Discard
						cmd.Stdout = io.Discard
						_ = cmd.Start()
						_ = cmd.Wait()
					}()
				}
			}(domain)
		}
	}()

	// 4) query with dns client
	client := &miekgdns.Client{Net: "udp", Timeout: time.Second * 2, SingleInflight: true}
	for _, domain := range c.ExtraDomain {
		var success = false
		for _, qType := range []uint16{miekgdns.TypeA /*, miekgdns.TypeAAAA*/} {
			var iErr = errors.New("No retry")
			err = retry.OnError(
				wait.Backoff{
					Steps:    1000,
					Duration: time.Millisecond * 30,
				},
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
					if len(answer.Answer) == 0 {
						return iErr
					}
					for _, rr := range answer.Answer {
						switch a := rr.(type) {
						case *miekgdns.A:
							if ip := net.ParseIP(a.A.String()); ip != nil && !ip.IsLoopback() {
								addRouteFunc(domain, a.A.String())
								c.extraHost = append(c.extraHost, dns.Entry{IP: a.A.String(), Domain: domain})
								success = true
							}
						case *miekgdns.AAAA:
							if ip := net.ParseIP(a.AAAA.String()); ip != nil && !ip.IsLoopback() {
								addRouteFunc(domain, a.AAAA.String())
								c.extraHost = append(c.extraHost, dns.Entry{IP: a.AAAA.String(), Domain: domain})
								success = true
							}
						}
					}
					return nil
				})
			if err != nil && err != iErr {
				return err
			}
			if success {
				break
			}
		}
		if !success {
			return fmt.Errorf("failed to resolve dns for domain %s", domain)
		}
	}
	return nil
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

func (c *ConnectOptions) GetClientset() *kubernetes.Clientset {
	return c.clientset
}

func (c *ConnectOptions) GetFactory() cmdutil.Factory {
	return c.factory
}

func (c *ConnectOptions) GetLocalTunIPv4() string {
	if c.localTunIPv4 != nil {
		return c.localTunIPv4.IP.String()
	}
	return ""
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
			log.Errorf("failed to patch image update to pod template: %v", err)
			return err
		}
		err = util.RolloutStatus(ctx, c.factory, c.Namespace, fmt.Sprintf("%s/%s", p.Info.Mapping.Resource.GroupResource().String(), p.Info.Name), time.Minute*60)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *ConnectOptions) heartbeats(ctx context.Context) {
	if !util.IsWindows() {
		return
	}
	ticker := time.NewTicker(time.Second * 15)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		select {
		case <-ctx.Done():
			return
		default:
		}
		func() {
			defer func() {
				if err := recover(); err != nil {
					log.Debug(err)
				}
			}()

			err := c.dhcp.ForEach(func(ip net.IP) {
				go func() {
					_, _ = util.Ping(ip.String())
				}()
			})
			if err != nil {
				log.Debug(err)
			}
		}()
	}
}

func (c *ConnectOptions) Equal(a *ConnectOptions) bool {
	return c.UseLocalDNS == a.UseLocalDNS &&
		c.Engine == a.Engine &&
		reflect.DeepEqual(c.ExtraDomain, a.ExtraDomain) &&
		reflect.DeepEqual(c.ExtraCIDR, a.ExtraCIDR)
}

func (c *ConnectOptions) GetTunDeviceName() (string, error) {
	var ips []net.IP
	if c.localTunIPv4 != nil {
		ips = append(ips, c.localTunIPv4.IP)
	}
	if c.localTunIPv6 != nil {
		ips = append(ips, c.localTunIPv6.IP)
	}
	device, err := util.GetTunDevice(ips...)
	if err != nil {
		return "", err
	}
	return device.Name, nil
}

func (c *ConnectOptions) GetKubeconfigCluster() string {
	rawConfig, err := c.GetFactory().ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return ""
	}
	if rawConfig.Contexts != nil && rawConfig.Contexts[rawConfig.CurrentContext] != nil {
		return rawConfig.Contexts[rawConfig.CurrentContext].Cluster
	}
	return ""
}

func (c *ConnectOptions) AddRolloutFunc(f func() error) {
	c.rollbackFuncList = append(c.rollbackFuncList, f)
}

func (c *ConnectOptions) getRolloutFunc() []func() error {
	return c.rollbackFuncList
}
