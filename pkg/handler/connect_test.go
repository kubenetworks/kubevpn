package handler

import (
	"net"
	"testing"

	"github.com/google/gopacket/routing"
	"github.com/libp2p/go-netroute"
)

func TestRoute(t *testing.T) {
	var r routing.Router
	var err error
	r, err = netroute.New()
	if err != nil {
		t.Fatal(err)
	}
	iface, gateway, src, err := r.Route(net.ParseIP("8.8.8.8"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("iface: %s, gateway: %s, src: %s", iface.Name, gateway, src)
}
