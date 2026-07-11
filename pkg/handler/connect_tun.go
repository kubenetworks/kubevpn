package handler

import (
	"context"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
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

func healthCheckTCPConn(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, controlPlanePort string, ownerID string, namespace string) {
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
		timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := rpc.NewTunConfigServiceClient(conn).GetTunIP(timeoutCtx, &rpc.TunIPRequest{
			OwnerID:   ownerID,
			Namespace: namespace,
		})
		if err != nil {
			conn.Close()
			conn = nil
		}
		return err
	})

	if conn != nil {
		conn.Close()
	}
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
