package logger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/bytedance/gopkg/util/gopool"
)

const (
	modelErrorFilePrefix = "model-error-"
	modelErrorFileSuffix = ".log"
	modelErrorDateLayout = "20060102"
	// modelErrorRetentionDays 模型请求报错日志最多保留的天数(含当天)。
	modelErrorRetentionDays = 7
)

var (
	modelErrorFileMu    sync.Mutex
	modelErrorFile      *os.File
	modelErrorDate      string
	modelErrorPath      string
	modelErrorSetupOnce sync.Once
)

// GetCurrentModelErrorLogPath 返回当前正在写入的模型请求报错日志文件路径。
func GetCurrentModelErrorLogPath() string {
	modelErrorFileMu.Lock()
	defer modelErrorFileMu.Unlock()
	return modelErrorPath
}

// SetupModelErrorLogger 在启动时清理过期文件,并启动每日清理任务。
// 多次调用只生效一次。
func SetupModelErrorLogger() {
	modelErrorSetupOnce.Do(func() {
		if *common.LogDir == "" {
			return
		}
		cleanupModelErrorLogs()
		gopool.Go(func() {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				cleanupModelErrorLogs()
			}
		})
	})
}

// LogModelError 将模型请求报错写入按天分割的本地日志文件,
// 形如 {LogDir}/model-error-YYYYMMDD.log,仅保留最近 7 天。
// 写日志失败不会影响主流程。
func LogModelError(ctx context.Context, msg string) {
	if *common.LogDir == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id := ctx.Value(common.RequestIdKey)
	if id == nil {
		id = "SYSTEM"
	}
	now := time.Now()
	date := now.Format(modelErrorDateLayout)
	path := filepath.Join(*common.LogDir, modelErrorFilePrefix+date+modelErrorFileSuffix)

	modelErrorFileMu.Lock()
	defer modelErrorFileMu.Unlock()

	if modelErrorFile == nil || modelErrorDate != date {
		fd, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[ERR] failed to open model error log file %s: %v\n", path, err)
			return
		}
		if modelErrorFile != nil {
			_ = modelErrorFile.Close()
		}
		modelErrorFile = fd
		modelErrorDate = date
		modelErrorPath = path
		// 跨天切换文件时异步清理过期日志,避免阻塞写入。
		gopool.Go(func() {
			cleanupModelErrorLogs()
		})
	}
	_, _ = fmt.Fprintf(modelErrorFile, "[ERR] %v | %s | %s \n", now.Format("2006/01/02 - 15:04:05"), id, msg)
}

// cleanupModelErrorLogs 删除超过保留期限的模型请求报错日志文件,跳过当前活动文件。
func cleanupModelErrorLogs() {
	if *common.LogDir == "" {
		return
	}
	entries, err := os.ReadDir(*common.LogDir)
	if err != nil {
		return
	}
	activePath := GetCurrentModelErrorLogPath()
	cutoffDate := time.Now().AddDate(0, 0, -(modelErrorRetentionDays - 1))
	cutoff := time.Date(cutoffDate.Year(), cutoffDate.Month(), cutoffDate.Day(), 0, 0, 0, 0, time.Local)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, modelErrorFilePrefix) || !strings.HasSuffix(name, modelErrorFileSuffix) {
			continue
		}
		fullPath := filepath.Join(*common.LogDir, name)
		if fullPath == activePath {
			continue
		}
		dateStr := strings.TrimSuffix(strings.TrimPrefix(name, modelErrorFilePrefix), modelErrorFileSuffix)
		logDate, parseErr := time.ParseInLocation(modelErrorDateLayout, dateStr, time.Local)
		if parseErr != nil {
			info, infoErr := entry.Info()
			if infoErr != nil {
				continue
			}
			logDate = info.ModTime()
		}
		if logDate.Before(cutoff) {
			_ = os.Remove(fullPath)
		}
	}
}