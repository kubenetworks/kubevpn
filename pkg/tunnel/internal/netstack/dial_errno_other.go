//go:build !windows

package netstack

import (
	"errors"
	"syscall"
)

func dialConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

func dialUnreachable(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH)
}
