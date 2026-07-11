//go:build windows

package tun

import "fmt"

func changeIP(ifName string, oldAddr, newAddr string) error {
	return fmt.Errorf("ChangeIP not yet implemented on Windows")
}
