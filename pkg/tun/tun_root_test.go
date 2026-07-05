//go:build tun

package tun

import (
	"net"
	"testing"

	"github.com/containernetworking/cni/pkg/types"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestListenerRequiresRoot(t *testing.T) {
	cfg := Config{
		Name: "utun-test",
		Addr: "198.18.0.100/16",
		MTU:  config.DefaultMTU,
	}
	ln, err := Listener(cfg)
	if err != nil {
		t.Fatalf("Listener() error = %v", err)
	}
	defer ln.Close()
}

func TestAddRoutesRequiresRoot(t *testing.T) {
	routes := []types.Route{
		{
			Dst: net.IPNet{
				IP:   net.ParseIP("192.168.99.0"),
				Mask: net.CIDRMask(24, 32),
			},
		},
	}
	_ = AddRoutes("utun-test", routes...)
}

func TestDeleteRoutesRequiresRoot(t *testing.T) {
	routes := []types.Route{
		{
			Dst: net.IPNet{
				IP:   net.ParseIP("192.168.99.0"),
				Mask: net.CIDRMask(24, 32),
			},
		},
	}
	_ = DeleteRoutes("utun-test", routes...)
}
