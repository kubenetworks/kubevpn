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
