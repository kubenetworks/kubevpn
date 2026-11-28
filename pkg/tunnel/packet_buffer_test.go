package tunnel

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

const (
	testSmallBufferSize  = 4 * 1024
	testMediumBufferSize = 16 * 1024
)

func TestNewPacketFromPoolUsesSmallestContainingClass(t *testing.T) {
	tests := []struct {
		size int
		want int
	}{
		{0, testSmallBufferSize},
		{1, testSmallBufferSize},
		{testSmallBufferSize, testSmallBufferSize},
		{testSmallBufferSize + 1, testMediumBufferSize},
		{testMediumBufferSize, testMediumBufferSize},
		{testMediumBufferSize + 1, config.LargeBufferSize},
		{config.LargeBufferSize, config.LargeBufferSize},
		{config.LargeBufferSize + 1, config.LargeBufferSize + 1},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("size=%d", tt.size), func(t *testing.T) {
			packet := newPacketFromPool(tt.size)
			if len(packet.data) != tt.want {
				t.Fatalf("len(data)=%d, want %d", len(packet.data), tt.want)
			}
			packet.release()
		})
	}
}

func TestUnknownReadBufferStillUsesLargePool(t *testing.T) {
	packet := newPacketFromPool(config.LargeBufferSize)
	if len(packet.data) != config.LargeBufferSize {
		t.Fatalf("len(data)=%d, want %d", len(packet.data), config.LargeBufferSize)
	}
	packet.release()
}

func TestBufferedTCPWriteUsesActualFrameClass(t *testing.T) {
	conn := &bufferedTCP{ch: make(chan *packet, 1), done: make(chan struct{})}
	frame := bytes.Repeat([]byte{0xab}, 1500)
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	packet := <-conn.ch
	defer packet.release()
	if len(packet.data) != testSmallBufferSize {
		t.Fatalf("1500-byte frame used %d-byte buffer, want %d", len(packet.data), testSmallBufferSize)
	}
	if !bytes.Equal(packet.frame(), frame) {
		t.Fatal("frame contents changed")
	}
}
