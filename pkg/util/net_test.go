package util

import (
	"fmt"
	"net"
	"testing"
)

func TestGetDevice(t *testing.T) {
	ip := net.ParseIP("223.254.0.104")
	device, err := GetTunDevice(ip)
	if err != nil {
		t.Error(err)
	}
	fmt.Println(device.Name)
}
