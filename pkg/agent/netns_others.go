//go:build !linux

package agent

import "fmt"

// EnterNetns is not implemented on non-Linux platforms.
func EnterNetns(procRoot string, pid int) error {
	return fmt.Errorf("EnterNetns is only supported on Linux")
}
