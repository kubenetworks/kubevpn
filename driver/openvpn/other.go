//go:build !windows
// +build !windows

package openvpn

import (
	"errors"
)

func Install() error {
	return errors.New("not need to implement")
}
