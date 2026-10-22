package logger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

func setupDailyLogTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldDir := *common.LogDir
	*common.LogDir = dir

	common.LogWriterMu.RLock()
	oldWriter := gin.DefaultWriter
	oldErrorWriter := gin.DefaultErrorWriter
	common.LogWriterMu.RUnlock()

	SetupLogger()

	t.Cleanup(func() {
		*common.LogDir = oldDir
		currentLogWriterMu.Lock()
		writer := currentLogWriter
		currentLogWriter = nil
		currentLogWriterMu.Unlock()
		if writer != nil {
			_ = writer.Close()
		}
		common.LogWriterMu.Lock()
		gin.DefaultWriter = oldWriter
		gin.DefaultErrorWriter = oldErrorWriter
		common.LogWriterMu.Unlock()
	})
	return dir
}

func TestDailyFileWriterRotatesByDate(t *testing.T) {
	dir := t.TempDir()
	writer := &dailyFileWriter{dir: dir}
	defer writer.Close()

	dayOne := time.Date(2026, 10, 20, 23, 59, 59, 0, time.Local)
	if err := writer.openForDate(dayOne); err != nil {
		t.Fatalf("open day one: %v", err)
	}
	if got, want := writer.CurrentPath(), filepath.Join(dir, "oneapi-20261020.log"); got != want {
		t.Fatalf("day one path = %q, want %q", got, want)
	}

	dayTwo := dayOne.Add(2 * time.Second)
	if err := writer.openForDate(dayTwo); err != nil {
		t.Fatalf("open day two: %v", err)
	}
	if got, want := writer.CurrentPath(), filepath.Join(dir, "oneapi-20261021.log"); got != want {
		t.Fatalf("day two path = %q, want %q", got, want)
	}
}

// NFS turns a deleted-but-open daily log into a hidden .nfs* file. The writer
// must notice that its canonical path no longer points at its open descriptor
// and recreate oneapi-YYYYMMDD.log instead of appending to an invisible inode.
func TestDailyFileWriterRecreatesDeletedDailyFile(t *testing.T) {
	dir := t.TempDir()
	writer := &dailyFileWriter{dir: dir}
	defer writer.Close()

	now := time.Now()
	if err := writer.openForDate(now); err != nil {
		t.Fatalf("open daily log: %v", err)
	}
	path := writer.CurrentPath()
	if _, err := writer.Write([]byte("before delete\n")); err != nil {
		t.Fatalf("write before delete: %v", err)
	}
	// Windows cannot unlink an open file. Close the descriptor first so the test
	// can remove the path; the writer still holds it and must reopen the canonical
	// daily file on the next write. On Linux/NFS the same state is produced by an
	// unlink while the descriptor stays open.
	writer.mu.Lock()
	_ = writer.file.Close()
	writer.lastCheck = time.Time{}
	writer.mu.Unlock()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove daily log: %v", err)
	}

	if _, err := writer.Write([]byte("after delete\n")); err != nil {
		t.Fatalf("write after delete: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("daily log was not recreated: %v", err)
	}
	if !strings.Contains(string(data), "after delete") {
		t.Fatalf("recreated daily log missing new content: %q", string(data))
	}
}

func TestModelErrorAndGatewayLogShareDailyFile(t *testing.T) {
	dir := setupDailyLogTest(t)
	ctx := context.WithValue(context.Background(), common.RequestIdKey, "req-123")

	LogModelError(ctx, "model request failed")
	LogError(ctx, "gateway error")

	path := filepath.Join(dir, "oneapi-"+time.Now().Format(dailyLogDateLayout)+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected merged daily log file: %v", err)
	}
	content := string(data)
	for _, want := range []string{"model request failed", "gateway error", "req-123"} {
		if !strings.Contains(content, want) {
			t.Errorf("merged log missing %q: %s", want, content)
		}
	}
}

func TestLogModelErrorNilContextUsesSystemId(t *testing.T) {
	dir := setupDailyLogTest(t)

	LogModelError(nil, "boom")

	path := filepath.Join(dir, "oneapi-"+time.Now().Format(dailyLogDateLayout)+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected merged daily log file: %v", err)
	}
	if !strings.Contains(string(data), "SYSTEM") {
		t.Errorf("expected SYSTEM fallback request id, got: %q", string(data))
	}
}
