//go:build !windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type unixProcessLock struct{ file *os.File }

func (l *unixProcessLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	return errors.Join(unlockErr, closeErr)
}

func acquireProcessLock(ctx context.Context, path string, wait bool) (io.Closer, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, config.FileModePrivateFile)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &unixProcessLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire daemon lock: %w", err)
		}
		if !wait {
			_ = file.Close()
			return nil, errDaemonAlreadyRunning
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
