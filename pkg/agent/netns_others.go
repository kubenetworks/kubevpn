//go:build !linux

package agent

import "fmt"

// EnterNetns is not implemented on non-Linux platforms.
func EnterNetns(procRoot string, pid int) error {
	return fmt.Errorf("enternetns is only supported on linux")
}
