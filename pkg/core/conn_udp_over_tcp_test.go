package core

import (
	"bytes"
	"testing"
)

// Regression: DatagramPacket.Write must not mutate d.Data in place, because
// WriteFunc may call it more than once on the same packet (retrying the next
// connection). A second call previously produced a corrupted frame.
func TestDatagramPacket_WriteIdempotent(t *testing.T) {
	payload := []byte{0x45, 0xaa, 0xbb, 0xcc, 0xdd}
	data := make([]byte, 64)
	copy(data, payload)
	d := &DatagramPacket{DataLength: uint16(len(payload)), Data: data}

	var first, second bytes.Buffer
	if err := d.Write(&first); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := d.Write(&second); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("Write is not idempotent: first=%x second=%x", first.Bytes(), second.Bytes())
	}

	// Frame must be a 2-byte big-endian length prefix followed by the payload.
	want := append([]byte{0x00, byte(len(payload))}, payload...)
	if !bytes.Equal(first.Bytes(), want) {
		t.Fatalf("frame mismatch: got %x want %x", first.Bytes(), want)
	}
}
