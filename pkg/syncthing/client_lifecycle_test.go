package syncthing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/thejerf/suture/v4"
)

type controlledApp struct {
	startErr error
	runErr   error
	stopping chan struct{}
	exited   chan struct{}
	stopOnce sync.Once
}

func (a *controlledApp) Start() error { return a.startErr }
func (a *controlledApp) Stop(svcutil.ExitStatus) svcutil.ExitStatus {
	a.stopOnce.Do(func() { close(a.stopping) })
	<-a.exited
	return svcutil.ExitSuccess
}
func (a *controlledApp) Wait() svcutil.ExitStatus { <-a.exited; return svcutil.ExitSuccess }
func (a *controlledApp) Error() error             { return a.runErr }

type controlledService struct{ started, stopping, release chan struct{} }

func (s *controlledService) Serve(ctx context.Context) error {
	close(s.started)
	<-ctx.Done()
	close(s.stopping)
	<-s.release
	return ctx.Err()
}

func TestClientLifecycle_JoinsApplicationAndEarlyServices(t *testing.T) {
	for _, failStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "startup-failure"}[failStart], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service := &controlledService{make(chan struct{}), make(chan struct{}), make(chan struct{})}
			early := suture.NewSimple("test-early")
			early.Add(service)
			earlyDone := early.ServeBackground(ctx)
			await := func(ch <-chan struct{}) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("lifecycle event timed out")
				}
			}
			await(service.started)
			app := &controlledApp{stopping: make(chan struct{}), exited: make(chan struct{})}
			var appOnce, serviceOnce sync.Once
			releaseApp := func() { appOnce.Do(func() { close(app.exited) }) }
			releaseService := func() { serviceOnce.Do(func() { close(service.release) }) }
			defer releaseApp()
			defer releaseService()
			startupErr := errors.New("startup failed")
			if failStart {
				app.startErr = startupErr
			}
			type result struct {
				h   *ClientHandle
				err error
			}
			started := make(chan result, 1)
			go func() { h, err := startClientApp(ctx, cancel, earlyDone, app); started <- result{h, err} }()
			var handle *ClientHandle
			stopped := make(chan struct{})
			if !failStart {
				select {
				case r := <-started:
					if r.err != nil {
						t.Fatal(r.err)
					}
					handle = r.h
				case <-time.After(5 * time.Second):
					t.Fatal("start timed out")
				}
				go func() { handle.Stop(); close(stopped) }()
			}
			await(app.stopping)
			select {
			case <-stopped:
				t.Fatal("Stop returned before application Wait")
			default:
			}
			if failStart {
				select {
				case <-started:
					t.Fatal("failed startup returned before app exit")
				default:
				}
			}
			releaseApp()
			await(service.stopping)
			if !failStart {
				select {
				case <-handle.Done():
					t.Fatal("Done closed before early service exit")
				default:
				}
			} else {
				select {
				case <-started:
					t.Fatal("failed startup returned before early service exit")
				default:
				}
			}
			releaseService()
			if failStart {
				select {
				case r := <-started:
					if r.h != nil || !errors.Is(r.err, startupErr) {
						t.Fatalf("unexpected result: %+v", r)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("startup cleanup timed out")
				}
			} else {
				await(stopped)
				await(handle.Done())
				handle.Stop()
				if handle.Err() != nil {
					t.Fatal(handle.Err())
				}
			}
		})
	}
}
