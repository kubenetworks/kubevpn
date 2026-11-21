package agent

import (
	"strings"
	"testing"
)

func TestExecArgsValidated(t *testing.T) {
	if err := ValidateExecArgs(nil); err == nil || !strings.Contains(err.Error(), "exec") {
		t.Fatalf("empty argv must error, got %v", err)
	}
	if err := ValidateExecArgs([]string{"ok"}); err != nil {
		t.Fatalf("non-empty argv must pass, got %v", err)
	}
}
