package action

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// TestMakeLogFilter_LenientSemantics verifies the connID/tun filter: a line is kept when it has no
// such tag at all (shared/early/setup logs) or its tag matches; only a line tagged with a DIFFERENT
// value is dropped. Tags must match as whole tokens (no prefix confusion).
func TestMakeLogFilter_LenientSemantics(t *testing.T) {
	const (
		taggedBoth  = `2026-07-27 15:31:15.713 packet.go:152 Info: [connID=7689bab91b63 tun=utun5] recv tcp`
		taggedConn  = `2026-07-27 15:23:17.304 context.go:113 info: [connID=7689bab91b63] Using traffic manager`
		otherConn   = `2026-07-27 15:23:17.304 context.go:113 info: [connID=aaaabbbbcccc] Using traffic manager`
		otherTun    = `2026-07-27 15:31:15.713 packet.go:152 Info: [connID=7689bab91b63 tun=utun50] recv tcp`
		untaggedLog = `2026-07-27 15:23:17.032 jump.go:97 debug: jumped via SSH bastion host to apiserver`
	)

	cases := []struct {
		name     string
		req      *rpc.LogRequest
		line     string
		wantKeep bool
	}{
		{"no filter keeps everything", &rpc.LogRequest{}, otherConn, true},
		{"connID match kept", &rpc.LogRequest{ConnectionID: "7689bab91b63"}, taggedConn, true},
		{"connID mismatch dropped", &rpc.LogRequest{ConnectionID: "7689bab91b63"}, otherConn, false},
		{"connID filter keeps untagged", &rpc.LogRequest{ConnectionID: "7689bab91b63"}, untaggedLog, true},
		{"connID prefix is not a whole-token match", &rpc.LogRequest{ConnectionID: "7689bab91b6"}, taggedConn, false},
		{"tun match kept", &rpc.LogRequest{Tun: "utun5"}, taggedBoth, true},
		{"tun mismatch dropped", &rpc.LogRequest{Tun: "utun5"}, otherTun, false},
		{"tun filter keeps untagged (e.g. user daemon)", &rpc.LogRequest{Tun: "utun5"}, taggedConn, true},
		{"tun prefix is not a whole-token match", &rpc.LogRequest{Tun: "utun5"}, otherTun, false},
		{"connID+tun both match", &rpc.LogRequest{ConnectionID: "7689bab91b63", Tun: "utun5"}, taggedBoth, true},
		{"connID matches but tun mismatches → drop", &rpc.LogRequest{ConnectionID: "7689bab91b63", Tun: "utun5"}, otherTun, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := makeLogFilter(tc.req)(tc.line); got != tc.wantKeep {
				t.Fatalf("keep=%v want %v for line %q", got, tc.wantKeep, tc.line)
			}
		})
	}
}
