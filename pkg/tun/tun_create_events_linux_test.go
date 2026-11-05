package tun

import (
	"net"
	"testing"
)

func init() {
	prepareCreateEventsTest = func(t *testing.T) {
		original := listTunInterfaces
		t.Cleanup(func() { listTunInterfaces = original })
		listTunInterfaces = func() ([]net.Interface, error) { return nil, nil }
	}
}
