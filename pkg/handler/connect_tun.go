package handler

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	miekgdns "github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// portForward sets up port-forwarding to the traffic manager pod with automatic
// retry when the pod is recreated or the connection drops.
func (c *ConnectOptions) portForward(ctx context.Context, portPair []string) error {
	firstCtx, firstCancelFunc := context.WithCancel(ctx)
	defer firstCancelFunc()
	errChan := make(chan error, 1)
	go func() {
		runtime.ErrorHandlers = []runtime.ErrorHandler{func(ctx context.Context, err error, msg string, keysAndValues ...any) {
			plog.G(ctx).Error(err)
		}}
		first := ptr.To(true)
		for ctx.Err() == nil {
			err := c.portForwardOnce(ctx, portPair, *first, firstCancelFunc)
			if *first {
				if err != nil {
					util.SafeWrite(errChan, err)
					return
				}
			}
			first = ptr.To(false)
			time.Sleep(time.Millisecond * 200)
		}
	}()
	ticker := time.NewTicker(time.Second * 60)
	defer ticker.Stop()
	select {
	case <-ticker.C:
		return config.ErrPortForwardTimeout
	case err := <-errChan:
		return err
	case <-firstCtx.Done():
		return nil
	}
}

// portForwardOnce runs a single port-forward session to the traffic manager pod.
// It selects a running pod, starts health checks and pod-deletion watchers, then
// blocks on the port-forward until the session ends. Returns nil when the session
// was established and later terminated (retry expected), or an error if the pod
// could not be found. When first is true, onReady is called once the port-forward
// becomes ready (before the session ends).
func (c *ConnectOptions) portForwardOnce(ctx context.Context, portPair []string, first bool, onReady func()) error {
	ctx2, cancelFunc2 := context.WithTimeout(ctx, time.Second*10)
	defer cancelFunc2()
	podList, err := c.GetRunningPodList(ctx2)
	if err != nil {
		plog.G(ctx).Debugf("Failed to get running pod: %v", err)
		return err
	}
	pod := podList[0]
	// add route in case we don't have permission to watch the pod, but
	// the pod was recreated with a new IP that is not yet routable
	_ = c.addRoute(util.GetPodIP(pod)...)

	childCtx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	readyChan := make(chan struct{})
	podName := pod.GetName()
	// detect pod deletion so we can redo port-forward
	go util.CheckPodStatus(childCtx, cancelFunc, podName, c.clientset.CoreV1().Pods(c.ManagerNamespace))
	domain := config.ConfigMapPodTrafficManager
	gvisorUDPPort, _, _ := strings.Cut(portPair[1], ":")
	go healthCheckPortForward(childCtx, cancelFunc, readyChan, gvisorUDPPort, domain, c.LocalTunIPv4.IP)
	go healthCheckTCPConn(childCtx, cancelFunc, readyChan, domain, util.GetPodIP(pod)[0])
	if first {
		go func() {
			select {
			case <-readyChan:
				onReady()
			case <-childCtx.Done():
			}
		}()
	}

	err = util.PortForwardPod(
		c.config,
		c.restclient,
		podName,
		c.ManagerNamespace,
		portPair,
		readyChan,
		childCtx.Done(),
		nil,
		plog.G(ctx).Logger.Out,
	)
	if err != nil {
		plog.G(ctx).Debugf("Port-forward error: %v", err)
	} else {
		plog.G(ctx).Debugf("Port forward retrying")
	}
	return nil
}

func (c *ConnectOptions) startLocalTunServer(ctx context.Context, forwardAddress string) (err error) {
	plog.G(ctx).Debugf("IPv4: %s, IPv6: %s", c.LocalTunIPv4.IP.String(), c.LocalTunIPv6.IP.String())

	tlsSecret, err := c.clientset.CoreV1().Secrets(c.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}

	var cidrList []*net.IPNet
	for _, ipNet := range c.cidrs {
		cidrList = append(cidrList, ipNet)
	}
	// add extra-cidr
	for _, s := range c.ExtraRouteInfo.ExtraCIDR {
		var ipNet *net.IPNet
		_, ipNet, err = net.ParseCIDR(s)
		if err != nil {
			return fmt.Errorf("invalid extra-cidr %s: %w", s, err)
		}
		cidrList = append(cidrList, ipNet)
	}

	var routes []types.Route
	for _, ipNet := range c.dedupAndFilterCIDRs(cidrList) {
		if ipNet != nil && !ipNet.IP.IsLoopback() {
			routes = append(routes, types.Route{Dst: *ipNet})
		}
	}
	if c.LocalTunIPv4 != nil {
		routes = append(routes, types.Route{Dst: net.IPNet{IP: c.LocalTunIPv4.IP, Mask: net.CIDRMask(32, 32)}})
	}
	if c.LocalTunIPv6 != nil {
		routes = append(routes, types.Route{Dst: net.IPNet{IP: c.LocalTunIPv6.IP, Mask: net.CIDRMask(128, 128)}})
	}

	tunConfig := tun.Config{
		Addr:   (&net.IPNet{IP: c.LocalTunIPv4.IP, Mask: net.CIDRMask(32, 32)}).String(),
		Routes: routes,
		MTU:    config.DefaultMTU,
	}
	if enable, _ := util.IsIPv6Enabled(); enable {
		tunConfig.Addr6 = (&net.IPNet{IP: c.LocalTunIPv6.IP, Mask: net.CIDRMask(128, 128)}).String()
	}

	forwardNode, err := core.ParseNode(forwardAddress)
	if err != nil {
		plog.G(ctx).Errorf("Failed to parse forward node %s: %v", forwardAddress, err)
		return err
	}
	forwarder := &core.Forwarder{
		Addr:        forwardNode.Addr,
		Connector:   core.NewUDPOverTCPConnector(),
		Transporter: core.TCPTransporter(tlsSecret.Data),
		MaxRetries:  5,
	}

	handler := core.TunHandler(forwarder, core.NewRouteHub())
	listener, err := tun.Listener(tunConfig)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create tun listener: %v", err)
		return err
	}

	server := core.Server{
		Listener: listener,
		Handler:  handler,
	}

	go func() {
		if err := Run(ctx, []core.Server{server}); err != nil && ctx.Err() == nil {
			plog.G(ctx).Errorf("[Client] Local TUN server exited: %v", err)
		}
	}()
	plog.G(ctx).Infof("[Client] TUN server started, forwarding to %s", forwardAddress)

	c.tunName, err = c.GetTunDeviceName()
	return err
}

func Run(ctx context.Context, servers []core.Server) error {
	errChan := make(chan error, len(servers))
	for i := range servers {
		go func(i int) {
			errChan <- func() error {
				svr := servers[i]
				defer svr.Listener.Close()
				go func() {
					<-ctx.Done()
					svr.Listener.Close()
				}()
				for ctx.Err() == nil {
					conn, err := svr.Listener.Accept()
					if err != nil {
						return err
					}
					go svr.Handler.Handle(ctx, conn)
				}
				return ctx.Err()
			}()
		}(i)
	}

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func healthCheckPortForward(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, localGvisorUDPPort string, domain string, ipv4 net.IP) {
	checker := func() error {
		conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", localGvisorUDPPort))
		if err != nil {
			return err
		}
		defer conn.Close()
		err = util.WriteProxyInfo(conn, stack.TransportEndpointID{
			LocalPort:     53,
			LocalAddress:  tcpip.AddrFrom4Slice(net.ParseIP("127.0.0.1").To4()),
			RemotePort:    0,
			RemoteAddress: tcpip.AddrFrom4Slice(ipv4.To4()),
		})
		if err != nil {
			return err
		}

		packetConn, _ := core.NewPacketConnOverTCP(ctx, conn)
		defer packetConn.Close()

		msg := new(miekgdns.Msg)
		msg.SetQuestion(miekgdns.Fqdn(domain), miekgdns.TypeA)
		client := miekgdns.Client{Net: "udp", Timeout: time.Second * 10}
		_, _, err = client.ExchangeWithConnContext(ctx, msg, &miekgdns.Conn{Conn: packetConn})
		return err
	}
	healthCheckLoop(ctx, cancelFunc, readyChan, checker)
}

func healthCheckTCPConn(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, domain string, dnsServer string) {
	healthCheckLoop(ctx, cancelFunc, readyChan, func() error {
		return nameserverChecker(ctx, domain, dnsServer)
	})
}

func healthCheckLoop(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, checker func() error) {
	defer cancelFunc()
	readyTimeout := time.NewTicker(time.Second * 60)
	defer readyTimeout.Stop()

	select {
	case <-readyChan:
	case <-readyTimeout.C:
		plog.G(ctx).Debugf("Wait port-forward to be ready timeout")
		return
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()
	for ; ctx.Err() == nil; <-ticker.C {
		err := retry.OnError(wait.Backoff{Duration: time.Second * 10, Steps: 3}, func(err error) bool {
			return err != nil
		}, checker)
		if err != nil {
			plog.G(ctx).Errorf("Failed to query DNS: %v", err)
			return
		}
	}
}
