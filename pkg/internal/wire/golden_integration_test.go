package wire

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"testing"
	"testing/iotest"
	"time"
)

type goldenFrameFixture struct {
	name    string
	payload []byte
	sha256  string
}

// legacyGoldenFrames fixes the bytes produced by the released framing protocol.
// Large payloads are deterministic inputs; their expected digests are static and
// must never be regenerated from the implementation under test.
func legacyGoldenFrames() []goldenFrameFixture {
	ipv4 := []byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 1, 0, 0, 198, 18, 0, 5, 198, 18, 0, 1}
	ipv6 := []byte{0x60, 0, 0, 0, 0, 0, 58, 64}
	ipv6 = append(ipv6, []byte{0x20, 0x01, 0, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}...)
	ipv6 = append(ipv6, []byte{0x20, 0x01, 0, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}...)
	fixtures := []goldenFrameFixture{
		{name: "empty", sha256: "96a296d224f285c67bee93c30f8a309157f0daa35dc5b87e410b78630a09cfc7"},
		{name: "control-declaration", payload: []byte{2}, sha256: "ae4b3280e56e2faf83f414a6e3dabe9d5fbe18976544c05fed121accb85b53fc"},
		{name: "tun-ipv4", payload: append([]byte{0}, ipv4...), sha256: "eb741245b8e7f9a1dc34c502ff550e2a05b6fc88ed114df72fcbd9d9d187a306"},
		{name: "gvisor-ipv6", payload: append([]byte{1}, ipv6...), sha256: "3a1a8a8d512456d787c9ec26da95807f1cabb4d17dab78230138cddc0ac071af"},
		{name: "control-icmpv4", payload: []byte{2, 8, 0, 0xf7, 0xff, 0, 1, 0, 1}, sha256: "5dc0b2d8028548f4021e7bb9baf9da3d0ddbcee2d3256fe6481333ff9735ad09"},
		{name: "one-byte", payload: []byte{0xa5}, sha256: "3bdbd6b62621c64b6d929e0f57eaa62b20d03ab335221644ba65a25de8f5478d"},
	}
	for _, size := range []int{65534, 65535} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte((i*31 + 7) % 251)
		}
		digest := "b54cfe682ab491dc2831f23c1220ab1e80668d382aa0bda07d7fd5c644a7755b"
		if size == 65535 {
			digest = "b15a88342b949e72136e22e57aa772bb8a2997041153c3e95ba0ef106635c893"
		}
		fixtures = append(fixtures, goldenFrameFixture{name: fmt.Sprintf("boundary-%d", size), payload: payload, sha256: digest})
	}
	return fixtures
}

func goldenFrame(payload []byte) []byte {
	frame := make([]byte, HeaderLen+len(payload))
	binary.BigEndian.PutUint16(frame[:HeaderLen], uint16(len(payload)))
	copy(frame[HeaderLen:], payload)
	return frame
}

func goldenWrite(t *testing.T, write func(net.Conn) error) []byte {
	t.Helper()
	raw, peer := net.Pipe()
	defer peer.Close()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(peer)
		done <- result{data: data, err: err}
	}()
	if err := write(raw); err != nil {
		_ = raw.Close()
		t.Fatalf("golden write: %v", err)
	}
	_ = raw.Close()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("read golden wire bytes: %v", got.err)
		}
		return got.data
	case <-time.After(3 * time.Second):
		t.Fatal("golden writer did not finish")
		return nil
	}
}

func TestGoldenFramingCompatibility(t *testing.T) {
	fixtures := legacyGoldenFrames()
	var concatenated []byte
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			want := goldenFrame(fixture.payload)
			digest := sha256.Sum256(want)
			if hex.EncodeToString(digest[:]) != fixture.sha256 {
				t.Fatalf("fixture digest mismatch: got %s want %s", hex.EncodeToString(digest[:]), fixture.sha256)
			}

			storage := append([]byte{0, 0}, fixture.payload...)
			got := goldenWrite(t, func(conn net.Conn) error {
				return WriteDatagram(conn, storage, len(fixture.payload))
			})
			if !bytes.Equal(got, want) {
				t.Fatal("WriteDatagram changed released wire bytes")
			}

			got = goldenWrite(t, func(raw net.Conn) error {
				conn, err := NewConn(context.Background(), raw)
				if err != nil {
					return err
				}
				defer conn.Close()
				n, err := conn.Write(fixture.payload)
				if err == nil && n != len(fixture.payload) {
					return io.ErrShortWrite
				}
				return err
			})
			if !bytes.Equal(got, want) {
				t.Fatal("Conn.Write changed released wire bytes")
			}

			batchInput := goldenFrame(fixture.payload)
			got = goldenWrite(t, func(conn net.Conn) error {
				var writer BatchWriter
				n, err := writer.Write(conn, [][]byte{batchInput})
				if err == nil && n != len(want) {
					return io.ErrShortWrite
				}
				return err
			})
			if !bytes.Equal(got, want) {
				t.Fatal("BatchWriter changed released wire bytes")
			}
		})
		concatenated = append(concatenated, goldenFrame(fixture.payload)...)
	}

	reader := iotest.OneByteReader(bytes.NewReader(concatenated))
	for i, fixture := range fixtures {
		buf := make([]byte, maxDatagramPayload)
		n, err := ReadDatagram(reader, buf)
		if err != nil {
			t.Fatalf("fragmented fixture %d: %v", i, err)
		}
		if !bytes.Equal(buf[:n], fixture.payload) {
			t.Fatalf("fragmented fixture %d payload changed", i)
		}
	}
}

func TestGoldenFramingThroughTLS(t *testing.T) {
	fixtures := legacyGoldenFrames()
	client, server := batchTCPPair(t, true)
	_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	frames := make([][]byte, 0, len(fixtures))
	total := 0
	for _, fixture := range fixtures {
		frame := goldenFrame(fixture.payload)
		frames = append(frames, frame)
		total += len(frame)
	}
	writeDone := make(chan error, 1)
	go func() {
		var writer BatchWriter
		n, err := writer.Write(client, frames)
		if err == nil && n != total {
			err = io.ErrShortWrite
		}
		writeDone <- err
	}()
	for i, fixture := range fixtures {
		buf := make([]byte, maxDatagramPayload)
		n, err := ReadDatagram(server, buf)
		if err != nil {
			t.Fatalf("TLS fixture %d: %v", i, err)
		}
		if !bytes.Equal(buf[:n], fixture.payload) {
			t.Fatalf("TLS fixture %d payload changed", i)
		}
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("TLS batch write: %v", err)
	}
}
