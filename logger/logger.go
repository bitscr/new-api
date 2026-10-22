package logger

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

const (
	loggerINFO  = "INFO"
	loggerWarn  = "WARN"
	loggerError = "ERR"
	loggerDebug = "DEBUG"
)

const dailyLogDateLayout = "20060102"

// dailyLogIdentityCheckInterval bounds how often the writer verifies that its
// open file is still the file at the canonical daily path. NFS turns a deleted
// but still-open file into a hidden ".nfs*" file; without this check the writer
// would keep appending to an invisible inode forever and the log page would show
// an empty directory.
const dailyLogIdentityCheckInterval = 10 * time.Second

var setupLogLock sync.Mutex

// dailyFileWriter appends every process's logs for one calendar day to the same
// file. Rotation happens lazily on the first write after midnight, so Gin access
// logs, common.SysLog and logger.Log* all follow the same daily file.
type dailyFileWriter struct {
	dir       string
	mu        sync.Mutex
	file      *os.File
	date      string
	path      string
	lastCheck time.Time
}

func (w *dailyFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureFileLocked(time.Now()); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

func (w *dailyFileWriter) openForDate(now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ensureFileLocked(now)
}

// ensureFileLocked 的调用方必须持有 w.mu。
func (w *dailyFileWriter) ensureFileLocked(now time.Time) error {
	date := now.Format(dailyLogDateLayout)
	if w.file != nil && w.date == date {
		if !w.lastCheck.IsZero() && now.Sub(w.lastCheck) < dailyLogIdentityCheckInterval {
			return nil
		}
		w.lastCheck = now
		current, currentErr := w.file.Stat()
		onDisk, diskErr := os.Stat(w.path)
		if currentErr == nil && diskErr == nil && os.SameFile(current, onDisk) {
			return nil
		}
		// A permission or transient stat error must not stop logging; only a
		// missing or replaced path means this descriptor no longer owns the file.
		if diskErr != nil && !os.IsNotExist(diskErr) && currentErr == nil {
			return nil
		}
		log.Printf("daily log file %s was removed or replaced; recreating", w.path)
		_ = w.file.Close()
		w.file = nil
		w.path = ""
	}
	path := filepath.Join(w.dir, "oneapi-"+date+".log")
	fd, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = fd
	w.date = date
	w.path = path
	w.lastCheck = now
	return nil
}

func (w *dailyFileWriter) CurrentPath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

func (w *dailyFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	w.path = ""
	w.date = ""
	return err
}

var (
	currentLogWriter   *dailyFileWriter
	currentLogWriterMu sync.RWMutex
)

// GetCurrentLogPath 返回当天正在写入的日志文件路径。
func GetCurrentLogPath() string {
	currentLogWriterMu.RLock()
	writer := currentLogWriter
	currentLogWriterMu.RUnlock()
	if writer == nil {
		return ""
	}
	return writer.CurrentPath()
}

// SetupLogger 打开当天的日志文件。文件按天命名(oneapi-YYYYMMDD.log)，
// 进程重启不会新建文件；跨天后的第一次写入由 dailyFileWriter 自动切换。
func SetupLogger() {
	if *common.LogDir == "" {
		return
	}
	if !setupLogLock.TryLock() {
		log.Println("setup log is already working")
		return
	}
	defer setupLogLock.Unlock()

	writer := &dailyFileWriter{dir: *common.LogDir}
	if err := writer.openForDate(time.Now()); err != nil {
		log.Fatal("failed to open log file")
	}

	currentLogWriterMu.Lock()
	oldWriter := currentLogWriter
	currentLogWriter = writer
	currentLogWriterMu.Unlock()

	common.LogWriterMu.Lock()
	gin.DefaultWriter = io.MultiWriter(os.Stdout, writer)
	gin.DefaultErrorWriter = io.MultiWriter(os.Stderr, writer)
	common.LogWriterMu.Unlock()

	if oldWriter != nil {
		_ = oldWriter.Close()
	}
}

func LogInfo(ctx context.Context, msg string) {
	logHelper(ctx, loggerINFO, msg)
}

func LogWarn(ctx context.Context, msg string) {
	logHelper(ctx, loggerWarn, msg)
}

func LogError(ctx context.Context, msg string) {
	logHelper(ctx, loggerError, msg)
}

func LogDebug(ctx context.Context, msg string, args ...any) {
	if common.DebugEnabled {
		if len(args) > 0 {
			msg = fmt.Sprintf(msg, args...)
		}
		logHelper(ctx, loggerDebug, msg)
	}
}

func logHelper(ctx context.Context, level string, msg string) {
	id := ctx.Value(common.RequestIdKey)
	if id == nil {
		id = "SYSTEM"
	}
	now := time.Now()
	common.LogWriterMu.RLock()
	writer := gin.DefaultErrorWriter
	if level == loggerINFO {
		writer = gin.DefaultWriter
	}
	_, _ = fmt.Fprintf(writer, "[%s] %v | %s | %s \n", level, now.Format("2006/01/02 - 15:04:05"), id, msg)
	common.LogWriterMu.RUnlock()
}

func LogQuota(quota int) string {
	// 新逻辑：根据额度展示类型输出
	q := float64(quota)
	switch operation_setting.GetQuotaDisplayType() {
	case operation_setting.QuotaDisplayTypeCNY:
		usd := q / common.QuotaPerUnit
		cny := usd * operation_setting.USDExchangeRate
		return fmt.Sprintf("¥%.6f 额度", cny)
	case operation_setting.QuotaDisplayTypeCustom:
		usd := q / common.QuotaPerUnit
		rate := operation_setting.GetGeneralSetting().CustomCurrencyExchangeRate
		symbol := operation_setting.GetGeneralSetting().CustomCurrencySymbol
		if symbol == "" {
			symbol = "¤"
		}
		if rate <= 0 {
			rate = 1
		}
		v := usd * rate
		return fmt.Sprintf("%s%.6f 额度", symbol, v)
	case operation_setting.QuotaDisplayTypeTokens:
		return fmt.Sprintf("%d 点额度", quota)
	default: // USD
		return fmt.Sprintf("＄%.6f 额度", q/common.QuotaPerUnit)
	}
}

func FormatQuota(quota int) string {
	q := float64(quota)
	switch operation_setting.GetQuotaDisplayType() {
	case operation_setting.QuotaDisplayTypeCNY:
		usd := q / common.QuotaPerUnit
		cny := usd * operation_setting.USDExchangeRate
		return fmt.Sprintf("¥%.6f", cny)
	case operation_setting.QuotaDisplayTypeCustom:
		usd := q / common.QuotaPerUnit
		rate := operation_setting.GetGeneralSetting().CustomCurrencyExchangeRate
		symbol := operation_setting.GetGeneralSetting().CustomCurrencySymbol
		if symbol == "" {
			symbol = "¤"
		}
		if rate <= 0 {
			rate = 1
		}
		v := usd * rate
		return fmt.Sprintf("%s%.6f", symbol, v)
	case operation_setting.QuotaDisplayTypeTokens:
		return fmt.Sprintf("%d", quota)
	default: // USD
		return fmt.Sprintf("＄%.6f", q/common.QuotaPerUnit)
	}
}

// LogJson 仅供测试使用 only for test
func LogJson(ctx context.Context, msg string, obj any) {
	jsonStr, err := common.Marshal(obj)
	if err != nil {
		LogError(ctx, fmt.Sprintf("json marshal failed: %s", err.Error()))
		return
	}
	LogDebug(ctx, fmt.Sprintf("%s | %s", msg, string(jsonStr)))
}
