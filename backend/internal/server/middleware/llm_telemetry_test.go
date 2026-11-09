//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthTelemetryEmail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{config.RunModeSimple, config.RunModeStandard} {
		t.Run(mode, func(t *testing.T) {
			user := &service.User{ID: 7, Email: "member@example.com", Username: "member", Status: service.StatusActive, Balance: 10}
			key := &service.APIKey{ID: 100, Name: "employee@example.com", UserID: user.ID, User: user, Key: "test-key", Status: service.StatusActive}
			repo := &stubApiKeyRepo{getByKey: func(_ context.Context, value string) (*service.APIKey, error) {
				if value != key.Key {
					return nil, service.ErrAPIKeyNotFound
				}
				return key, nil
			}}
			cfg := &config.Config{RunMode: mode}
			svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			router := gin.New()
			router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg)))
			called := false
			router.POST("/v1/responses", func(c *gin.Context) {
				called = true
				require.Equal(t, user.ID, c.Request.Context().Value(ctxkey.UserID))
				require.Equal(t, user.Email, c.Request.Context().Value(ctxkey.UserEmail))
				require.Equal(t, key.ID, c.Request.Context().Value(ctxkey.TelemetryAPIKeyID))
				require.Equal(t, key.Name, c.Request.Context().Value(ctxkey.TelemetryAPIKeyName))
				require.Equal(t, user.Username, c.Request.Context().Value(ctxkey.TelemetryUserName))
				c.Status(http.StatusOK)
			})
			for _, credential := range []string{key.Key, "invalid-key"} {
				called = false
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"user":"spoof@example.com","metadata":{"user_id":"spoof@example.com"}}`))
				req.Header.Set("x-api-key", credential)
				req.Header.Set("X-User-Email", "spoof@example.com")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if credential == key.Key {
					require.Equal(t, http.StatusOK, w.Code)
					require.True(t, called)
				} else {
					require.Equal(t, http.StatusUnauthorized, w.Code)
					require.False(t, called)
				}
			}
		})
	}
}
