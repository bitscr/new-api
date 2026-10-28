package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUniversalVerifyAcceptsLoginPassword(t *testing.T) {
	db := setupInviteRegistrationControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.TwoFA{}, &model.PasskeyCredential{}))

	hashed, err := common.Password2Hash("Password123!")
	require.NoError(t, err)
	user := model.User{
		Username: "password-verify-user",
		Password: hashed,
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
	}
	require.NoError(t, db.Create(&user).Error)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("universal-verify-secret"))))
	router.GET("/login", func(c *gin.Context) {
		loaded, loadErr := model.GetUserById(user.Id, false)
		require.NoError(t, loadErr)
		setupLogin(loaded, c)
	})
	router.POST("/api/verify", middleware.UserAuth(), UniversalVerify)

	login := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	router.ServeHTTP(login, loginReq)
	require.Equal(t, http.StatusOK, login.Code)
	cookieHeader := login.Result().Cookies()[0].String()

	verify := func(password string) map[string]any {
		body, marshalErr := common.Marshal(gin.H{"method": "password", "code": password})
		require.NoError(t, marshalErr)
		req := httptest.NewRequest(http.MethodPost, "/api/verify", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", cookieHeader)
		req.Header.Set("New-Api-User", strconv.Itoa(user.Id))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		var result map[string]any
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
		return result
	}

	require.False(t, verify("wrong-password")["success"].(bool))
	require.True(t, verify("Password123!")["success"].(bool))
}
