//go:build linux

package agent

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// EnterNetns moves the calling thread into the target PID's network namespace.
// The caller must have runtime.LockOSThread held (the kubevpn agent command
// does) and typically follows this with syscall.Exec so the exec'd payload
// runs inside the pod netns.
func EnterNetns(procRoot string, pid int) error {
	runtime.LockOSThread()
	fd, err := unix.Open(fmt.Sprintf("%s/%d/ns/net", procRoot, pid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open netns of pid %d: %w", pid, err)
	}
	defer unix.Close(fd)
	if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("setns to pid %d netns: %w", pid, err)
	}
	return nil
}
