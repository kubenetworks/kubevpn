package action

import (
	"bytes"
	"context"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// TestServerStreamLogger_FileAlwaysDebug_StreamFollowsLevel verifies Req1:
// the daemon log file always captures Debug, while the gRPC stream to the CLI
// is filtered to streamLevel (Info by default, Debug when --debug).
func TestServerStreamLogger_FileAlwaysDebug_StreamFollowsLevel(t *testing.T) {
	t.Run("stream Info: file has Debug, stream does not", func(t *testing.T) {
		var file, stream bytes.Buffer
		logger := newServerStreamLogger(&file, int32(log.InfoLevel), func(msg string) error {
			stream.WriteString(msg)
			return nil
		})
		logger.Debug("dbg-line")
		logger.Info("info-line")

		if !strings.Contains(file.String(), "dbg-line") || !strings.Contains(file.String(), "info-line") {
			t.Fatalf("file should contain both Debug and Info, got: %q", file.String())
		}
		if strings.Contains(stream.String(), "dbg-line") {
			t.Fatalf("stream should NOT contain Debug at Info level, got: %q", stream.String())
		}
		if !strings.Contains(stream.String(), "info-line") {
			t.Fatalf("stream should contain Info, got: %q", stream.String())
		}
	})

	t.Run("stream Debug (--debug): stream also has Debug", func(t *testing.T) {
		var file, stream bytes.Buffer
		logger := newServerStreamLogger(&file, int32(log.DebugLevel), func(msg string) error {
			stream.WriteString(msg)
			return nil
		})
		logger.Debug("dbg-line")

		if !strings.Contains(file.String(), "dbg-line") {
			t.Fatalf("file should contain Debug, got: %q", file.String())
		}
		if !strings.Contains(stream.String(), "dbg-line") {
			t.Fatalf("stream should contain Debug when streamLevel=Debug, got: %q", stream.String())
		}
	})
}

// TestConnIDTag_FileOnly verifies Req2: the connID context field renders as a
// [connID=...] prefix in the server-format file output, but never reaches the
// CLI stream (message-only).
func TestConnIDTag_FileOnly(t *testing.T) {
	var file, stream bytes.Buffer
	logger := newServerStreamLogger(&file, int32(log.InfoLevel), func(msg string) error {
		stream.WriteString(msg)
		return nil
	})
	ctx := plog.WithLogger(context.Background(), logger)
	ctx = plog.WithField(ctx, LogFieldConnID, "abc123def456")

	plog.G(ctx).Info("hello world")

	if !strings.Contains(file.String(), "[connID=abc123def456]") {
		t.Fatalf("file should carry connID prefix, got: %q", file.String())
	}
	if strings.Contains(stream.String(), "connID") {
		t.Fatalf("stream (CLI) must not contain connID prefix, got: %q", stream.String())
	}
	if !strings.Contains(stream.String(), "hello world") {
		t.Fatalf("stream should still carry the message, got: %q", stream.String())
	}
}
