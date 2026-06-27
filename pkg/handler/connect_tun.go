package handler

import (
	"context"
	"net"
	"time"

	miekgdns "github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

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

// addRoute delegates to NetworkManager if available. Before the TUN device is
// created (and NetworkManager is wired), this is a no-op because tunName is empty.
func (c *ConnectOptions) addRoute(ipStrList ...string) error {
	if c.network != nil {
		return c.network.AddRoute(ipStrList...)
	}
	return nil
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
