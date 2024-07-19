package dhcp

import (
	"encoding/base64"
	"net"
	"testing"

	"github.com/cilium/ipam/service/allocator"
	"github.com/cilium/ipam/service/ipallocator"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestName(t *testing.T) {
	cidr := &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}
	dhcp, err := ipallocator.NewAllocatorCIDRRange(cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := "Aw=="
	var str []byte
	str, err = base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	err = dhcp.Restore(cidr, str)
	if err != nil {
		t.Fatal(err)
	}
	next, err := dhcp.AllocateNext()
	if err != nil {
		t.Fatal(err)
	}
	t.Log(next.String())
	_, bytes, _ := dhcp.Snapshot()
	t.Log(string(bytes))
}

func TestInit(t *testing.T) {
	cidr := &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask}
	dhcp, err := ipallocator.NewAllocatorCIDRRange(cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, bytes, err := dhcp.Snapshot()
	t.Log(string(snapshot), string(bytes), err)
}
