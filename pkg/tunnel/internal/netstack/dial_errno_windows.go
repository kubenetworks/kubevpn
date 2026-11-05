//go:build windows

package netstack

import (
	"errors"
	"syscall"
)

// Windows socket errors are WSA* values; syscall.ECONNREFUSED and friends are
// invented APPLICATION_ERROR numbers that real dial errors never carry, so the
// WSA errnos must be matched directly.
const (
	wsaECONNREFUSED = syscall.Errno(10061)
	wsaENETUNREACH  = syscall.Errno(10051)
	wsaEHOSTUNREACH = syscall.Errno(10065)
)

func dialConnRefused(err error) bool {
	return errors.Is(err, wsaECONNREFUSED)
}

func dialUnreachable(err error) bool {
	return errors.Is(err, wsaENETUNREACH) || errors.Is(err, wsaEHOSTUNREACH)
}
