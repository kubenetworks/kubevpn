package wire

import (
	"io"
	"net"
	"testing"
)

func TestBatchWriterWritevPartialProgress(t *testing.T) {
	tcpClient, tcpServer := tcpConnPair(t)
	defer tcpClient.Close()
	defer tcpServer.Close()

	frames := [][]byte{make([]byte, 40000), make([]byte, 20000)}
	writer := &BatchWriter{writeToTCP: func(_ *net.TCPConn, buffers net.Buffers) (int64, error) {
		if len(buffers) != 2 || len(buffers[0]) != 40000 || len(buffers[1]) != 20000 {
			t.Fatalf("writev changed frame boundaries: %v", buffers)
		}
		return 45000, io.ErrUnexpectedEOF
	}}
	n, err := writer.Write(tcpClient, frames)
	if n != 45000 || err != io.ErrUnexpectedEOF {
		t.Fatalf("writev progress = %d, %v, want 45000, %v", n, err, io.ErrUnexpectedEOF)
	}

	writer.writeToTCP = func(*net.TCPConn, net.Buffers) (int64, error) { return 0, nil }
	if n, err = writer.Write(tcpClient, frames); n != 0 || err != io.ErrShortWrite {
		t.Fatalf("zero write progress = %d, %v, want 0, %v", n, err, io.ErrShortWrite)
	}
}

func tcpConnPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.Accept()
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	return client.(*net.TCPConn), server.(*net.TCPConn)
}
