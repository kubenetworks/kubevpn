package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

func TestRotateLogStopsWithDaemonContext(t *testing.T) {
	logger := &lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "daemon.log")}
	defer logger.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rotateLog(ctx, logger)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rotation goroutine did not exit after daemon cancellation")
	}
}
