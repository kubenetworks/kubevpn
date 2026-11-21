package agent

import (
	"fmt"
	"os"
	"syscall"
)

// ValidateExecArgs guards the exec payload.
func ValidateExecArgs(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("exec payload required after --")
	}
	return nil
}

// Exec replaces the current process with argv after entering the netns. Used
// by the kubevpn agent command; never returns on success.
func Exec(argv []string) error {
	if err := ValidateExecArgs(argv); err != nil {
		return err
	}
	return syscall.Exec(argv[0], argv, os.Environ())
}
