package controller

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestLogFilesIncludeExternalAndNFSFiles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	oldDir := *common.LogDir
	*common.LogDir = dir
	t.Cleanup(func() { *common.LogDir = oldDir })

	for _, name := range []string{
		"oneapi-20261021.log",
		"stdout.log",
		".nfs00000000380f7ffd00000019",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0644))
	}

	files, err := getLogFiles()
	require.NoError(t, err)
	require.Len(t, files, 3)
	byName := map[string]bool{}
	for _, file := range files {
		byName[file.Name] = file.Managed
	}
	require.True(t, byName["oneapi-20261021.log"])
	require.False(t, byName["stdout.log"])
	require.False(t, byName[".nfs00000000380f7ffd00000019"])

	for _, name := range []string{"stdout.log", ".nfs00000000380f7ffd00000019"} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/performance/logs/export?name="+name, nil)
		ExportLogFile(c)
		require.Equal(t, http.StatusOK, recorder.Code, name)
		require.Equal(t, name, recorder.Body.String(), name)
	}
}

func TestCleanupLogFilesProtectsNewestManagedAndExternalFiles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	oldDir := *common.LogDir
	*common.LogDir = dir
	t.Cleanup(func() { *common.LogDir = oldDir })

	old := time.Now().Add(-48 * time.Hour)
	newest := time.Now().Add(-25 * time.Hour)
	files := []struct {
		name string
		mod  time.Time
	}{
		{"oneapi-20261018.log", old},
		{"oneapi-20261019.log", old},
		{"oneapi-20261020.log", newest},
		{"stdout.log", old},
	}
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		require.NoError(t, os.WriteFile(path, []byte(file.name), 0644))
		require.NoError(t, os.Chtimes(path, file.mod, file.mod))
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/performance/logs?mode=by_days&value=1", nil)
	CleanupLogFiles(c)

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			DeletedCount   int `json:"deleted_count"`
			ProtectedCount int `json:"protected_count"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.Equal(t, 2, response.Data.DeletedCount)
	require.Equal(t, 1, response.Data.ProtectedCount, "the newest managed log must not be deleted")

	for _, name := range []string{"oneapi-20261020.log", "stdout.log"} {
		_, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, name+" must remain")
	}
	for _, name := range []string{"oneapi-20261018.log", "oneapi-20261019.log"} {
		_, err := os.Stat(filepath.Join(dir, name))
		require.True(t, os.IsNotExist(err), name+" must be removed")
	}
}
