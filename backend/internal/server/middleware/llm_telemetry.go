package middleware

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/llmtelemetry"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func LLMTelemetry() gin.HandlerFunc {
	return llmtelemetry.RequestMiddleware(func(ctx context.Context) llmtelemetry.Identity {
		id := llmtelemetry.Identity{}
		id.APIKeyID, _ = ctx.Value(ctxkey.TelemetryAPIKeyID).(int64)
		id.APIKeyName, _ = ctx.Value(ctxkey.TelemetryAPIKeyName).(string)
		id.UserID, _ = ctx.Value(ctxkey.UserID).(int64)
		id.UserEmail, _ = ctx.Value(ctxkey.UserEmail).(string)
		id.UserName, _ = ctx.Value(ctxkey.TelemetryUserName).(string)
		id.Platform, _ = ctx.Value(ctxkey.Platform).(string)
		if group, ok := ctx.Value(ctxkey.Group).(*service.Group); ok && group != nil {
			id.GroupID = group.ID
			id.GroupName = group.Name
			if id.Platform == "" {
				id.Platform = group.Platform
			}
		}
		return id
	}, func(raw string) string {
		if parsed := service.ParseMetadataUserID(raw); parsed != nil {
			return parsed.SessionID
		}
		return ""
	})
}
