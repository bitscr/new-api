package controller

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestExportLogFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	oldDir := *common.LogDir
	*common.LogDir = dir
	t.Cleanup(func() { *common.LogDir = oldDir })

	name := "oneapi-20261021.log"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("hello log"), 0644))

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/performance/logs/export?name="+name, nil)
	ExportLogFile(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "hello log", recorder.Body.String())
	require.Contains(t, recorder.Header().Get("Content-Disposition"), name)

	badRecorder := httptest.NewRecorder()
	badContext, _ := gin.CreateTestContext(badRecorder)
	badContext.Request = httptest.NewRequest(http.MethodGet, "/api/performance/logs/export?name=../secret.log", nil)
	ExportLogFile(badContext)
	require.Contains(t, badRecorder.Body.String(), `"success":false`)
}

func TestExportDatabaseSQLiteSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldDB := model.DB
	oldSQLite, oldMySQL, oldPostgreSQL := common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL
	dbPath := filepath.Join(t.TempDir(), "source.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{PrepareStmt: true})
	require.NoError(t, err)
	require.NoError(t, db.Exec("CREATE TABLE export_probe (id INTEGER PRIMARY KEY, name TEXT)").Error)
	require.NoError(t, db.Exec("INSERT INTO export_probe (name) VALUES ('snapshot')").Error)
	model.DB = db
	common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = true, false, false
	t.Cleanup(func() {
		model.DB = oldDB
		common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = oldSQLite, oldMySQL, oldPostgreSQL
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/performance/database/export", nil)
	ExportDatabase(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.True(t, strings.HasPrefix(recorder.Body.String(), "SQLite format 3"), "must download a real SQLite snapshot")
	require.Contains(t, recorder.Header().Get("Content-Disposition"), "one-api.db")
}
