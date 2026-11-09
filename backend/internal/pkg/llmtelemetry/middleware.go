package llmtelemetry

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Identity struct {
	UserID, GroupID, APIKeyID int64
	UserEmail                 string
	UserName, GroupName       string
	APIKeyName                string
	Platform                  string
}
type requestKey struct{}
type requestState struct {
	identity func(context.Context) Identity
	mu       sync.Mutex
	content  []attribute.KeyValue
	session  *clientSession
}

func (s *requestState) recordContent(attrs []attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content = nil
	for _, a := range attrs {
		if a.Key == "langfuse.observation.input" || a.Key == "langfuse.observation.output" || a.Key == "sub2api.input.truncated" || a.Key == "sub2api.output.truncated" {
			s.content = append(s.content, a)
		}
	}
}
func (s *requestState) contentAttributes() []attribute.KeyValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]attribute.KeyValue(nil), s.content...)
}

func llmPath(path string) bool {
	return strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/messages") || strings.HasSuffix(path, "/chat/completions")
}
func identityAttributes(id Identity, mappings ...map[string]string) []attribute.KeyValue {
	a := []attribute.KeyValue{attribute.Int64("sub2api.user_id", id.UserID), attribute.Int64("sub2api.group_id", id.GroupID), attribute.Int64("sub2api.api_key_id", id.APIKeyID)}
	if id.UserID > 0 {
		user := strconv.FormatInt(id.UserID, 10)
		source := "user_id"
		if email := strings.TrimSpace(id.UserEmail); email != "" {
			user = email
			source = "user_email"
		}
		if email := safeIdentityName(id.UserEmail); email != "" {
			a = append(a, attribute.String("sub2api.user_email", email))
		}

		a = append(a, attribute.String("user.id", user), attribute.String("langfuse.user.id", user), attribute.String("sub2api.user_identity_source", source))
		for _, field := range []struct {
			source, name string
			present      bool
		}{
			{"sub2api.api_key_name", id.APIKeyName, id.APIKeyID > 0},
			{"sub2api.group_name", id.GroupName, id.GroupID > 0},
			{"sub2api.user_name", id.UserName, true},
		} {
			if name := safeIdentityName(field.name); field.present && name != "" {
				a = append(a, attribute.String(field.source, name))
			}
		}
	}
	if id.Platform != "" {
		a = append(a, attribute.String("gen_ai.provider.name", clip(id.Platform, 64)))
	}
	if len(mappings) > 0 {
		return mapAttributes(a, mappings[0])
	}
	return a
}

// Only bounded safe names obtained from authentication are exported.
func safeIdentityName(raw string) string {
	name := strings.TrimSpace(raw)
	if len(name) == 0 || len(name) > 256 || !utf8.ValidString(name) || redactText(name) != name {
		return ""
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return ""
		}
	}
	return name
}

// RequestMiddleware belongs after request-ID generation and before gateway auth.
// Identity is resolved lazily from the authenticated request context.
func RequestMiddleware(identity func(context.Context) Identity, parseUserID ...func(string) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		r := current.Load()
		if r == nil || c.Request.Method != http.MethodPost || !llmPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		ctx, span := r.tracer.Start(c.Request.Context(), "sub2api.request", trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attribute.String("gen_ai.operation.name", "workflow"), attribute.String("langfuse.observation.type", "span"), attribute.String("http.request.method", c.Request.Method)))
		state := &requestState{identity: identity}
		state.session = observeClientSession(c.Request, parseUserID...)
		ctx = context.WithValue(ctx, requestKey{}, state)
		c.Request = c.Request.WithContext(ctx)
		defer func() {
			panicValue := recover()
			content := state.contentAttributes()
			if r.cfg.Backend == "langfuse" {
				content = currentTurnAttributes(content)
			}
			span.SetAttributes(content...)
			span.SetAttributes(identityAttributes(identity(c.Request.Context()))...)
			span.SetAttributes(billingCorrelationAttributes(c.Request.Context())...)
			span.SetAttributes(state.sessionAttributes(identity(c.Request.Context()))...)
			span.SetAttributes(attribute.Int("http.response.status_code", c.Writer.Status()), attribute.String("http.route", c.FullPath()), attribute.Bool("sub2api.request.response_written", c.Writer.Written()))
			if requestID, ok := c.Request.Context().Value(ctxkey.RequestID).(string); ok {
				span.SetAttributes(attribute.String("sub2api.request_id", clip(requestID, 256)))
			}
			contextErr := c.Request.Context().Err()
			if errors.Is(contextErr, context.Canceled) {
				// Cancellation can be observed after business completion. It alone
				// proves neither request failure nor successful client delivery.
				span.SetAttributes(attribute.Bool("sub2api.request.canceled", true), attribute.String("sub2api.request.context_error", "context_canceled"))
			} else if errors.Is(contextErr, context.DeadlineExceeded) {
				span.SetAttributes(attribute.String("sub2api.request.context_error", "deadline_exceeded"))
			}
			switch {
			case panicValue != nil:
				span.SetStatus(codes.Error, "request panicked")
			case c.Writer.Status() >= 400:
				span.SetStatus(codes.Error, "request HTTP error")
			case errors.Is(contextErr, context.DeadlineExceeded):
				span.SetStatus(codes.Error, "request deadline exceeded")
			}
			span.End()
			if panicValue != nil {
				panic(panicValue)
			}
		}()
		c.Next()
	}
}

// Mirrors the ordinary HTTP usage owner: client ID takes precedence over local ID.
// Forced money events and WebSocket billing are outside these HTTP model routes.
func billingCorrelationAttributes(ctx context.Context) []attribute.KeyValue {
	id, _ := ctx.Value(ctxkey.ClientRequestID).(string)
	prefix := "client:"
	if strings.TrimSpace(id) == "" {
		id, _ = ctx.Value(ctxkey.RequestID).(string)
		prefix = "local:"
	}
	id = strings.TrimSpace(id)
	if len(id) == 0 || len(id) > 240 || redactText(id) != id || strings.HasPrefix(id, "sk-") || strings.HasPrefix(id, "pk-") {
		return nil
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return nil
		}
	}
	return []attribute.KeyValue{attribute.String("sub2api.billing_request_id", prefix+id)}
}
