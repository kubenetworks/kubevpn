//go:build linux

package elevate

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDetachedLaunchTTYAuthentication(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	h := newProcessHarness(t, true, true, "tty")
	// The controlling terminal is fd 3; all standard streams remain pipes.
	h.cmd.ExtraFiles = []*os.File{slave}
	h.cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
	h.start(t)
	prompt := make(chan error, 1)
	go func() {
		var b [64]byte
		var text strings.Builder
		for {
			n, err := master.Read(b[:])
			text.Write(b[:n])
			if strings.Contains(text.String(), "test password: ") {
				prompt <- nil
				return
			}
			if err != nil {
				prompt <- err
				return
			}
		}
	}()
	select {
	case err := <-prompt:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sudo did not prompt on controlling terminal")
	}
	select {
	case err := <-h.done:
		t.Fatalf("launcher returned before authentication: %v", err)
	default:
	}
	if _, err := io.WriteString(master, "test-password\n"); err != nil {
		t.Fatal(err)
	}
	h.wait(t, false)
	h.assertDetached(t, true)
}
