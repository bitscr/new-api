package logger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
)

func setupErrorLogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := *common.LogDir
	*common.LogDir = dir
	t.Cleanup(func() {
		*common.LogDir = old
		modelErrorFileMu.Lock()
		if modelErrorFile != nil {
			_ = modelErrorFile.Close()
			modelErrorFile = nil
		}
		modelErrorDate = ""
		modelErrorPath = ""
		modelErrorFileMu.Unlock()
	})
	return dir
}

func TestLogModelErrorWritesDailyFile(t *testing.T) {
	dir := setupErrorLogDir(t)
	ctx := context.WithValue(context.Background(), common.RequestIdKey, "req-123")

	LogModelError(ctx, "model request failed")

	path := filepath.Join(dir, modelErrorFilePrefix+time.Now().Format(modelErrorDateLayout)+modelErrorFileSuffix)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected daily error log file, got error: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "model request failed") {
		t.Errorf("log content missing message: %q", content)
	}
	if !strings.Contains(content, "req-123") {
		t.Errorf("log content missing request id: %q", content)
	}
}

func TestLogModelErrorNilContextUsesSystemId(t *testing.T) {
	dir := setupErrorLogDir(t)

	LogModelError(nil, "boom")

	path := filepath.Join(dir, modelErrorFilePrefix+time.Now().Format(modelErrorDateLayout)+modelErrorFileSuffix)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected daily error log file, got error: %v", err)
	}
	if !strings.Contains(string(data), "SYSTEM") {
		t.Errorf("expected SYSTEM fallback request id, got: %q", string(data))
	}
}

func TestCleanupModelErrorLogsRetention(t *testing.T) {
	dir := setupErrorLogDir(t)

	today := time.Now().Format(modelErrorDateLayout)
	withinRetention := time.Now().AddDate(0, 0, -3).Format(modelErrorDateLayout)
	expired := time.Now().AddDate(0, 0, -8).Format(modelErrorDateLayout)

	todayPath := filepath.Join(dir, modelErrorFilePrefix+today+modelErrorFileSuffix)
	withinPath := filepath.Join(dir, modelErrorFilePrefix+withinRetention+modelErrorFileSuffix)
	expiredPath := filepath.Join(dir, modelErrorFilePrefix+expired+modelErrorFileSuffix)
	unrelatedPath := filepath.Join(dir, "oneapi-20200101000000.log")

	for _, p := range []string{todayPath, withinPath, expiredPath, unrelatedPath} {
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("failed to prepare file %s: %v", p, err)
		}
	}

	cleanupModelErrorLogs()

	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Errorf("expected expired log file to be removed: %s", expiredPath)
	}
	for _, p := range []string{todayPath, withinPath, unrelatedPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file to be kept: %s (%v)", p, err)
		}
	}
}

func TestCleanupModelErrorLogsSkipsActiveFile(t *testing.T) {
	dir := setupErrorLogDir(t)

	expired := time.Now().AddDate(0, 0, -30).Format(modelErrorDateLayout)
	path := filepath.Join(dir, modelErrorFilePrefix+expired+modelErrorFileSuffix)
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatalf("failed to prepare file: %v", err)
	}

	modelErrorFileMu.Lock()
	modelErrorPath = path
	modelErrorFileMu.Unlock()

	cleanupModelErrorLogs()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected active log file to be kept: %s (%v)", path, err)
	}
}