package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) SetLarkQuotaAlertService(alerts *service.LarkQuotaAlertService) {
	h.larkQuotaAlerts = alerts
}

func (h *SettingHandler) GetLarkQuotaSettings(c *gin.Context) {
	if h.larkQuotaAlerts == nil {
		response.Error(c, 503, "Lark quota notifications unavailable")
		return
	}
	settings, err := h.larkQuotaAlerts.GetSettings(c.Request.Context())
	if err != nil {
		response.Error(c, 500, "Failed to load Lark quota settings")
		return
	}
	response.Success(c, settings)
}

func (h *SettingHandler) UpdateLarkQuotaSettings(c *gin.Context) {
	if h.larkQuotaAlerts == nil {
		response.Error(c, 503, "Lark quota notifications unavailable")
		return
	}
	var input service.LarkQuotaSettings
	if c.ShouldBindJSON(&input) != nil {
		response.Error(c, 400, "Invalid Lark quota settings")
		return
	}
	settings, err := h.larkQuotaAlerts.UpdateSettings(c.Request.Context(), input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *SettingHandler) TestLarkQuotaNotification(c *gin.Context) {
	if h.larkQuotaAlerts == nil {
		response.Error(c, 503, "Lark quota notifications unavailable")
		return
	}
	if err := h.larkQuotaAlerts.TestNotification(c.Request.Context()); err != nil {
		// Do not expose DB/crypto/transport errors to the client.
		response.Error(c, 400, "Lark test failed; check the saved webhook, signing secret and robot security settings")
		return
	}
	response.Success(c, gin.H{"message": "Lark test notification sent"})
}
