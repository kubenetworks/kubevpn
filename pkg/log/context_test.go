package log

import (
	"context"
	"testing"
)

func TestLog(t *testing.T) {
	ctx := context.Background()
	G(ctx).WithField("tun", "abc").Debug("debug")
	logger := G(ctx).WithField("tun", "abc").Logger
	logger.Debug("debug")
	logger.Warn("warn")
}

func TestWithFields(t *testing.T) {
	ctx := WithField(context.Background(), "request_id", "12345")
	ctx = WithField(ctx, "user_id", "user-abc")

	logger := GetLogger(ctx)
	logger.Info("this log will contains request_id and user_id")

	ctx2 := WithFields(ctx, map[string]any{
		"action": "login",
		"ip":     "192.168.1.1",
	})

	logger2 := GetLogger(ctx2)
	logger2.Info("this log will contains four fields")

	// 在不同方法中使用
	processRequest(ctx2)
}

func processRequest(ctx context.Context) {
	logger := GetLogger(ctx)
	logger.Debug("request handling...")

	logger.WithField("step", "validation").Info("please input validation")
}

func TestWithFieldsMerge(t *testing.T) {
	ctx := WithFields(context.Background(), map[string]any{
		"service": "api",
		"version": "v1",
	})

	// merge fields
	ctx = WithFields(ctx, map[string]any{
		"endpoint": "/users",
		"method":   "GET",
	})

	ctx = WithField(ctx, "version", "v2")

	logger := GetLogger(ctx)
	logger.Info("should show all fields，version changed to v2")
}
