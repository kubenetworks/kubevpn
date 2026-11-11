package tunnel

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errBlockedRouteWrite = errors.New("blocked route write")

type blockingRoutePacketWriter struct {
	*mockConn
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (w *blockingRoutePacketWriter) writePacket(ctx context.Context, p *packet) error {
	if w.calls.Add(1) > 1 {
		p.release()
		return nil
	}
	w.once.Do(func() { close(w.entered) })
	select {
	case <-w.release:
		return errBlockedRouteWrite
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRouteLifetime_BlockedBufferedWritePreservesReplacementIdentity(t *testing.T) {
	for _, mode := range []string{"same-list", "replacement-list", "readded-member"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			hub := NewRouter()
			key := net.ParseIP("198.18.0.90")
			old := &blockingRoutePacketWriter{mockConn: newMockConn("127.0.0.1:90", false), entered: make(chan struct{}), release: make(chan struct{})}
			fallback := &routeBenchmarkWriter{mockConn: newMockConn("127.0.0.1:91", false)}
			hub.AddRoute(ctx, key, fallback)
			hub.AddRoute(ctx, key, old)
			pkt := batchPacket(32, 0)
			pkt.release()
			done := make(chan error, 1)
			go func() {
				_, err := hub.WriteToRoutePacket(ctx, string(key), pkt)
				done <- err
			}()
			<-old.entered

			fresh := &routeBenchmarkWriter{mockConn: newMockConn("127.0.0.1:92", false)}
			switch mode {
			case "same-list":
				hub.RemoveRoutesByConn(ctx, old)
				hub.AddRoute(ctx, key, fresh)
			case "replacement-list":
				hub.RemoveRoutesByConn(ctx, old)
				hub.RemoveRoutesByConn(ctx, fallback)
				hub.AddRoute(ctx, key, fresh)
			case "readded-member":
				hub.RemoveRoutesByConn(ctx, old)
				hub.AddRoute(ctx, key, old)
			}

			// A different route remains fully usable while the old write is blocked.
			other := &routeBenchmarkWriter{mockConn: newMockConn("127.0.0.1:93", false)}
			otherPkt := batchPacket(32, 2)
			otherPkt.release()
			otherIP := net.ParseIP("198.18.0.93")
			hub.AddRoute(ctx, otherIP, other)
			if _, err := hub.WriteToRoutePacket(ctx, string(otherIP), otherPkt); err != nil {
				t.Fatal(err)
			}
			hub.RemoveRoutesByConn(ctx, other)
			close(old.release)
			if err := <-done; mode == "replacement-list" && err == nil {
				t.Fatal("stale snapshot unexpectedly wrote the replacement list")
			}

			next := batchPacket(32, 3)
			next.release()
			wantConn := net.Conn(fresh)
			if mode == "readded-member" {
				wantConn = old
			}
			used, err := hub.WriteToRoutePacket(ctx, string(key), next)
			if err != nil || used != wantConn {
				t.Fatalf("newest route write used %v, want %v (err=%v)", used, wantConn, err)
			}
			hub.RemoveRoutesByConn(ctx, old)
			hub.RemoveRoutesByConn(ctx, fresh)
			hub.RemoveRoutesByConn(ctx, fallback)
			checkBatchRefs(t, []*packet{pkt, otherPkt, next})
		})
	}
}

func TestRouteLifetime_BufferedAdmissionCancelKeepsReaddedMember(t *testing.T) {
	ctx, cancel := context.WithCancel(withDatapathDiagnostics(context.Background(), newDatapathDiagnostics()))
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	conn := &bufferedTCP{runDone: make(chan struct{}), Conn: raw, ch: make(chan *packet, 1), done: make(chan struct{})}
	conn.enqueueStage = diagnosticStage(ctx, "buffered_enqueue", "server", -1, 0)
	defer conn.enqueueStage.close()
	hub := NewRouter()
	key := net.ParseIP("198.18.0.94")
	hub.AddRoute(ctx, key, conn)
	first := batchPacket(32, 0)
	first.release()
	if _, err := hub.WriteToRoutePacket(ctx, string(key), first); err != nil {
		t.Fatal(err)
	}
	second := batchPacket(32, 1)
	second.release()
	done := make(chan error, 1)
	go func() {
		_, err := hub.WriteToRoutePacket(ctx, string(key), second)
		done <- err
	}()
	waitFor(t, time.Second, func() bool {
		conn.enqueueStage.mu.Lock()
		defer conn.enqueueStage.mu.Unlock()
		return conn.enqueueStage.waits != nil
	}, "buffered producer is waiting for queue admission")

	hub.RemoveRoutesByConn(ctx, conn)
	hub.AddRoute(ctx, key, conn)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("admission cancellation = %v, want context.Canceled", err)
	}
	if !hub.HasRoute(string(key)) {
		t.Fatal("admission cancellation removed the re-added route identity")
	}
	drainPacketChan(conn.ch)
	checkBatchRefs(t, []*packet{first, second})
}
