package dhcp

import (
	"encoding/base64"
	"net"
	"testing"

	"github.com/cilium/ipam/service/allocator"
	"github.com/cilium/ipam/service/ipallocator"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func newAllocator(t *testing.T, cidr *net.IPNet, snapshot string) *ipallocator.Range {
	t.Helper()
	alloc, err := ipallocator.NewAllocatorCIDRRange(cidr, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		t.Fatalf("create allocator: %v", err)
	}
	if snapshot != "" {
		b, err := base64.StdEncoding.DecodeString(snapshot)
		if err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		if err = alloc.Restore(cidr, b); err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
	return alloc
}

func snapshot(t *testing.T, alloc *ipallocator.Range) string {
	t.Helper()
	_, b, err := alloc.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestCIDRConfig(t *testing.T) {
	if config.CIDR == nil {
		t.Fatal("config.CIDR is nil")
	}
	if config.CIDR6 == nil {
		t.Fatal("config.CIDR6 is nil")
	}
	ones4, _ := config.CIDR.Mask.Size()
	ones6, _ := config.CIDR6.Mask.Size()
	t.Logf("CIDR: %s (/%d), CIDR6: %s (/%d)", config.CIDR, ones4, config.CIDR6, ones6)
}

func TestAllocateNext_UniqueIPs(t *testing.T) {
	alloc := newAllocator(t, config.CIDR, "")

	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		ip, err := alloc.AllocateNext()
		if err != nil {
			t.Fatalf("allocate #%d: %v", i, err)
		}
		if !config.CIDR.Contains(ip) {
			t.Fatalf("IP %s not in CIDR %s", ip, config.CIDR)
		}
		key := ip.String()
		if seen[key] {
			t.Fatalf("duplicate at #%d: %s", i, key)
		}
		seen[key] = true
	}
}

func TestAllocateNext_IPv6(t *testing.T) {
	alloc := newAllocator(t, config.CIDR6, "")

	ip, err := alloc.AllocateNext()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if !config.CIDR6.Contains(ip) {
		t.Fatalf("IPv6 %s not in CIDR6 %s", ip, config.CIDR6)
	}
	if ip.To4() != nil {
		t.Fatalf("expected IPv6, got IPv4: %s", ip)
	}
}

func TestSnapshotRestore_Preserves(t *testing.T) {
	alloc := newAllocator(t, config.CIDR, "")

	ip1, _ := alloc.AllocateNext()
	ip2, _ := alloc.AllocateNext()

	snap := snapshot(t, alloc)

	// Restore into new allocator
	alloc2 := newAllocator(t, config.CIDR, snap)

	// ip1 and ip2 should be marked as allocated — next should be different
	ip3, err := alloc2.AllocateNext()
	if err != nil {
		t.Fatalf("allocate after restore: %v", err)
	}
	if ip3.Equal(ip1) || ip3.Equal(ip2) {
		t.Fatalf("restored allocator gave already-allocated IP: %s (ip1=%s ip2=%s)", ip3, ip1, ip2)
	}
}

func TestRelease_FreesForReuse(t *testing.T) {
	alloc := newAllocator(t, config.CIDR, "")

	ip1, _ := alloc.AllocateNext()
	_, _ = alloc.AllocateNext()

	// Release ip1
	err := alloc.Release(ip1)
	if err != nil {
		t.Fatalf("release: %v", err)
	}

	// Snapshot + restore (simulates ConfigMap round-trip)
	snap := snapshot(t, alloc)
	alloc2 := newAllocator(t, config.CIDR, snap)

	// Next alloc should get ip1 back (it was released)
	ip3, err := alloc2.AllocateNext()
	if err != nil {
		t.Fatalf("allocate after release: %v", err)
	}
	if !ip1.Equal(ip3) {
		t.Fatalf("expected released IP %s, got %s", ip1, ip3)
	}
}

func TestAllocateSpecific(t *testing.T) {
	alloc := newAllocator(t, config.CIDR, "")

	target := net.ParseIP("198.18.0.50")
	err := alloc.Allocate(target)
	if err != nil {
		t.Fatalf("allocate specific: %v", err)
	}

	// Should not get this IP from AllocateNext
	for i := 0; i < 100; i++ {
		ip, err := alloc.AllocateNext()
		if err != nil {
			break
		}
		if ip.Equal(target) {
			t.Fatalf("AllocateNext returned specifically-allocated IP: %s", ip)
		}
	}
}

func TestBase64Encoding(t *testing.T) {
	alloc := newAllocator(t, config.CIDR, "")
	_, _ = alloc.AllocateNext()

	snap := snapshot(t, alloc)
	if snap == "" {
		t.Fatal("empty snapshot")
	}

	// Verify it's valid base64
	_, err := base64.StdEncoding.DecodeString(snap)
	if err != nil {
		t.Fatalf("snapshot is not valid base64: %v", err)
	}
}
