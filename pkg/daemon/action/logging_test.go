package action

import (
	"bytes"
	"context"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// TestServerStreamLogger_FileAndStreamFollowLevel verifies that BOTH the daemon log file and the
// gRPC stream to the CLI follow the request's level (req.Level): Info by default, Debug with
// --debug. A non-debug connection records no Debug lines anywhere (file included), so per-packet
// tracing never floods the file unless the user asked for it.
func TestServerStreamLogger_FileAndStreamFollowLevel(t *testing.T) {
	t.Run("Info: neither file nor stream has Debug", func(t *testing.T) {
		var file, stream bytes.Buffer
		logger := newServerStreamLogger(&file, int32(log.InfoLevel), func(msg string) error {
			stream.WriteString(msg)
			return nil
		})
		logger.Debug("dbg-line")
		logger.Info("info-line")

		if strings.Contains(file.String(), "dbg-line") {
			t.Fatalf("file should NOT contain Debug at Info level, got: %q", file.String())
		}
		if !strings.Contains(file.String(), "info-line") {
			t.Fatalf("file should contain Info, got: %q", file.String())
		}
		if strings.Contains(stream.String(), "dbg-line") {
			t.Fatalf("stream should NOT contain Debug at Info level, got: %q", stream.String())
		}
		if !strings.Contains(stream.String(), "info-line") {
			t.Fatalf("stream should contain Info, got: %q", stream.String())
		}
	})

	t.Run("Debug (--debug): both file and stream have Debug", func(t *testing.T) {
		var file, stream bytes.Buffer
		logger := newServerStreamLogger(&file, int32(log.DebugLevel), func(msg string) error {
			stream.WriteString(msg)
			return nil
		})
		logger.Debug("dbg-line")

		if !strings.Contains(file.String(), "dbg-line") {
			t.Fatalf("file should contain Debug when streamLevel=Debug, got: %q", file.String())
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
