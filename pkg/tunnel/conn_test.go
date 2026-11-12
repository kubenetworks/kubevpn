package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// bufferedTCP tests
// ---------------------------------------------------------------------------

func TestBufferedTCP_WriteQueuesData(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buffered := newBufferedTCP(ctx, client, nil)

	payload := []byte("hello buffered tcp")
	framed := make([]byte, datagramHeaderLen+len(payload))
	binary.BigEndian.PutUint16(framed[:datagramHeaderLen], uint16(len(payload)))
	copy(framed[datagramHeaderLen:], payload)
	n, err := buffered.Write(framed)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len(framed) {
		t.Fatalf("Write returned n=%d, want %d", n, len(framed))
	}

	// Read from the server side — the background goroutine should have forwarded the data
	buf := make([]byte, 256)
	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	rn, err := server.Read(buf)
	if err != nil {
		t.Fatalf("server Read error: %v", err)
	}
	if !bytes.Equal(buf[:rn], framed) {
		t.Fatalf("got %q, want %q", buf[:rn], framed)
	}
}

func TestBufferedTCP_WriteEmptyIsNoop(t *testing.T) {
	_, client := net.Pipe()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buffered := newBufferedTCP(ctx, client, nil)
	n, err := buffered.Write(nil)
	if err != nil {
		t.Fatalf("Write(nil) returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("Write(nil) returned n=%d, want 0", n)
	}

	n, err = buffered.Write([]byte{})
	if err != nil {
		t.Fatalf("Write([]) returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("Write([]) returned n=%d, want 0", n)
	}
}

func TestBufferedTCP_CloseDrainsQueue(t *testing.T) {
	server, client := net.Pipe()

	ctx, cancel := context.WithCancel(context.Background())

	buffered := newBufferedTCP(ctx, client, nil)

	// Write multiple packets quickly
	for i := 0; i < 5; i++ {
		_, err := buffered.Write([]byte("data"))
		if err != nil {
			t.Fatalf("Write #%d error: %v", i, err)
		}
	}

	// Read them from server side in a goroutine to avoid blocking the pipe
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			_ = server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			_, err := server.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	// Give background goroutine time to flush
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Close should not panic and should drain remaining items
	err := buffered.Close()
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	wg.Wait()
	server.Close()
}

func TestBufferedTCP_WriteAfterCloseReturnsError(t *testing.T) {
	_, client := net.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buffered := newBufferedTCP(ctx, client, nil)
	_ = buffered.Close()

	_, err := buffered.Write([]byte("after close"))
	if err == nil {
		t.Fatal("Write after Close should return error")
	}
}

// ---------------------------------------------------------------------------
// framing.Conn tests
// ---------------------------------------------------------------------------

func TestUDPConnOverTCP_WriteWrapsWithLengthPrefix(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ctx := context.Background()
	conn, err := framing.NewConn(ctx, client)
	if err != nil {
		t.Fatalf("framing.NewConn: %v", err)
	}

	payload := []byte("udp payload over tcp")

	// net.Pipe is synchronous: Write blocks until Read drains, so read in a goroutine
	var readErr error
	var gotLen uint16
	var gotPayload []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2+len(payload))
		_, readErr = io.ReadFull(server, buf)
		if readErr != nil {
			return
		}
		gotLen = binary.BigEndian.Uint16(buf[:2])
		gotPayload = buf[2:]
	}()

	n, err := conn.Write(payload)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write returned n=%d, want %d", n, len(payload))
	}

	<-done
	if readErr != nil {
		t.Fatalf("server ReadFull error: %v", readErr)
	}
	if int(gotLen) != len(payload) {
		t.Fatalf("length prefix = %d, want %d", gotLen, len(payload))
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload mismatch: got %q, want %q", gotPayload, payload)
	}
}

func TestUDPConnOverTCP_ReadUnwraps(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ctx := context.Background()
	conn, err := framing.NewConn(ctx, client)
	if err != nil {
		t.Fatalf("framing.NewConn: %v", err)
	}

	payload := []byte("unwrap this")

	// Write a framed packet from the server side
	go func() {
		frame := make([]byte, 2+len(payload))
		binary.BigEndian.PutUint16(frame[:2], uint16(len(payload)))
		copy(frame[2:], payload)
		_, _ = server.Write(frame)
	}()

	buf := make([]byte, 65536)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Read returned n=%d, want %d", n, len(payload))
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("Read payload mismatch: got %q, want %q", buf[:n], payload)
	}
}

func TestUDPConnOverTCP_RoundTrip(t *testing.T) {
	serverPipe, clientPipe := net.Pipe()
	defer serverPipe.Close()
	defer clientPipe.Close()

	ctx := context.Background()
	writer, _ := framing.NewConn(ctx, clientPipe)
	reader, _ := framing.NewConn(ctx, serverPipe)

	messages := []string{"first", "second packet", "third with more data here!"}

	// Writer sends all messages
	go func() {
		for _, msg := range messages {
			_, _ = writer.Write([]byte(msg))
		}
	}()

	// Reader receives all messages
	buf := make([]byte, 65536)
	for _, want := range messages {
		n, err := reader.Read(buf)
		if err != nil {
			t.Errorf("Read error: %v", err)
			return
		}
		got := string(buf[:n])
		if got != want {
			t.Errorf("Read got %q, want %q", got, want)
		}
	}
}

func TestUDPConnOverTCP_ContextCancellation(t *testing.T) {
	_, client := net.Pipe()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	conn, _ := framing.NewConn(ctx, client)

	cancel()

	buf := make([]byte, 1024)
	_, err := conn.Read(buf)
	if err == nil {
		t.Fatal("Read on cancelled context should return error")
	}
}

// ---------------------------------------------------------------------------
// ReadDatagram tests
// ---------------------------------------------------------------------------

func TestWriteDatagram_Format(t *testing.T) {
	// framing.WriteDatagram outputs [2-byte len][payload], stamping the length in place into the
	// reserved headroom. The payload must sit at buf[datagramHeaderLen:].
	data := make([]byte, 65536)
	payload := []byte("datagram test payload")
	copy(data[datagramHeaderLen:], payload)

	var buf bytes.Buffer
	err := framing.WriteDatagram(&buf, data, len(payload))
	if err != nil {
		t.Fatalf("framing.WriteDatagram error: %v", err)
	}

	written := buf.Bytes()
	if len(written) < 2+len(payload) {
		t.Fatalf("written length = %d, want at least %d", len(written), 2+len(payload))
	}

	gotLen := binary.BigEndian.Uint16(written[:2])
	if int(gotLen) != len(payload) {
		t.Fatalf("length prefix = %d, want %d", gotLen, len(payload))
	}
	if !bytes.Equal(written[2:2+len(payload)], payload) {
		t.Fatalf("payload mismatch: got %q, want %q", written[2:2+len(payload)], payload)
	}
}

func TestReadDatagram_ReadRoundTrip(t *testing.T) {
	payload := []byte("round trip datagram")

	// Frame it manually
	frame := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(payload)))
	copy(frame[2:], payload)

	// Read it back via framing.ReadDatagram
	reader := bytes.NewReader(frame)
	buf := make([]byte, 65536)
	n, err := framing.ReadDatagram(reader, buf)
	if err != nil {
		t.Fatalf("framing.ReadDatagram error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("payload length = %d, want %d", n, len(payload))
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("data mismatch: got %q, want %q", buf[:n], payload)
	}
}

func TestReadDatagram_ReadShortHeaderReturnsError(t *testing.T) {
	// Only 1 byte — cannot read 2-byte length header
	reader := bytes.NewReader([]byte{0x00})
	buf := make([]byte, 65536)
	_, err := framing.ReadDatagram(reader, buf)
	if err == nil {
		t.Fatal("framing.ReadDatagram with short header should return error")
	}
}

func TestReadDatagram_ReadTruncatedBodyReturnsError(t *testing.T) {
	// Header says 10 bytes but only 3 bytes of body follow
	frame := make([]byte, 5)
	binary.BigEndian.PutUint16(frame[:2], 10)
	copy(frame[2:], []byte("abc"))

	reader := bytes.NewReader(frame)
	buf := make([]byte, 65536)
	_, err := framing.ReadDatagram(reader, buf)
	if err == nil {
		t.Fatal("framing.ReadDatagram with truncated body should return error")
	}
}
