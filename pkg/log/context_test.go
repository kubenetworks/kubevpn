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
	// 创建一个带有字段的 context
	ctx := WithField(context.Background(), "request_id", "12345")
	ctx = WithField(ctx, "user_id", "user-abc")

	// 从 context 获取带有字段的 logger
	logger := GetLogger(ctx)
	logger.Info("这条日志会自动包含 request_id 和 user_id")

	// 可以继续添加更多字段
	ctx2 := WithFields(ctx, map[string]any{
		"action": "login",
		"ip":     "192.168.1.1",
	})

	logger2 := GetLogger(ctx2)
	logger2.Info("这条日志会包含所有四个字段")

	// 在不同方法中使用
	processRequest(ctx2)
}

func processRequest(ctx context.Context) {
	// 在其他方法中，可以直接从 ctx 获取带字段的 logger
	logger := GetLogger(ctx)
	logger.Debug("处理请求中...")

	// 也可以临时添加额外的字段
	logger.WithField("step", "validation").Info("验证输入参数")
}

func TestWithFieldsMerge(t *testing.T) {
	// 测试字段合并功能
	ctx := WithFields(context.Background(), map[string]any{
		"service": "api",
		"version": "v1",
	})

	// 添加更多字段，新字段会和旧字段合并
	ctx = WithFields(ctx, map[string]any{
		"endpoint": "/users",
		"method":   "GET",
	})

	// 覆盖已存在的字段
	ctx = WithField(ctx, "version", "v2")

	logger := GetLogger(ctx)
	logger.Info("所有字段都会出现在日志中，version 被更新为 v2")
}
