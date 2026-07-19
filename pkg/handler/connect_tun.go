package handler

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

const (
	// healthCheckDialTimeout bounds dialing/checking the local control-plane and port-forward endpoints.
	healthCheckDialTimeout = 5 * time.Second
	// portForwardReadyTimeout is how long to wait for the port-forward to become ready before giving up.
	portForwardReadyTimeout = 60 * time.Second
	// healthCheckLoopInterval is the period between port-forward health checks.
	healthCheckLoopInterval = 30 * time.Second
	// healthCheckPerfWarnThreshold logs a perf warning when a health check exceeds this duration.
	healthCheckPerfWarnThreshold = 500 * time.Millisecond
	// healthCheckRetryInterval is the fixed backoff between health-check retries within one tick.
	healthCheckRetryInterval = 10 * time.Second
	// healthCheckRetrySteps is the number of health-check retries attempted within one tick.
	healthCheckRetrySteps = 3
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
			dialCtx, dialCancel := context.WithTimeout(ctx, healthCheckDialTimeout)
			defer dialCancel()
			conn, err = grpc.DialContext(dialCtx, target,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithBlock(),
			)
			if err != nil {
				return err
			}
		}
		checkCtx, checkCancel := context.WithTimeout(ctx, healthCheckDialTimeout)
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

func healthCheckLoop(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, checker func() error) {
	defer cancelFunc()
	readyTimeout := time.NewTicker(portForwardReadyTimeout)
	defer readyTimeout.Stop()

	select {
	case <-readyChan:
	case <-readyTimeout.C:
		plog.G(ctx).Debugf("Wait port-forward to be ready timeout")
		return
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(healthCheckLoopInterval)
	defer ticker.Stop()
	for ; ctx.Err() == nil; <-ticker.C {
		checkStart := time.Now()
		err := retry.OnError(wait.Backoff{Duration: healthCheckRetryInterval, Steps: healthCheckRetrySteps}, func(err error) bool {
			return err != nil
		}, checker)
		elapsed := time.Since(checkStart)
		if elapsed > healthCheckPerfWarnThreshold {
			plog.G(ctx).Warnf("[Perf] Health check took %v", elapsed)
		}
		if err != nil {
			plog.G(ctx).Errorf("Health check failed after %v: %v", elapsed, err)
			return
		}
	}
}
