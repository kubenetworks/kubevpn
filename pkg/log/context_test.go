package log

import (
	"context"
	"testing"
	"time"
)

func TestGetLoggerFromContext(t *testing.T) {
	logger := InitLoggerForServer()
	ctx := WithLogger(context.Background(), logger)
	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	timeout, c := context.WithTimeout(cancel, time.Second*10)
	defer c()
	l := GetLogger(timeout)
	if logger != l {
		panic("not same")
	}
	cancel = WithoutLogger(cancel)
	defaultLogger := GetLogger(cancel)
	if defaultLogger != L {
		panic("not same")
	}
	logger.Debug("debug")
}
