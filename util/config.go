package util

import (
	"sync"
	"time"

	"github.com/go-log/log"
)

// Debug is a flag that enables the debug log.
var Debug bool

var (
	tinyBufferSize   = 512
	smallBufferSize  = 2 * 1024  // 2KB small buffer
	mediumBufferSize = 8 * 1024  // 8KB medium buffer
	largeBufferSize  = 32 * 1024 // 32KB large buffer
)

var (
	SPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, smallBufferSize)
		},
	}
	MPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, mediumBufferSize)
		},
	}
	LPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, largeBufferSize)
		},
	}
)

var (
	// KeepAliveTime is the keep alive time period for TCP connection.
	KeepAliveTime = 180 * time.Second
	// DialTimeout is the timeout of dial.
	DialTimeout = 5 * time.Second
	// HandshakeTimeout is the timeout of handshake.
	HandshakeTimeout = 5 * time.Second
	// ConnectTimeout is the timeout for connect.
	ConnectTimeout = 5 * time.Second
	// ReadTimeout is the timeout for reading.
	ReadTimeout = 10 * time.Second
	// WriteTimeout is the timeout for writing.
	WriteTimeout = 10 * time.Second
)

var (
	// DefaultMTU is the default mtu for tun/tap device
	DefaultMTU = 1350
)

// SetLogger sets a new logger for internal log system.
func SetLogger(logger log.Logger) {
	log.DefaultLogger = logger
}
