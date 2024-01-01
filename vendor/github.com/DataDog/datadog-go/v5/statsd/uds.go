//go:build !windows
// +build !windows

package statsd

import (
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"time"
)

// udsWriter is an internal class wrapping around management of UDS connection
type udsWriter struct {
	// Address to send metrics to, needed to allow reconnection on error
	addr string
	// Transport used
	transport string
	// Established connection object, or nil if not connected yet
	conn net.Conn
	// write timeout
	writeTimeout time.Duration
	sync.RWMutex // used to lock conn / writer can replace it
}

// newUDSWriter returns a pointer to a new udsWriter given a socket file path as addr.
func newUDSWriter(addr string, writeTimeout time.Duration, transport string) (*udsWriter, error) {
	// Defer connection to first Write
	writer := &udsWriter{addr: addr, transport: transport, conn: nil, writeTimeout: writeTimeout}
	return writer, nil
}

// GetTransportName returns the transport used by the writer
func (w *udsWriter) GetTransportName() string {
	w.RLock()
	defer w.RUnlock()

	if w.transport == "unix" {
		return writerNameUDSStream
	} else {
		return writerNameUDS
	}
}

// retryOnWriteErr returns true if we should retry writing after a write error
func (w *udsWriter) retryOnWriteErr(err error, stream bool) bool {
	// Never retry when using unixgram (to preserve the historical behavior)
	if !stream {
		return false
	}
	// Otherwise we retry on timeout because we might have written a partial packet
	if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
		return true
	}
	return false
}

func (w *udsWriter) shouldCloseConnection(err error) bool {
	if err, isNetworkErr := err.(net.Error); err != nil && (!isNetworkErr || !err.Timeout()) {
		// Statsd server disconnected, retry connecting at next packet
		return true
	}
	return false
}

// writeFull writes the whole data to the UDS connection
func (w *udsWriter) writeFull(data []byte, stopIfNoneWritten bool, stream bool) (int, error) {
	written := 0
	for written < len(data) {
		n, e := w.conn.Write(data[written:])
		written += n

		// If we haven't written anything, and we're supposed to stop if we can't write anything, return the error
		if written == 0 && stopIfNoneWritten {
			return written, e
		}

		// If there's an error, check if it is retryable
		if e != nil && !w.retryOnWriteErr(e, stream) {
			return written, e
		}

		// When using "unix" we need to be able to finish to write partially written packets once we have started.
		if stream {
			w.conn.SetWriteDeadline(time.Time{})
		}
	}
	return written, nil
}

// Write data to the UDS connection with write timeout and minimal error handling:
// create the connection if nil, and destroy it if the statsd server has disconnected
func (w *udsWriter) Write(data []byte) (int, error) {
	var n int
	conn, err := w.ensureConnection()
	if err != nil {
		return 0, err
	}
	stream := conn.LocalAddr().Network() == "unix"

	// When using streams the deadline will only make us drop the packet if we can't write it at all,
	// once we've started writing we need to finish.
	conn.SetWriteDeadline(time.Now().Add(w.writeTimeout))

	// When using streams, we append the length of the packet to the data
	if stream {
		bs := []byte{0, 0, 0, 0}
		binary.LittleEndian.PutUint32(bs, uint32(len(data)))
		_, err = w.writeFull(bs, true, true)
		if err == nil {
			n, err = w.writeFull(data, false, true)
		}
	} else {
		n, err = w.writeFull(data, true, false)
	}

	if w.shouldCloseConnection(err) {
		w.unsetConnection()
	}
	return n, err
}

func (w *udsWriter) Close() error {
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}

func (w *udsWriter) tryToDial(network string) (net.Conn, error) {
	udsAddr, err := net.ResolveUnixAddr(network, w.addr)
	if err != nil {
		return nil, err
	}
	newConn, err := net.Dial(udsAddr.Network(), udsAddr.String())
	if err != nil {
		return nil, err
	}
	return newConn, nil
}

func (w *udsWriter) ensureConnection() (net.Conn, error) {
	// Check if we've already got a socket we can use
	w.RLock()
	currentConn := w.conn
	w.RUnlock()

	if currentConn != nil {
		return currentConn, nil
	}

	// Looks like we might need to connect - try again with write locking.
	w.Lock()
	defer w.Unlock()
	if w.conn != nil {
		return w.conn, nil
	}

	var newConn net.Conn
	var err error

	// Try to guess the transport if not specified.
	if w.transport == "" {
		newConn, err = w.tryToDial("unixgram")
		// try to connect with unixgram failed, try again with unix streams.
		if err != nil && strings.Contains(err.Error(), "protocol wrong type for socket") {
			newConn, err = w.tryToDial("unix")
		}
	} else {
		newConn, err = w.tryToDial(w.transport)
	}

	if err != nil {
		return nil, err
	}
	w.conn = newConn
	w.transport = newConn.RemoteAddr().Network()
	return newConn, nil
}

func (w *udsWriter) unsetConnection() {
	w.Lock()
	defer w.Unlock()
	w.conn = nil
}
