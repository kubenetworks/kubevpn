//go:build linux

package tun

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"golang.zx2c4.com/wireguard/tun"
)

func TestStage4LinuxNativeQualification(t *testing.T) {
	// Type qualification only: never call a zero NativeTun's methods.
	d := &batchDevice{dev: &tun.NativeTun{}}
	q, ok := any(d).(interface {
		canReadDirect([][]byte, []int, int) bool
	})
	if !ok {
		t.Fatal("missing native qualification")
	}
	b := config.LPool.New().([]byte)
	if len(b) != 65536 || cap(b) != 65538 {
		t.Fatalf("production factory len/cap=%d/%d", len(b), cap(b))
	}
	if !q.canReadDirect([][]byte{b}, make([]int, 1), 3) {
		t.Fatal("real Linux factory does not qualify")
	}
	for _, off := range []int{0, 3} {
		for _, capacity := range []int{65536, 65538} {
			dst := [][]byte{make([]byte, 65536, capacity)}
			if q.canReadDirect(dst, make([]int, 1), off) != (capacity >= off+65535) {
				t.Fatalf("off/cap %d/%d", off, capacity)
			}
		}
	}
	for _, tc := range []struct {
		b   [][]byte
		s   []int
		off int
	}{
		{nil, nil, 0}, {[][]byte{b}, nil, 3}, {[][]byte{b}, make([]int, 1), -1},
		{[][]byte{b[:2]}, make([]int, 1), 3},
		{[][]byte{b, make([]byte, 65536)}, make([]int, 2), 3},
	} {
		if q.canReadDirect(tc.b, tc.s, tc.off) {
			t.Fatal("invalid direct destination accepted")
		}
	}
}
