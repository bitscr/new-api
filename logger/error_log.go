package logger

import (
	"context"

	"github.com/QuantumNous/new-api/common"
)

// LogModelError 将模型请求报错写入与网关日志相同的按天文件，
// 便于“服务器日志管理”统一查看和清理。
func LogModelError(ctx context.Context, msg string) {
	if *common.LogDir == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	LogError(ctx, msg)
}
