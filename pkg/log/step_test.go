package log

import (
	"bytes"
	"context"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestStep_StreamCarriesSentinel_FileStaysClean verifies the two-output contract:
// the gRPC stream receives sentinel-encoded step messages (for the CLI spinner),
// while the daemon log file stays clean (no sentinel, no internal field key).
func TestStep_StreamCarriesSentinel_FileStaysClean(t *testing.T) {
	var fileBuf, streamBuf bytes.Buffer
	logger := GetLoggerForServer(int32(log.DebugLevel), &fileBuf)
	logger.AddHook(&StreamHook{Writer: &streamBuf, Level: log.InfoLevel})
	ctx := WithLogger(context.Background(), logger)

	StepStart(ctx, "Forwarding ports")
	StepDone(ctx, "Forwarded ports (%s)", "TCP/UDP")

	// Stream side: each line is sentinel-encoded and decodes to the right kind.
	lines := nonEmptyLines(streamBuf.String())
	if len(lines) != 2 {
		t.Fatalf("expected 2 stream lines, got %d: %q", len(lines), lines)
	}
	if k, text := DecodeStep(lines[0]); k != StepBegin || text != "Forwarding ports" {
		t.Fatalf("line 0: kind=%v text=%q, want StepBegin/Forwarding ports", k, text)
	}
	if k, text := DecodeStep(lines[1]); k != StepEnd || text != "Forwarded ports (TCP/UDP)" {
		t.Fatalf("line 1: kind=%v text=%q, want StepEnd/Forwarded ports (TCP/UDP)", k, text)
	}

	// File side: clean — no sentinel bytes, no internal field key, real text present.
	file := fileBuf.String()
	if strings.Contains(file, stepSentinelStart) || strings.Contains(file, stepSentinelDone) {
		t.Errorf("log file must not contain step sentinels: %q", file)
	}
	if strings.Contains(file, stepFieldKey) {
		t.Errorf("log file must not contain internal field key %q: %q", stepFieldKey, file)
	}
	if !strings.Contains(file, "Forwarding ports") || !strings.Contains(file, "Forwarded ports (TCP/UDP)") {
		t.Errorf("log file should contain the step messages: %q", file)
	}
}

// TestStepTitle_StreamCarriesHeadingSentinel verifies the heading helper rides
// the stream as a StepHeading sentinel while the log file stays clean.
func TestStepTitle_StreamCarriesHeadingSentinel(t *testing.T) {
	var fileBuf, streamBuf bytes.Buffer
	logger := GetLoggerForServer(int32(log.DebugLevel), &fileBuf)
	logger.AddHook(&StreamHook{Writer: &streamBuf, Level: log.InfoLevel})
	ctx := WithLogger(context.Background(), logger)

	StepTitle(ctx, "Connecting to the cluster ...")

	lines := nonEmptyLines(streamBuf.String())
	if len(lines) != 1 {
		t.Fatalf("expected 1 stream line, got %d: %q", len(lines), lines)
	}
	if k, text := DecodeStep(lines[0]); k != StepHeading || text != "Connecting to the cluster ..." {
		t.Fatalf("kind=%v text=%q, want StepHeading/Connecting to the cluster ...", k, text)
	}

	file := fileBuf.String()
	if strings.Contains(file, stepSentinelTitle) {
		t.Errorf("log file must not contain the heading sentinel: %q", file)
	}
	if strings.Contains(file, stepFieldKey) {
		t.Errorf("log file must not contain internal field key %q: %q", stepFieldKey, file)
	}
	if !strings.Contains(file, "Connecting to the cluster ...") {
		t.Errorf("log file should contain the heading text: %q", file)
	}
}

func TestEncodeDecodeStep_RoundTrip(t *testing.T) {
	for _, kind := range []StepKind{StepNone, StepBegin, StepEnd, StepHeading} {
		got, text := DecodeStep(EncodeStep(kind, "hello"))
		if text != "hello" {
			t.Errorf("kind %v: text=%q, want hello", kind, text)
		}
		if got != kind {
			t.Errorf("kind %v: decoded %v", kind, got)
		}
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
