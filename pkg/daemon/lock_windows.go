//go:build windows

package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"runtime"

	"golang.org/x/sys/windows"
)

type windowsProcessLock struct{ handle windows.Handle }

// Close must run on the goroutine that acquired the lock. Windows mutexes are
// owned by an OS thread, so acquisition keeps that thread locked until here.
func (l *windowsProcessLock) Close() error {
	releaseErr := windows.ReleaseMutex(l.handle)
	closeErr := windows.CloseHandle(l.handle)
	runtime.UnlockOSThread()
	return errors.Join(releaseErr, closeErr)
}

func acquireProcessLock(ctx context.Context, path string, wait bool) (io.Closer, error) {
	runtime.LockOSThread()
	acquired := false
	defer func() {
		if !acquired {
			runtime.UnlockOSThread()
		}
	}()

	digest := sha256.Sum256([]byte(path))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf("Local\\kubevpn-daemon-%x", digest[:8]))
	if err != nil {
		return nil, fmt.Errorf("build daemon mutex name: %w", err)
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, fmt.Errorf("create daemon mutex: %w", err)
	}
	if handle == 0 {
		return nil, errors.New("create daemon mutex returned an empty handle")
	}
	for {
		state, waitErr := windows.WaitForSingleObject(handle, 50)
		if waitErr != nil {
			_ = windows.CloseHandle(handle)
			return nil, fmt.Errorf("wait for daemon mutex: %w", waitErr)
		}
		switch state {
		case windows.WAIT_OBJECT_0, uint32(windows.WAIT_ABANDONED):
			acquired = true
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
		if wait {
			select {
			case <-ctx.Done():
				_ = windows.CloseHandle(handle)
				return nil, ctx.Err()
			default:
			}
		}
	}
}
