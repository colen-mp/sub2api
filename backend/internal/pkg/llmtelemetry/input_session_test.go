package llmtelemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestLangfuseInputPrecisionUnknownItemsAndLimits(t *testing.T) {
	body := []byte(`{"instructions":"system","input":[{"type":"additional_tools","tools":[{"name":"lookup"}]},{"type":"reasoning","encrypted_content":"private"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":9007199254740993,\"password\":\"private\"}"},{"type":"custom_tool_call","call_id":"custom_1","name":"exec","input":"echo hi"},{"type":"custom_tool_call_output","call_id":"custom_1","output":"hi"},{"role":"user","content":"最新问题"}]}`)
	a := inputAttributes(body, false, true, 16384, "langfuse")
	text := attr(a, "langfuse.observation.input").(string)
	require.True(t, json.Valid([]byte(text)))
	require.Contains(t, text, "9007199254740993")
	require.Contains(t, text, "additional_tools")
	require.NotContains(t, text, "private")
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &doc))
	require.Len(t, doc.Messages, 7)
	require.Equal(t, "assistant", doc.Messages[2]["role"])
	require.Equal(t, "[REDACTED REASONING]", doc.Messages[2]["content"])
	require.Equal(t, "custom_1", doc.Messages[5]["tool_call_id"])
	require.Equal(t, "最新问题", doc.Messages[6]["content"])
	for _, msg := range []string{strings.Repeat("前文", 3000), strings.Repeat("x", 3000) + "最新消息尾部"} {
		body, _ := json.Marshal(map[string]any{"instructions": strings.Repeat("system", 2000), "input": []any{map[string]any{"role": "user", "content": "old"}, map[string]any{"role": "user", "content": msg}}, "tools": []any{map[string]any{"description": strings.Repeat("tools", 1000)}}})
		attrs := inputAttributes(body, false, true, 1024, "langfuse")
		got := attr(attrs, "langfuse.observation.input").(string)
		require.LessOrEqual(t, len(got), 1024)
		require.True(t, json.Valid([]byte(got)))
		require.Contains(t, got, inputLimitMarker)
		require.True(t, attr(attrs, "sub2api.input.truncated").(bool))
		if strings.HasSuffix(msg, "最新消息尾部") {
			require.Contains(t, got, "最新消息尾部")
		}
		state := &requestState{}
		state.recordContent(attrs)
		require.Equal(t, true, attr(state.contentAttributes(), "sub2api.input.truncated"))
	}
}

func TestSessionWithoutContentCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, allowed := range []bool{false, true} {
		exp := tracetest.NewInMemoryExporter()
		cfg := testConfig()
		cfg.Backend = "langfuse"
		cfg.CaptureContent = allowed
		cfg.CaptureGroups = map[int64]bool{99: true} // Authenticated group 1 is excluded.
		r := newRuntime(cfg, exp)
		old := current.Swap(r)
		engine := gin.New()
		engine.Use(RequestMiddleware(func(context.Context) Identity { return Identity{UserID: 7, GroupID: 1} }))
		engine.POST("/v1/responses", func(c *gin.Context) {
			body, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			u := Wrap(&mockUpstream{do: func(req *http.Request) (*http.Response, error) {
				_, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"output":[]}`))}, nil
			}})
			req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", "https://simulation.invalid/v1/responses", strings.NewReader(string(body)))
			resp, err := u.Do(req, "", 101, 1)
			require.NoError(t, err)
			_, _ = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())
			c.Status(200)
		})
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"private-content","metadata":{"session_id":"body-session"}}`)))
		require.NoError(t, r.provider.ForceFlush(context.Background()))
		spans := exp.GetSpans()
		require.Len(t, spans, 2)
		for _, span := range spans {
			require.NotEmpty(t, attr(span.Attributes, "langfuse.session.id"))
			require.Nil(t, attr(span.Attributes, "langfuse.observation.input"))
			require.Nil(t, attr(span.Attributes, "langfuse.observation.output"))
		}
		require.Equal(t, attr(spans[0].Attributes, "langfuse.session.id"), attr(spans[1].Attributes, "langfuse.session.id"))
		require.NoError(t, r.provider.Shutdown(context.Background()))
		current.Store(old)
	}
}

func TestFinalTransportFailureReplacesRootContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	exp := tracetest.NewInMemoryExporter()
	cfg := testConfig()
	cfg.Backend = "langfuse"
	r := newRuntime(cfg, exp)
	old := current.Swap(r)
	defer current.Store(old)
	defer r.provider.Shutdown(context.Background())
	engine := gin.New()
	engine.Use(RequestMiddleware(func(context.Context) Identity { return Identity{UserID: 7, GroupID: 1} }))
	engine.POST("/v1/responses", func(c *gin.Context) {
		calls := 0
		u := Wrap(&mockUpstream{do: func(req *http.Request) (*http.Response, error) {
			_, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			calls++
			if calls == 2 {
				return nil, errors.New("transport failure")
			}
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"old-output"}}]}`))}, nil
		}})
		for _, text := range []string{"old-input", "final-input"} {
			req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", "https://simulation.invalid/v1/responses", strings.NewReader(`{"input":"`+text+`"}`))
			resp, err := u.Do(req, "", 101, 1)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				require.NoError(t, resp.Body.Close())
			}
		}
		c.Status(502)
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", nil))
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	var root []attribute.KeyValue
	for _, span := range exp.GetSpans() {
		if span.Name == "sub2api.request" {
			root = span.Attributes
		}
	}
	require.Contains(t, attr(root, "langfuse.observation.input"), "final-input")
	require.NotContains(t, attr(root, "langfuse.observation.input"), "old-input")
	require.Nil(t, attr(root, "langfuse.observation.output"))
}

func TestClientSessionSourcesAndBoundedBody(t *testing.T) {
	for _, tc := range []struct{ name, header, value, body, want string }{
		{"codex", "session-id", "session-a", `{"client_metadata":{"session_id":"body"}}`, "session-a"},
		{"underscore", "session_id", "session-a", `{}`, "session-a"},
		{"claude header", "x-claude-code-session-id", "session-a", `{}`, "session-a"},
		{"conversation", "conversation_id", "session-a", `{}`, "session-a"},
		{"codex metadata", "x-codex-turn-metadata", `{"session_id":"session-a"}`, `{}`, "session-a"},
		{"body metadata", "", "", `{"metadata":{"session_id":"session-a"}}`, "session-a"},
		{"body client metadata", "", "", `{"input":"` + strings.Repeat("x", 100000) + `","client_metadata":{"session_id":"session-a"}}`, "session-a"},
		{"invalid", "session-id", "bad\nvalue", `{}`, ""},
		{"no guessing", "", "", `{"prompt_cache_key":"cache","user":"someone","input":"hello"}`, ""},
		{"unfinished", "", "", `{"metadata":{"session_id":"session-a"}`, ""},
		{"oversized metadata", "", "", `{"metadata":{"large":"` + strings.Repeat("x", 5000) + `","session_id":"session-a"}}`, ""},
		{"numeric", "", "", `{"metadata":{"session_id":123}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(tc.body))
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}
			s := observeClientSession(req)
			got, e := io.ReadAll(req.Body)
			require.NoError(t, e)
			require.Equal(t, tc.body, string(got))
			require.NoError(t, req.Body.Close())
			v, _ := s.identifier()
			require.Equal(t, tc.want, v)
			state := &requestState{session: s}
			require.Nil(t, state.sessionAttributes(Identity{}))
			if tc.want != "" {
				a := attr(state.sessionAttributes(Identity{UserID: 1}), "langfuse.session.id")
				b := attr(state.sessionAttributes(Identity{UserID: 2}), "langfuse.session.id")
				require.NotEqual(t, a, b)
				require.NotContains(t, a, tc.want)
			} else {
				require.Nil(t, state.sessionAttributes(Identity{UserID: 1}))
			}
			if s.projection != nil {
				require.LessOrEqual(t, s.projection.token.Len(), 4096)
				require.LessOrEqual(t, len(s.projection.bytes()), 5000)
			}
		})
	}
}

// Uses a local fake transport: no provider calls or account/billing writes.
// Opt-in live mode exports only synthetic observations using existing config.
func TestSessionTurnsRetryAndInputReadback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	exp := tracetest.NewInMemoryExporter()
	cfg := testConfig()
	cfg.Backend = "langfuse"
	cfg.MaxBytes = 16384
	// All 15 observations must fit even when the exporter is not scheduled
	// until after the five requests finish. Queue overflow has separate tests.
	cfg.QueueSize = 32
	r := newRuntime(cfg, exp)
	old := current.Swap(r)
	defer current.Store(old)
	if os.Getenv("SUB2API_TEST_LIVE_LANGFUSE") == "1" {
		require.NoError(t, r.provider.Shutdown(context.Background()))
		stop, err := InitFromEnv(context.Background())
		require.NoError(t, err)
		defer stop(context.Background())
		r = current.Load()
	} else {
		defer r.provider.Shutdown(context.Background())
	}
	engine := gin.New()
	engine.Use(RequestMiddleware(func(ctx context.Context) Identity {
		return Identity{UserID: ctx.Value(simulationUserKey{}).(int64), UserEmail: "session-smoke@example.invalid", GroupID: 1}
	}))
	engine.POST("/v1/responses", func(c *gin.Context) {
		// Read original body before constructing a different account session.
		body, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		require.NoError(t, c.Request.Body.Close())
		c.Header("X-Test-Trace", trace.SpanFromContext(c.Request.Context()).SpanContext().TraceID().String())
		calls := 0
		u := Wrap(&mockUpstream{do: func(req *http.Request) (*http.Response, error) {
			b, e := io.ReadAll(req.Body)
			require.NoError(t, e)
			require.Equal(t, string(body), string(b))
			calls++
			if calls == 1 {
				return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"retry"}`))}, nil
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":[{"type":"message","content":[{"type":"output_text","text":"session-smoke-answer"}]}],"usage":{"input_tokens":12,"output_tokens":3}}`))}, nil
		}})
		for account := int64(101); account <= 102; account++ {
			req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", "https://simulation.invalid/v1/responses", strings.NewReader(string(body)))
			req.Header.Set("session-id", fmt.Sprintf("account-converged-%d", account))
			resp, e := u.Do(req, "", account, 1)
			require.NoError(t, e)
			_, _ = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())
		}
		c.Status(200)
	})
	traces := []string{}
	for _, row := range []struct {
		user    int64
		session string
	}{{900000000101, "session-a"}, {900000000101, "session-a"}, {900000000102, "session-a"}, {900000000101, "session-b"}, {900000000101, ""}} {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"simulation","instructions":"session-smoke-system","input":[{"type":"function_call","name":"lookup","call_id":"call_1","arguments":"{\"id\":9007199254740993}"},{"type":"function_call_output","call_id":"call_1","output":"result"},{"role":"user","content":"session-smoke-latest"}]}`)).WithContext(context.WithValue(context.Background(), simulationUserKey{}, row.user))
		if row.session != "" {
			req.Header.Set("session-id", row.session)
		}
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code)
		traces = append(traces, w.Header().Get("X-Test-Trace"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, r.provider.ForceFlush(ctx))
	require.Zero(t, r.ExportFailures.Load())
	if os.Getenv("SUB2API_TEST_LIVE_LANGFUSE") == "1" {
		b, _ := json.Marshal(traces)
		t.Log("SESSION_SMOKE_TRACES", string(b))
		return
	}
	spans := exp.GetSpans()
	require.Len(t, spans, 15)
	sessions := []any{}
	for _, tid := range traces {
		var session any
		for _, span := range spans {
			if span.SpanContext.TraceID().String() == tid {
				v := attr(span.Attributes, "langfuse.session.id")
				if session == nil {
					session = v
				}
				require.Equal(t, session, v)
				if span.Name == "sub2api.request" {
					require.Equal(t, false, attr(span.Attributes, "sub2api.input.truncated"))
					require.Contains(t, attr(span.Attributes, "langfuse.observation.input"), "session-smoke-latest")
				}
			}
		}
		sessions = append(sessions, session)
	}
	require.Equal(t, sessions[0], sessions[1])
	require.NotEqual(t, sessions[0], sessions[2])
	require.NotEqual(t, sessions[0], sessions[3])
	require.Nil(t, sessions[4])
}
