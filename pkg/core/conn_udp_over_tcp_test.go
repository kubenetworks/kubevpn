package core

import (
	"bytes"
	"testing"
)

// Regression: writeDatagram must be idempotent, because WriteFunc may call it more than
// once on the same buffer (retrying the next connection). It writes the length header in
// place into the reserved headroom, so repeated calls produce the identical frame.
func TestWriteDatagram_Idempotent(t *testing.T) {
	payload := []byte{0x45, 0xaa, 0xbb, 0xcc, 0xdd}
	data := make([]byte, 64)
	copy(data[datagramHeaderLen:], payload)

	var first, second bytes.Buffer
	if err := writeDatagram(&first, data, len(payload)); err != nil {
		t.Fatalf("first writeDatagram: %v", err)
	}
	if err := writeDatagram(&second, data, len(payload)); err != nil {
		t.Fatalf("second writeDatagram: %v", err)
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
