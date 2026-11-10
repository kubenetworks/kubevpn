package tunnel

import (
	"bytes"
	"net"
	"sync/atomic"
	"testing"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestCopyViewToPoolDoesNotAliasRecycledView(t *testing.T) {
	srcIP, dstIP := net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 99)
	ip := buildIPv4Packet(srcIP, dstIP, []byte("first-payload"))
	payload := append([]byte{packetTypeToGvisor}, ip...)
	view := buffer.NewViewSize(datagramHeaderLen + len(payload))
	copy(view.AsSlice()[datagramHeaderLen:], payload)
	view.TrimFront(datagramHeaderLen)
	buf, src, dst, err := copyViewToPool(view)
	if err != nil {
		t.Fatal(err)
	}
	defer config.LPool.Put(buf)
	view.Release()
	for i := 0; i < 8; i++ {
		other := buffer.NewViewSize(datagramHeaderLen + len(payload))
		copy(other.AsSlice()[datagramHeaderLen:], bytes.Repeat([]byte{0x7f}, len(payload)))
		other.Release()
	}
	if got := buf[datagramHeaderLen : datagramHeaderLen+len(payload)]; !bytes.Equal(got, payload) {
		t.Fatalf("copied payload changed after View recycle: got %x want %x", got, payload)
	}
	if !src.Equal(srcIP) || !dst.Equal(dstIP) {
		t.Fatalf("addresses alias recycled View: src=%s dst=%s", src, dst)
	}
}

type retainedPacketDispatcher struct {
	pkt      *stack.PacketBuffer
	releases atomic.Int32
}

func (d *retainedPacketDispatcher) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	pkt.IncRef()
	d.pkt = pkt
}
func (*retainedPacketDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {
}
func (*retainedPacketDispatcher) OnUnexpectedInputInterface(stack.MulticastPacketContext) {}

func TestGvisorViewRetainedAfterInject(t *testing.T) {
	d := &retainedPacketDispatcher{}
	endpoint := channel.New(1, 1500, tcpip.LinkAddress("02:00:00:00:00:01"))
	endpoint.Attach(d)
	payload := []byte("retained-payload")
	view := buffer.NewViewSize(datagramHeaderLen + len(payload))
	copy(view.AsSlice()[datagramHeaderLen:], payload)
	view.TrimFront(datagramHeaderLen)
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload:   buffer.MakeWithView(view),
		OnRelease: func() { d.releases.Add(1) },
	})
	endpoint.InjectInbound(header.IPv4ProtocolNumber, pkt)
	pkt.DecRef()
	for i := 0; i < 8; i++ {
		other := buffer.NewViewSize(datagramHeaderLen + len(payload))
		copy(other.AsSlice()[datagramHeaderLen:], bytes.Repeat([]byte{0x2a}, len(payload)))
		other.Release()
	}
	got, ok := d.pkt.Data().PullUp(len(payload))
	if !ok || !bytes.Equal(got, payload) {
		t.Fatalf("retained packet was recycled: got %x, want %x", got, payload)
	}
	d.pkt.DecRef()
	if got := d.releases.Load(); got != 1 {
		t.Fatalf("release callback count = %d, want 1", got)
	}
}
