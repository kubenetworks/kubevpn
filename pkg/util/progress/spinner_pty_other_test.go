//go:build !linux

package progress

import (
	"os"
	"testing"
)

// openPTY has no dependency-free implementation off Linux, so the smart-TTY
// portion of TestSmartTTY skips there (ok=false). The plain-path guarantees that
// actually fix the reported bug are platform-independent and covered by the
// buffer/pipe cases.
func openPTY(t *testing.T) (*os.File, *os.File, bool) {
	t.Helper()
	return nil, nil, false
}
