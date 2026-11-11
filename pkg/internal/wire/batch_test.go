package wire

import (
	"bytes"
	"net"
	"testing"
)

func TestBatchWriterStampsReservedHeaders(t *testing.T) {
	payloads := [][]byte{{0, 0x45, 1, 2}, {1, 0x60, 3, 4}, {2, 8, 0}}
	frames := make([][]byte, 0, len(payloads))
	var want []byte
	for _, payload := range payloads {
		frame := append([]byte{0xff, 0xff}, payload...)
		frames = append(frames, frame)
		want = append(want, goldenFrame(payload)...)
	}
	got := goldenWrite(t, func(conn net.Conn) error {
		var writer BatchWriter
		_, err := writer.Write(conn, frames)
		return err
	})
	if !bytes.Equal(got, want) {
		t.Fatalf("batch bytes = %x, want %x", got, want)
	}
}

func TestBatchWriterValidatesWholeBatchBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		invalid []byte
	}{
		{name: "missing-header", invalid: []byte{1}},
		{name: "oversized", invalid: make([]byte, HeaderLen+maxDatagramPayload+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := batchTCPPair(t, false)
			tracked := &framingConn{Conn: raw}
			first := append([]byte{0xff, 0xff}, []byte{0, 0x45, 1}...)
			var writer BatchWriter
			n, err := writer.Write(tracked, [][]byte{first, tc.invalid})
			if err == nil || n != 0 {
				t.Fatalf("invalid batch n=%d err=%v", n, err)
			}
			if tracked.writes.Load() != 0 {
				t.Fatal("invalid suffix reached the socket after a valid prefix")
			}
			if first[0] != 0xff || first[1] != 0xff {
				t.Fatal("invalid batch mutated an earlier frame before validation completed")
			}
		})
	}
}
