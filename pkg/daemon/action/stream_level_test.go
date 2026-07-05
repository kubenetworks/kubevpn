package action

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestServerStreamLogger_FileAlwaysDebug_StreamGated locks the two logging rules:
//  1. the daemon log file always records at Debug (full record for `kubevpn logs`), regardless
//     of the requested stream level;
//  2. what is streamed to the CLI is gated by the request's level — Debug lines reach the CLI only
//     when the level is Debug; a zero/absent level is treated as Info (not PanicLevel).
func TestServerStreamLogger_FileAlwaysDebug_StreamGated(t *testing.T) {
	cases := []struct {
		name            string
		level           int32
		wantDebugStream bool
	}{
		{"debug streams debug", int32(log.DebugLevel), true},
		{"info hides debug", int32(log.InfoLevel), false},
		{"zero treated as info", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var file bytes.Buffer
			var stream strings.Builder
			logger := newServerStreamLogger(&file, tc.level, func(msg string) error {
				stream.WriteString(msg)
				return nil
			})

			logger.Debug("DBG-LINE")
			logger.Info("INF-LINE")

			// File side: always full Debug.
			if f := file.String(); !strings.Contains(f, "DBG-LINE") || !strings.Contains(f, "INF-LINE") {
				t.Fatalf("file should record both Debug and Info, got %q", f)
			}

			// Stream side: Info always; Debug only when requested.
			out := stream.String()
			if !strings.Contains(out, "INF-LINE") {
				t.Fatalf("info should always stream, got %q", out)
			}
			if got := strings.Contains(out, "DBG-LINE"); got != tc.wantDebugStream {
				t.Fatalf("debug streamed=%v want %v (level=%d), stream=%q", got, tc.wantDebugStream, tc.level, out)
			}
		})
	}
}
