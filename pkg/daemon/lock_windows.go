//go:build windows

package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/sys/windows"
)

type windowsProcessLock struct{ handle windows.Handle }

func (l *windowsProcessLock) Close() error {
	return errors.Join(windows.ReleaseMutex(l.handle), windows.CloseHandle(l.handle))
}

func acquireProcessLock(ctx context.Context, path string, wait bool) (io.Closer, error) {
	digest := sha256.Sum256([]byte(path))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf("Local\\kubevpn-daemon-%x", digest[:8]))
	if err != nil {
		return nil, fmt.Errorf("build daemon mutex name: %w", err)
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		return nil, fmt.Errorf("create daemon mutex: %w", err)
	}
	for {
		state, waitErr := windows.WaitForSingleObject(handle, 0)
		if waitErr != nil {
			_ = windows.CloseHandle(handle)
			return nil, fmt.Errorf("wait for daemon mutex: %w", waitErr)
		}
		switch state {
		case windows.WAIT_OBJECT_0, uint32(windows.WAIT_ABANDONED):
			return &windowsProcessLock{handle: handle}, nil
		case uint32(windows.WAIT_TIMEOUT):
			if !wait {
				_ = windows.CloseHandle(handle)
				return nil, errDaemonAlreadyRunning
			}
		default:
			_ = windows.CloseHandle(handle)
			return nil, fmt.Errorf("unexpected daemon mutex state %d", state)
		}
		select {
		case <-ctx.Done():
			_ = windows.CloseHandle(handle)
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
