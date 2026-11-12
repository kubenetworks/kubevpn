package tunnel

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
)

func TestPacketCanonicalLayoutCharacterization(t *testing.T) {
	buf := bytes.Repeat([]byte{0xaa}, config.LargeBufferSize)
	ip := []byte{0x45, 0x00, 0x00, 0x04}
	copy(buf[tunReserve:], ip)
	buf[datagramHeaderLen] = packetTypeToGvisor
	pkt := newPacket(buf, typePrefixLen+len(ip), nil, nil)

	if got, want := pkt.Length(), typePrefixLen+len(ip); got != want {
		t.Fatalf("packet.Length() = %d, want payload length %d (not buffer length %d)", got, want, len(buf))
	}
	if err := framing.WriteDatagram(new(bytes.Buffer), pkt.Data(), pkt.Length()); err != nil {
		t.Fatalf("WriteDatagram: %v", err)
	}

	wantPrefix := []byte{0x00, 0x05, packetTypeToGvisor, 0x45, 0x00, 0x00, 0x04}
	if !bytes.Equal(buf[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("wire prefix = %v, want %v", buf[:len(wantPrefix)], wantPrefix)
	}
	if got := binary.BigEndian.Uint16(buf[:datagramHeaderLen]); got != uint16(typePrefixLen+len(ip)) {
		t.Fatalf("encoded length = %d, want %d", got, typePrefixLen+len(ip))
	}
	for i, b := range buf[len(wantPrefix):] {
		if b != 0xaa {
			t.Fatalf("byte beyond frame at offset %d changed to %#x", len(wantPrefix)+i, b)
		}
	}
}

func TestPacketViewsAndWriteTo(t *testing.T) {
	pkt := newPacket(config.LPool.Get().([]byte)[:], 0, nil, nil)
	ip := []byte{0x45, 0x01, 0x02}
	pkt.setIP(ip)
	pkt.setType(packetTypeControl)
	if got := pkt.packetType(); got != packetTypeControl {
		t.Fatalf("packetType() = %d, want %d", got, packetTypeControl)
	}
	if got := pkt.ip(); !bytes.Equal(got, ip) {
		t.Fatalf("ip() = %v, want %v", got, ip)
	}
	if got := pkt.frameLen(); got != datagramHeaderLen+typePrefixLen+len(ip) {
		t.Fatalf("frameLen() = %d, want %d", got, datagramHeaderLen+typePrefixLen+len(ip))
	}
	var out bytes.Buffer
	n, err := pkt.writeTo(&out)
	if err != nil {
		t.Fatalf("writeTo: %v", err)
	}
	want := []byte{0x00, 0x04, packetTypeControl, 0x45, 0x01, 0x02}
	if n != len(want) || !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("writeTo n=%d bytes=%v, want %d/%v", n, out.Bytes(), len(want), want)
	}
	if got := pkt.frame(); !bytes.Equal(got, want) {
		t.Fatalf("frame() = %v, want %v", got, want)
	}
	pkt.release()
}

func TestPacketRefcountCharacterization(t *testing.T) {
	pkt := newPacket(config.LPool.Get().([]byte)[:], typePrefixLen, nil, nil)
	if got := pkt.refs.Load(); got != 0 {
		t.Fatalf("new packet extra refs = %d, want 0", got)
	}
	pkt.acquire()
	pkt.release()
	if got := pkt.refs.Load(); got != 0 {
		t.Fatalf("packet extra refs after acquire/release = %d, want 0", got)
	}
	pkt.release()
	if got := pkt.refs.Load(); got != -1 {
		t.Fatalf("packet refs after final release = %d, want -1", got)
	}
}

func TestPacketSharedOwnersReleaseOnce(t *testing.T) {
	pkt := newPacket(config.LPool.Get().([]byte)[:], typePrefixLen, nil, nil)
	const consumers = 3
	pkt.acquire()
	pkt.acquire()
	var wg sync.WaitGroup
	wg.Add(consumers)
	for range consumers {
		go func() {
			defer wg.Done()
			pkt.release()
		}()
	}
	wg.Wait()
	if got := pkt.refs.Load(); got != -1 {
		t.Fatalf("refs after all owners released = %d, want -1", got)
	}
}

func TestPacketImmediateDoubleReleasePanics(t *testing.T) {
	pkt := newPacket(config.LPool.Get().([]byte)[:], typePrefixLen, nil, nil)
	pkt.release()
	defer func() {
		if recover() == nil {
			t.Fatal("double release before reuse did not panic")
		}
	}()
	pkt.release()
}

func TestPacketPoolResetsMetadata(t *testing.T) {
	src, dst := net.IPv4(198, 18, 0, 2), net.IPv4(10, 0, 0, 1)
	pkt := newPacket(config.LPool.Get().([]byte)[:], 4, src, dst)
	pkt.probeEpoch = 9
	pkt.probeQueued = true
	pkt.release()
	if err := func() (err error) {
		defer func() {
			if recover() != nil {
				err = fmt.Errorf("acquire after final release panicked")
			}
		}()
		pkt.acquire()
		return nil
	}(); err == nil {
		t.Fatal("acquire after final release did not panic")
	}

	recycled := newPacket(config.LPool.Get().([]byte)[:], 0, nil, nil)
	if recycled.refs.Load() != 0 || recycled.data == nil || recycled.length != 0 || recycled.src != nil || recycled.dst != nil || recycled.probeEpoch != 0 || recycled.probeQueued {
		t.Fatalf("recycled packet retained metadata: %+v", recycled)
	}
	recycled.release()
}
