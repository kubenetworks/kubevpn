package core

import (
	"bytes"
	"context"
	"encoding/binary"
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

	buffered := NewBufferedTCP(ctx, client)

	payload := []byte("hello buffered tcp")
	n, err := buffered.Write(payload)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write returned n=%d, want %d", n, len(payload))
	}

	// Read from the server side — the background goroutine should have forwarded the data
	buf := make([]byte, 256)
	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	rn, err := server.Read(buf)
	if err != nil {
		t.Fatalf("server Read error: %v", err)
	}
	if !bytes.Equal(buf[:rn], payload) {
		t.Fatalf("got %q, want %q", buf[:rn], payload)
	}
}

func TestBufferedTCP_WriteEmptyIsNoop(t *testing.T) {
	_, client := net.Pipe()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buffered := NewBufferedTCP(ctx, client)
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

	buffered := NewBufferedTCP(ctx, client)

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

	buffered := NewBufferedTCP(ctx, client)
	_ = buffered.Close()

	_, err := buffered.Write([]byte("after close"))
	if err == nil {
		t.Fatal("Write after Close should return error")
	}
}

// ---------------------------------------------------------------------------
// UDPConnOverTCP tests
// ---------------------------------------------------------------------------

func TestUDPConnOverTCP_WriteWrapsWithLengthPrefix(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ctx := context.Background()
	conn, err := NewUDPConnOverTCP(ctx, client)
	if err != nil {
		t.Fatalf("NewUDPConnOverTCP: %v", err)
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
	conn, err := NewUDPConnOverTCP(ctx, client)
	if err != nil {
		t.Fatalf("NewUDPConnOverTCP: %v", err)
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
	writer, _ := NewUDPConnOverTCP(ctx, clientPipe)
	reader, _ := NewUDPConnOverTCP(ctx, serverPipe)

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
	conn, _ := NewUDPConnOverTCP(ctx, client)

	cancel()

	buf := make([]byte, 1024)
	_, err := conn.Read(buf)
	if err == nil {
		t.Fatal("Read on cancelled context should return error")
	}
}

// ---------------------------------------------------------------------------
// DatagramPacket tests
// ---------------------------------------------------------------------------

func TestDatagramPacket_WriteFormat(t *testing.T) {
	// writeDatagram outputs [2-byte len][payload], stamping the length in place into the
	// reserved headroom. The payload must sit at buf[datagramHeaderLen:].
	data := make([]byte, 65536)
	payload := []byte("datagram test payload")
	copy(data[datagramHeaderLen:], payload)

	var buf bytes.Buffer
	err := writeDatagram(&buf, data, len(payload))
	if err != nil {
		t.Fatalf("writeDatagram error: %v", err)
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

func TestDatagramPacket_ReadRoundTrip(t *testing.T) {
	payload := []byte("round trip datagram")

	// Frame it manually
	frame := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(payload)))
	copy(frame[2:], payload)

	// Read it back via readDatagramPacket
	reader := bytes.NewReader(frame)
	buf := make([]byte, 65536)
	dgram, err := readDatagramPacket(reader, buf)
	if err != nil {
		t.Fatalf("readDatagramPacket error: %v", err)
	}
	if int(dgram.DataLength) != len(payload) {
		t.Fatalf("DataLength = %d, want %d", dgram.DataLength, len(payload))
	}
	if !bytes.Equal(dgram.Data[:dgram.DataLength], payload) {
		t.Fatalf("data mismatch: got %q, want %q", dgram.Data[:dgram.DataLength], payload)
	}
}

func TestDatagramPacket_ReadShortHeaderReturnsError(t *testing.T) {
	// Only 1 byte — cannot read 2-byte length header
	reader := bytes.NewReader([]byte{0x00})
	buf := make([]byte, 65536)
	_, err := readDatagramPacket(reader, buf)
	if err == nil {
		t.Fatal("readDatagramPacket with short header should return error")
	}
}

func TestDatagramPacket_ReadTruncatedBodyReturnsError(t *testing.T) {
	// Header says 10 bytes but only 3 bytes of body follow
	frame := make([]byte, 5)
	binary.BigEndian.PutUint16(frame[:2], 10)
	copy(frame[2:], []byte("abc"))

	reader := bytes.NewReader(frame)
	buf := make([]byte, 65536)
	_, err := readDatagramPacket(reader, buf)
	if err == nil {
		t.Fatal("readDatagramPacket with truncated body should return error")
	}
}

// ---------------------------------------------------------------------------
// Node tests
// ---------------------------------------------------------------------------

func TestNodeParseBasic(t *testing.T) {
	tests := []struct {
		input    string
		protocol string
		addr     string
		forward  string
	}{
		{"gtcp://:10801", "gtcp", ":10801", ""},
		{"gudp://:10802", "gudp", ":10802", ""},
		{"ssh://192.168.1.1:22", "ssh", "192.168.1.1:22", ""},
		{"tun://", "tun", "", ""},
		{"tun:///tcp://10.0.0.1:8422", "tun", "", "tcp://10.0.0.1:8422"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			node, err := ParseNode(tt.input)
			if err != nil {
				t.Fatalf("ParseNode(%q) error: %v", tt.input, err)
			}
			if node.Protocol != tt.protocol {
				t.Errorf("Protocol = %q, want %q", node.Protocol, tt.protocol)
			}
			if node.Addr != tt.addr {
				t.Errorf("Addr = %q, want %q", node.Addr, tt.addr)
			}
			if node.Forward != tt.forward {
				t.Errorf("Forward = %q, want %q", node.Forward, tt.forward)
			}
		})
	}
}

func TestNodeParseWithParams(t *testing.T) {
	input := "tun://?net=198.18.0.100/16&mtu=1500&name=utun9"
	node, err := ParseNode(input)
	if err != nil {
		t.Fatalf("ParseNode error: %v", err)
	}
	if node.Protocol != "tun" {
		t.Errorf("Protocol = %q, want tun", node.Protocol)
	}
	if node.Get("net") != "198.18.0.100/16" {
		t.Errorf("net param = %q, want 198.18.0.100/16", node.Get("net"))
	}
	if node.GetInt("mtu") != 1500 {
		t.Errorf("mtu param = %d, want 1500", node.GetInt("mtu"))
	}
	if node.Get("name") != "utun9" {
		t.Errorf("name param = %q, want utun9", node.Get("name"))
	}
}

func TestNodeParseErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
		{"bare path no colon", "/some/path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseNode(tt.input)
			if err == nil {
				t.Fatalf("ParseNode(%q) should return error", tt.input)
			}
		})
	}
}

func TestNodeStringRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		node *Node
	}{
		{
			"simple gtcp",
			NewNode("gtcp", ":10801"),
		},
		{
			"tun with params",
			NewNode("tun", "").
				WithParam("net", "198.18.0.100/16").
				WithParam("mtu", "1500"),
		},
		{
			"tun with forward",
			NewNode("tun", "").
				WithForward("tcp://10.0.0.1:8422"),
		},
		{
			"ssh with address",
			NewNode("ssh", "192.168.1.1:22"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.node.String()
			parsed, err := ParseNode(s)
			if err != nil {
				t.Fatalf("ParseNode(%q) error: %v", s, err)
			}
			if parsed.Protocol != tt.node.Protocol {
				t.Errorf("Protocol mismatch: got %q, want %q", parsed.Protocol, tt.node.Protocol)
			}
			if parsed.Addr != tt.node.Addr {
				t.Errorf("Addr mismatch: got %q, want %q", parsed.Addr, tt.node.Addr)
			}
			// Compare query values
			for key, vals := range tt.node.Values {
				for i, v := range vals {
					parsedVals := parsed.Values[key]
					if i >= len(parsedVals) || parsedVals[i] != v {
						t.Errorf("Values[%q][%d] mismatch: got %q, want %q",
							key, i, parsedVals, v)
					}
				}
			}
		})
	}
}

func TestNodeGetAndGetInt(t *testing.T) {
	node := NewNode("tun", "").
		WithParam("mtu", "9000").
		WithParam("empty", "")

	if node.GetInt("mtu") != 9000 {
		t.Errorf("GetInt(mtu) = %d, want 9000", node.GetInt("mtu"))
	}
	if node.GetInt("missing") != 0 {
		t.Errorf("GetInt(missing) = %d, want 0", node.GetInt("missing"))
	}
	if node.GetInt("empty") != 0 {
		t.Errorf("GetInt(empty) = %d, want 0", node.GetInt("empty"))
	}
	if node.Get("missing") != "" {
		t.Errorf("Get(missing) = %q, want empty", node.Get("missing"))
	}
}

func TestNodeNewNodeBuilderPattern(t *testing.T) {
	node := NewNode("tun", "").
		WithForward("tcp://10.0.0.1:8422").
		WithParam("net", "198.18.0.102/16").
		WithParam("route", "10.233.0.0/16,10.233.64.0/18")

	if node.Protocol != "tun" {
		t.Errorf("Protocol = %q, want tun", node.Protocol)
	}
	if node.Forward != "tcp://10.0.0.1:8422" {
		t.Errorf("Forward = %q, want tcp://10.0.0.1:8422", node.Forward)
	}
	if node.Get("route") != "10.233.0.0/16,10.233.64.0/18" {
		t.Errorf("route = %q", node.Get("route"))
	}
}
