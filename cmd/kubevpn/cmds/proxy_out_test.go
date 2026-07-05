package cmds

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidateProxyOutListeners(t *testing.T) {
	if err := validateProxyOutListeners("", ""); err == nil {
		t.Fatal("expected validation error when no listeners are configured")
	}
	if err := validateProxyOutListeners("127.0.0.1:1080", ""); err != nil {
		t.Fatalf("unexpected error for socks listener: %v", err)
	}
	if err := validateProxyOutListeners("", "127.0.0.1:3128"); err != nil {
		t.Fatalf("unexpected error for http connect listener: %v", err)
	}
}

func TestPrintManagedSocks(t *testing.T) {
	// Non-TTY writer: progress.Success degrades to plain text via styleLine, so the
	// managed-SOCKS success lines must contain the human text with no ANSI escape
	// bytes leaking (same contract as TestRenderer_NonTTY — pipes/CI/log scrapers
	// keep matching the raw text).
	var out bytes.Buffer
	printManagedSocksStarted(&out, "abc123def456", "127.0.0.1:1080")
	got := out.String()
	for _, want := range []string{
		"Started managed local SOCKS5 proxy",
		"abc123def456",
		"127.0.0.1:1080",
		// hints still follow the success line
		"ALL_PROXY=socks5h://127.0.0.1:1080",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("started output: expected %q, got %q", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("started output leaked ANSI escape on non-TTY: %q", got)
	}

	out.Reset()
	printManagedSocksStopped(&out, "abc123def456")
	got = out.String()
	for _, want := range []string{"Stopped managed local SOCKS5 proxy", "abc123def456"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stopped output: expected %q, got %q", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("stopped output leaked ANSI escape on non-TTY: %q", got)
	}

	// empty connection ID is a no-op for the stopped line
	out.Reset()
	printManagedSocksStopped(&out, "")
	if out.Len() != 0 {
		t.Fatalf("expected no output for empty connection ID, got %q", out.String())
	}
}

func TestPrintProxyOutHints(t *testing.T) {
	var out bytes.Buffer
	printProxyOutHints(&out, "127.0.0.1:1080", "127.0.0.1:3128")

	got := out.String()
	for _, want := range []string{
		"ALL_PROXY=socks5h://127.0.0.1:1080",
		"HTTP_PROXY=http://127.0.0.1:3128",
		"HTTPS_PROXY=http://127.0.0.1:3128",
		"TCP traffic only",
		"socks5h",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q, got %q", want, got)
		}
	}
}
