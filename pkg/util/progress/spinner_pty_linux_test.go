//go:build linux

package progress

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY opens a Linux pty master/slave pair using only golang.org/x/sys/unix
// (already vendored), so the smart-TTY test can exercise a real terminal fd
// without pulling in a dedicated pty dependency. Returns (master, slave, true)
// or fails the test on error.
func openPTY(t *testing.T) (*os.File, *os.File, bool) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	// Unlock the slave and read its number.
	var unlock int32 // 0 = unlock
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, int(unlock)); err != nil {
		master.Close()
		t.Fatalf("TIOCSPTLCK: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		t.Fatalf("TIOCGPTN: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Fatalf("open /dev/pts/%d: %v", n, err)
	}
	return master, slave, true
}
