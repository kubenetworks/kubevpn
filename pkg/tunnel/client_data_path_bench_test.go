package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// BenchmarkClientDataPath covers the client forwarding hop and socket writer,
// unlike BenchmarkSlotBatchActivity (slot only) and BenchmarkPumpTunBatchReuse
// (TUN dispatch only). Each operation waits for the peer to receive the burst.
func BenchmarkClientDataPath(b *testing.B) {
	for _, size := range []int{64, config.DefaultMTU, config.LargeBufferSize - tunReserve} {
		for _, burst := range []int{1, 32} {
			b.Run(fmt.Sprintf("size=%d/burst=%d", size, burst), func(b *testing.B) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					b.Fatal(err)
				}
				logger := logrus.New()
				logger.Out = io.Discard
				ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), logger))
				dev := &tunDevice{
					tunName:     "bench0",
					tunInbound:  make(chan *packet, MaxSize),
					tunOutbound: make(chan *packet, MaxSize),
					errChan:     make(chan error, 1),
				}
				transport := (testDialer{Addr: listener.Addr().String(), MaxRetries: 1, Transporter: testTCPTransport{}, Connector: testConnector{}}).config()
				client := newClientTransport(dev, transport, &HeartbeatStats{})
				runDone := make(chan struct{})
				go func() {
					defer close(runDone)
					client.runConnPool(ctx)
				}()
				peer, err := listener.Accept()
				if err != nil {
					cancel()
					listener.Close()
					b.Fatal(err)
				}
				if err = peer.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					cancel()
					_ = peer.Close()
					_ = listener.Close()
					select {
					case <-runDone:
					case <-time.After(5 * time.Second):
						b.Error("client data path did not join")
					}
					client.close()
					drainPacketChan(dev.tunInbound)
					drainPacketChan(dev.tunOutbound)
				})

				src, dst := net.IPv4(198, 18, 0, 2).To4(), net.IPv4(10, 0, 0, 1).To4()
				ip := buildIPv4Packet(src, dst, bytes.Repeat([]byte{0x5a}, size-20))
				send := func() {
					buf := config.LPool.Get().([]byte)
					n := copy(buf[tunReserve:], ip)
					client.routeOutbound(ctx, buf, n, src, dst)
				}
				receive := func() {
					buf := make([]byte, config.LargeBufferSize)
					n, readErr := framing.ReadDatagram(peer, buf)
					if readErr != nil {
						b.Fatal(readErr)
					}
					if n != len(ip)+typePrefixLen || buf[0] != packetTypeToGvisor || !bytes.Equal(buf[typePrefixLen:n], ip) {
						b.Fatalf("received corrupt frame of %d bytes", n)
					}
				}
				send()
				receive() // connection and first writer are warm before timing.
				b.SetBytes(int64(burst * len(ip)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for range burst {
						send()
					}
					for range burst {
						receive()
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(burst), "frames/op")
			})
		}
	}
}
