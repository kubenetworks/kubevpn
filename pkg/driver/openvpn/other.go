//go:build !windows
// +build !windows

package openvpn

import (
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

func Install() error {
	return errors.New("not need to implement")
}
