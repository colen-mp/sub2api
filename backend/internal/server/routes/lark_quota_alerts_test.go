//go:build unit

package routes

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type larkRouteSettings struct{ service.SettingRepository }

func (*larkRouteSettings) GetValue(context.Context, string) (string, error) {
	return "", service.ErrSettingNotFound
}

func TestLarkQuotaRegisteredRoutesRequireAdminAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, access := range []string{"anonymous", "restricted", "full"} {
		t.Run(access, func(t *testing.T) {
			keys := &scopedRouteKeys{}
			settings := service.NewSettingService(nil, nil)
			settings.SetScopedAdminKeyRepository(keys)
			key := ""
			if access != "anonymous" {
				var err error
				full := access == "full"
				key, _, err = settings.CreateScopedAdminKey(context.Background(), service.ScopedAdminKeyInput{Name: "Lark route test", Enabled: true, ConfirmFullAccess: full, Policy: adminaccess.Policy{FullAccess: full}}, 9)
				require.NoError(t, err)
			}
			alerts := service.NewLarkQuotaAlertService(&larkRouteSettings{}, nil, nil, nil, nil, nil)
			t.Cleanup(alerts.Stop)
			settingHandler := adminhandler.NewSettingHandler(settings, nil, nil, nil, nil, nil, nil)
			settingHandler.SetLarkQuotaAlertService(alerts)
			auth := middleware.NewAdminAuthMiddleware(nil, service.NewUserService(&scopedRouteUsers{}, nil, nil, nil), settings, nil)
			r := gin.New()
			RegisterAdminRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{Setting: settingHandler}}, auth, middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }), middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }), nil, nil)
			for _, tc := range []struct {
				method, suffix string
				fullStatus     int
			}{
				{"GET", "", 200},
				{"PUT", "", 400},       // Invalid thresholds reach validation only for full admins.
				{"POST", "/test", 400}, // No saved robot; never performs an HTTP request.
			} {
				req := httptest.NewRequest(tc.method, "/api/v1/admin/settings/lark-quota-alerts"+tc.suffix, strings.NewReader(`{"thresholds":[0],"windows":["5h"]}`))
				req.Header.Set("Content-Type", "application/json")
				if key != "" {
					req.Header.Set("x-api-key", key)
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				want := map[string]int{"anonymous": 401, "restricted": 403, "full": tc.fullStatus}[access]
				require.Equal(t, want, w.Code, tc.method+": "+w.Body.String())
				if w.Code == 200 {
					require.Contains(t, w.Body.String(), `"enabled":false`)
					require.Contains(t, w.Body.String(), `"webhook_configured":false`)
				}
			}
		})
	}
}
