package wire

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
)

// pair wraps net.Pipe endpoints so both raw and obfuscated views can be used
// in one test.
func obfPair(t *testing.T) (dialer net.Conn, acceptor net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	return ObfuscateDial(c1), ObfuscateAccept(c2)
}

func TestObfuscatedConn_RoundTrip(t *testing.T) {
	dialer, acceptor := obfPair(t)

	msgs := [][]byte{
		[]byte("hello"),
		bytes.Repeat([]byte{0x00}, 1),     // single byte
		bytes.Repeat([]byte{0xff}, 70000), // spans the 64KB scratch growth path
	}
	for _, m := range msgs {
		go func() { _, _ = dialer.Write(m) }()
		buf := make([]byte, len(m))
		if _, err := io.ReadFull(acceptor, buf); err != nil {
			t.Fatalf("acceptor read: %v", err)
		}
		if !bytes.Equal(buf, m) {
			t.Fatalf("dialer->acceptor mismatch at len=%d", len(m))
		}
	}

	// Acceptor replies once it has consumed the seed.
	reply := []byte("ack")
	go func() { _, _ = acceptor.Write(reply) }()
	buf := make([]byte, len(reply))
	if _, err := io.ReadFull(dialer, buf); err != nil {
		t.Fatalf("dialer read: %v", err)
	}
	if !bytes.Equal(buf, reply) {
		t.Fatal("acceptor->dialer mismatch")
	}
}

func TestObfuscatedConn_KeystreamAdvances(t *testing.T) {
	// Two identical plaintext writes must produce different ciphertext: the
	// keystream is positional, not a fixed pad.
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	w := ObfuscateDial(c1)

	plaintext := bytes.Repeat([]byte("same plaintext twice "), 3)
	go func() {
		_, _ = w.Write(plaintext)
		_, _ = w.Write(plaintext)
	}()

	wire := make([]byte, obfSeedLen+2*len(plaintext))
	if _, err := io.ReadFull(c2, wire); err != nil {
		t.Fatal(err)
	}
	first, second := wire[obfSeedLen:obfSeedLen+len(plaintext)], wire[obfSeedLen+len(plaintext):]
	if bytes.Equal(first, second) {
		t.Fatal("identical plaintexts produced identical ciphertext — keystream not advancing")
	}
	if bytes.Equal(first, plaintext) || bytes.Equal(second, plaintext) {
		t.Fatal("plaintext visible on the wire")
	}
}

func TestObfuscatedConn_WireBytesDiffer(t *testing.T) {
	// The acceptor reads the RAW stream first: bytes must not contain the
	// plaintext, and the first 16 bytes are the transmitted seed.
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	w := ObfuscateDial(c1)

	plaintext := bytes.Repeat([]byte{0x41}, 64) // "AAAA..."
	go func() { _, _ = w.Write(plaintext) }()

	wire := make([]byte, obfSeedLen+len(plaintext))
	if _, err := io.ReadFull(c2, wire); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, plaintext) {
		t.Fatal("plaintext visible on the wire")
	}
	seed := wire[:obfSeedLen]
	payload := wire[obfSeedLen:]
	if bytes.Equal(payload, plaintext) {
		t.Fatal("payload not obfuscated")
	}
	// Keystream derivation must not be identity: at least most bytes change.
	same := 0
	for i, b := range payload {
		if b == plaintext[i] {
			same++
		}
	}
	if same > len(plaintext)/2 {
		t.Fatalf("payload mostly unchanged (%d/%d bytes)", same, len(plaintext))
	}
	if bytes.Equal(seed, payload[:obfSeedLen]) {
		t.Fatal("seed equals payload prefix — seed not random?")
	}
}

func TestObfuscatedConn_AcceptorWriteBeforeSeed(t *testing.T) {
	_, acceptor := obfPair(t)
	_, err := acceptor.Write([]byte("early"))
	if !errors.Is(err, errObfuscationWriteBeforeSeed) {
		t.Fatalf("expected errObfuscationWriteBeforeSeed, got %v", err)
	}
}

func TestObfuscatedConn_DialerWriteCountExcludesSeed(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	w := ObfuscateDial(c1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		sink := make([]byte, obfSeedLen+len("payload"))
		_, _ = io.ReadFull(c2, sink)
	}()
	n, err := w.Write([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if n != len("payload") {
		t.Fatalf("Write returned %d, want %d (seed must not count)", n, len("payload"))
	}
	<-done
}

func TestObfuscatedConn_OverFramingRoundTrip(t *testing.T) {
	// The production stack: framing.Conn over obfuscatedConn over a pipe.
	// Datagrams must round-trip in both directions.
	SetObfuscation(true)
	t.Cleanup(func() { SetObfuscation(false) })

	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	client, err := NewConn(context.Background(), ObfuscateDial(c1))
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewConn(context.Background(), ObfuscateAccept(c2))
	if err != nil {
		t.Fatal(err)
	}

	datagrams := [][]byte{
		bytes.Repeat([]byte{0x55}, 128),
		bytes.Repeat([]byte{0xAA}, 4096),
		bytes.Repeat([]byte{0x0A}, maxDatagramPayload), // max frame; with seed+header crosses one 64KB XOR block
	}
	buf := make([]byte, 128*1024)
	for _, dg := range datagrams {
		go func() {
			if _, err := client.Write(dg); err != nil {
				t.Errorf("client Write: %v", err)
			}
		}()
		n, err := server.Read(buf)
		if err != nil {
			t.Fatalf("server Read: %v", err)
		}
		if !bytes.Equal(buf[:n], dg) {
			t.Fatalf("datagram mismatch at len=%d (got %d)", len(dg), n)
		}
	}

	// Server back to client through the framed write path.
	for _, dg := range datagrams {
		go func() {
			if _, err := server.Write(dg); err != nil {
				t.Errorf("server Write: %v", err)
			}
		}()
		n, err := client.Read(buf)
		if err != nil {
			t.Fatalf("client Read: %v", err)
		}
		if !bytes.Equal(buf[:n], dg) {
			t.Fatalf("reverse datagram mismatch at len=%d (got %d)", len(dg), n)
		}
	}
}
