package handler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	miekgdns "github.com/miekg/dns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
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

func healthCheckGRPC(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, controlPlanePort string) {
	target := net.JoinHostPort("127.0.0.1", controlPlanePort)
	var conn *grpc.ClientConn

	healthCheckLoop(ctx, cancelFunc, readyChan, func() error {
		if conn == nil {
			var err error
			dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
			defer dialCancel()
			conn, err = grpc.DialContext(dialCtx, target,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithBlock(),
			)
			if err != nil {
				return err
			}
		}
		checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
		defer checkCancel()
		resp, err := grpc_health_v1.NewHealthClient(conn).Check(checkCtx, &grpc_health_v1.HealthCheckRequest{})
		if err != nil {
			conn.Close()
			conn = nil
			return err
		}
		if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
			conn.Close()
			conn = nil
			return fmt.Errorf("control plane not serving: %s", resp.Status)
		}
		return nil
	})

	if conn != nil {
		conn.Close()
	}
}

func healthCheckPortForward(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, localGvisorUDPPort string, domain string) {
	var conn net.Conn
	var packetConn net.Conn

	dial := func() error {
		var err error
		conn, err = net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", localGvisorUDPPort), 5*time.Second)
		if err != nil {
			return err
		}
		err = util.WriteProxyInfo(conn, stack.TransportEndpointID{
			LocalPort:     53,
			LocalAddress:  tcpip.AddrFrom4Slice(net.ParseIP("127.0.0.1").To4()),
			RemotePort:    0,
			RemoteAddress: tcpip.Address{},
		})
		if err != nil {
			conn.Close()
			conn = nil
			return err
		}
		packetConn, _ = core.NewPacketConnOverTCP(ctx, conn)
		return nil
	}

	closeConn := func() {
		if packetConn != nil {
			packetConn.Close()
			packetConn = nil
		}
		if conn != nil {
			conn.Close()
			conn = nil
		}
	}

	checker := func() error {
		if conn == nil {
			if err := dial(); err != nil {
				return err
			}
		}
		msg := new(miekgdns.Msg)
		msg.SetQuestion(miekgdns.Fqdn(domain), miekgdns.TypeA)
		client := miekgdns.Client{Net: "udp", Timeout: 10 * time.Second}
		resp, _, err := client.ExchangeWithConnContext(ctx, msg, &miekgdns.Conn{Conn: packetConn})
		if err != nil {
			closeConn()
			return err
		}
		if len(resp.Answer) == 0 {
			closeConn()
			return errors.New("no answers")
		}
		return nil
	}
	healthCheckLoop(ctx, cancelFunc, readyChan, checker)
	closeConn()
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
		checkStart := time.Now()
		err := retry.OnError(wait.Backoff{Duration: time.Second * 10, Steps: 3}, func(err error) bool {
			return err != nil
		}, checker)
		elapsed := time.Since(checkStart)
		if elapsed > 500*time.Millisecond {
			plog.G(ctx).Warnf("[Perf] Health check took %v", elapsed)
		}
		if err != nil {
			plog.G(ctx).Errorf("Health check failed after %v: %v", elapsed, err)
			return
		}
	}
}
