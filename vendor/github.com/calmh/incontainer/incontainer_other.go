//go:build !linux
// +build !linux

package incontainer

func Detect() bool {
	return false
}
