package wire

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"testing/iotest"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestReadViewExactLengthAndMakeWithView(t *testing.T) {
	payload := []byte("golden datagram")
	frame := make([]byte, HeaderLen+len(payload))
	binary.BigEndian.PutUint16(frame[:HeaderLen], uint16(len(payload)))
	copy(frame[HeaderLen:], payload)
	conn, _ := NewConn(context.Background(), &framingConn{Conn: &fragmentedReader{reader: iotest.OneByteReader(bytes.NewReader(frame))}})
	defer conn.Close()
	view, err := conn.(*Conn).ReadView()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := view.Size(), len(payload); got != want {
		t.Fatalf("view size = %d, want %d", got, want)
	}
	original := view.AsSlice()
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithView(view)})
	got, ok := pkt.Data().PullUp(len(original))
	if !ok {
		t.Fatal("PullUp failed")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
	if &got[0] != &original[0] {
		t.Fatal("MakeWithView copied the payload instead of transferring the view")
	}
	pkt.DecRef()
}

func TestReadViewEmptyAndShortBody(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		conn, _ := NewConn(context.Background(), &framingConn{Conn: &fragmentedReader{reader: bytes.NewReader([]byte{0, 0})}})
		defer conn.Close()
		view, err := conn.(*Conn).ReadView()
		if err != nil || view.Size() != 0 {
			t.Fatalf("empty view = %v, %v", view, err)
		}
		view.Release()
	})
	t.Run("short body", func(t *testing.T) {
		tracked := &framingConn{Conn: &fragmentedReader{reader: bytes.NewReader([]byte{0, 3, 7})}}
		conn, _ := NewConn(context.Background(), tracked)
		defer conn.Close()
		_, err := conn.(*Conn).ReadView()
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("short body error = %v, want %v", err, io.ErrUnexpectedEOF)
		}
		if tracked.closes.Load() != 1 {
			t.Fatal("short body did not close the damaged stream")
		}
	})
}

type fragmentedReader struct{ reader io.Reader }

func (r *fragmentedReader) Read(p []byte) (int, error)       { return r.reader.Read(p) }
func (r *fragmentedReader) Write(p []byte) (int, error)      { return len(p), nil }
func (r *fragmentedReader) Close() error                     { return nil }
func (r *fragmentedReader) LocalAddr() net.Addr              { return nil }
func (r *fragmentedReader) RemoteAddr() net.Addr             { return nil }
func (r *fragmentedReader) SetDeadline(time.Time) error      { return nil }
func (r *fragmentedReader) SetReadDeadline(time.Time) error  { return nil }
func (r *fragmentedReader) SetWriteDeadline(time.Time) error { return nil }

func BenchmarkServerIngressBuffer(b *testing.B) {
	for _, size := range []int{64, 1500, 9000, 65535} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			payload := bytes.Repeat([]byte{0x5a}, size)
			frame := make([]byte, HeaderLen+len(payload))
			binary.BigEndian.PutUint16(frame[:HeaderLen], uint16(len(payload)))
			copy(frame[HeaderLen:], payload)
			for _, mode := range []string{"byte_slice", "view"} {
				b.Run(mode, func(b *testing.B) {
					reader := bytes.NewReader(frame)
					b.ReportAllocs()
					b.SetBytes(int64(size))
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						reader.Reset(frame)
						if mode == "byte_slice" {
							buf := make([]byte, size)
							if _, err := ReadDatagram(reader, buf); err != nil {
								b.Fatal(err)
							}
							b := buffer.MakeWithData(buf)
							b.Release()
						} else {
							v, err := readDatagramView(reader)
							if err != nil {
								b.Fatal(err)
							}
							b := buffer.MakeWithView(v)
							b.Release()
						}
					}
				})
			}
		})
	}
}
