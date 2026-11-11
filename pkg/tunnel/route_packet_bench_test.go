package tunnel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type atomicPacketWriter struct {
	*mockConn
	count atomic.Int64
}

func (w *atomicPacketWriter) writePacket(_ context.Context, p *packet) error {
	w.count.Add(1)
	p.release()
	return nil
}

func BenchmarkRoutePacketDispatch(b *testing.B) {
	for _, members := range []int{1, 2, 4} {
		for _, routes := range []int{1, 32, 256} {
			for _, parallel := range []bool{false, true} {
				if parallel && (members > 2 || routes > 32) {
					continue
				}
				b.Run(fmt.Sprintf("members=%d/routes=%d/parallel=%t", members, routes, parallel), func(b *testing.B) {
					ctx := context.Background()
					hub := NewRouter()
					var writers []*atomicPacketWriter
					var keys []string
					for i := 0; i < routes; i++ {
						ip := net.IPv4(198, 18, byte(i>>8), byte(i)).To4()
						keys = append(keys, string(ip))
						for j := 0; j < members; j++ {
							writer := &atomicPacketWriter{mockConn: newMockConn(fmt.Sprintf("127.0.0.1:%d", j+1), false)}
							writers = append(writers, writer)
							hub.AddRoute(ctx, ip, writer)
						}
					}
					write := func(p *packet, i int) {
						if _, err := hub.WriteToRoutePacket(ctx, keys[i%len(keys)], p); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportAllocs()
					b.SetBytes(1500)
					b.ResetTimer()
					if parallel {
						b.RunParallel(func(pb *testing.PB) {
							p := batchPacket(1500, 7)
							defer p.release()
							i := 0
							for pb.Next() {
								write(p, i)
								i++
							}
						})
					} else {
						p := batchPacket(1500, 7)
						defer p.release()
						for i := 0; i < b.N; i++ {
							write(p, i)
						}
					}
					b.StopTimer()
					var delivered int64
					for _, writer := range writers {
						delivered += writer.count.Load()
					}
					if delivered != int64(b.N) {
						b.Fatalf("delivered %d frames, want %d", delivered, b.N)
					}
				})
			}
		}
	}
}

func BenchmarkRoutePacketBackpressure(b *testing.B) {
	for _, parallel := range []bool{false, true} {
		b.Run(fmt.Sprintf("parallel=%t", parallel), func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			raw, peer := net.Pipe()
			conn := &bufferedTCP{runDone: make(chan struct{}), Conn: raw, ch: make(chan *packet, 1), done: make(chan struct{})}
			ip := net.IPv4(198, 18, 0, 4).To4()
			hub := NewRouter()
			hub.AddRoute(ctx, ip, conn)
			p := batchPacket(1500, 7)
			p.release()
			defer p.release()
			var workers sync.WaitGroup
			workers.Add(2)
			go func() { defer workers.Done(); conn.run(ctx) }()
			var sinkCount int
			var sinkErr error
			go func() {
				defer workers.Done()
				sinkCount, sinkErr = benchmarkFrameSink(peer, [][]byte{p.data[:1500]}, 0, nil)
			}()
			b.Cleanup(func() {
				conn.Close()
				peer.Close()
				workers.Wait()
			})
			write := func(p *packet) {
				if _, err := hub.WriteToRoutePacket(ctx, string(ip), p); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.SetBytes(1500)
			b.ResetTimer()
			if parallel {
				b.RunParallel(func(pb *testing.PB) {
					pkt := batchPacket(1500, 7)
					defer pkt.release()
					for pb.Next() {
						write(pkt)
					}
				})
			} else {
				for i := 0; i < b.N; i++ {
					write(p)
				}
			}
			close(conn.ch)
			workers.Wait()
			b.StopTimer()
			if sinkErr != nil || sinkCount != b.N {
				b.Fatalf("delivered %d frames, want %d (err=%v)", sinkCount, b.N, sinkErr)
			}
			if p.refs.Load() != 0 {
				b.Fatal("route retained packet after backpressure drain")
			}
		})
	}
}

func BenchmarkRoutePacketFrameSizes(b *testing.B) {
	for _, mode := range []string{"concreteTCP", "realTLS"} {
		for _, size := range []int{64, 9000} {
			b.Run(fmt.Sprintf("%s/size=%d", mode, size), func(b *testing.B) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				hub := NewRouter()
				ip := net.IPv4(198, 18, 0, 5).To4()
				p := batchPacket(size, 7)
				p.release()
				defer p.release()
				raw, peer := batchTCPPair(b, mode == "realTLS")
				conn := &bufferedTCP{runDone: make(chan struct{}), Conn: raw, ch: make(chan *packet, MaxSize), done: make(chan struct{})}
				hub.AddRoute(ctx, ip, conn)
				var workers sync.WaitGroup
				workers.Add(2)
				go func() { defer workers.Done(); conn.run(ctx) }()
				var count int
				var sinkErr error
				go func() {
					defer workers.Done()
					count, sinkErr = benchmarkFrameSink(peer, [][]byte{p.data[:size]}, 0, nil)
				}()
				b.Cleanup(func() {
					cancel()
					raw.Close()
					peer.Close()
					workers.Wait()
				})
				b.ReportAllocs()
				b.SetBytes(int64(size))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := hub.WriteToRoutePacket(ctx, string(ip), p); err != nil {
						b.Fatal(err)
					}
				}
				close(conn.ch)
				workers.Wait()
				b.StopTimer()
				if sinkErr != nil || count != b.N {
					b.Fatalf("delivered %d frames, want %d (err=%v)", count, b.N, sinkErr)
				}
			})
		}
	}
}
