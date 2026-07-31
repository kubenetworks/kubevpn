package action

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestServerStreamLogger_FileAndStreamFollowLevel locks the logging rule: both the daemon log
// file and the CLI stream follow the request's level. Debug lines are recorded (file) and streamed
// (CLI) only when the level is Debug; Info always shows on both; a zero/absent level is treated as
// Info (not PanicLevel).
func TestServerStreamLogger_LevelTable(t *testing.T) {
	cases := []struct {
		name      string
		level     int32
		wantDebug bool
	}{
		{"debug records+streams debug", int32(log.DebugLevel), true},
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

			// Both file and stream: Info always; Debug only when the level is Debug.
			f := file.String()
			if !strings.Contains(f, "INF-LINE") {
				t.Fatalf("info should always record to file, got %q", f)
			}
			if got := strings.Contains(f, "DBG-LINE"); got != tc.wantDebug {
				t.Fatalf("file debug=%v want %v (level=%d), file=%q", got, tc.wantDebug, tc.level, f)
			}
			out := stream.String()
			if !strings.Contains(out, "INF-LINE") {
				t.Fatalf("info should always stream, got %q", out)
			}
			if got := strings.Contains(out, "DBG-LINE"); got != tc.wantDebug {
				t.Fatalf("stream debug=%v want %v (level=%d), stream=%q", got, tc.wantDebug, tc.level, out)
			}
		})
	}
}
