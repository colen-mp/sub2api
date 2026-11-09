package llmtelemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Explicit opt-in: synthetic provider responses, real OTLP export to local
// Langfuse only. This does not call a model or impersonate a real employee.
func TestLocalLangfuseStructuredFixtures(t *testing.T) {
	if os.Getenv("SUB2API_TELEMETRY_LOCAL_FIXTURES") != "true" {
		t.Skip("local ingestion opt-in required")
	}
	require.Equal(t, "http://127.0.0.1:3001/api/public/otel/v1/traces", os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	t.Setenv("SUB2API_TELEMETRY_ENABLED", "true")
	t.Setenv("SUB2API_TELEMETRY_BACKEND", "langfuse")
	t.Setenv("SUB2API_TELEMETRY_CAPTURE_CONTENT", "true")
	t.Setenv("SUB2API_TELEMETRY_CAPTURE_REASONING", "true")
	t.Setenv("SUB2API_TELEMETRY_CAPTURE_GROUP_IDS", "")
	shutdown, e := InitFromEnv(context.Background())
	require.NoError(t, e)
	defer shutdown(context.Background())
	gin.SetMode(gin.TestMode)
	for _, fixture := range []struct{ path, body string }{
		{"/v1/responses", sse(`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"synthetic-summary"}`, `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"item-test","call_id":"call-test","name":"lookup","arguments":""}}`, `{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"n\":9007199254740993}"}`, `{"type":"response.completed","response":{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"synthetic-summary"}],"encrypted_content":"private-cipher"},{"type":"function_call","id":"item-test","call_id":"call-test","name":"lookup","arguments":"{\"n\":9007199254740993}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`)},
		{"/v1/messages", sse(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"synthetic-summary"}}`, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-test","name":"lookup","input":{}}}`, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"n\":9007199254740993}"}}`, `{"type":"content_block_stop","index":1}`, `{"type":"message_delta","usage":{"output_tokens":1}}`, `{"type":"message_stop"}`)},
		{"/v1/chat/completions", sse(`{"choices":[{"index":0,"delta":{"reasoning_content":"synthetic-summary","tool_calls":[{"index":0,"id":"call-test","type":"function","function":{"name":"lookup","arguments":"{\"n\":"}}]}}]}`, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"9007199254740993}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, `[DONE]`)},
	} {
		inner := &mockUpstream{do: func(req *http.Request) (*http.Response, error) {
			_, e := io.ReadAll(req.Body)
			require.NoError(t, e)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(fixture.body))}, nil
		}}
		u := Wrap(inner)
		engine := gin.New()
		engine.Use(RequestMiddleware(func(context.Context) Identity {
			return Identity{UserID: 999999, GroupID: 999999, Platform: "synthetic-fixture"}
		}))
		engine.POST(fixture.path, func(c *gin.Context) {
			span := trace.SpanFromContext(c.Request.Context())
			span.SetAttributes(attribute.StringSlice("langfuse.trace.tags", []string{"synthetic-protocol-fixture"}))
			t.Logf("fixture_trace path=%s trace_id=%s", fixture.path, span.SpanContext().TraceID().String())
			req, e := http.NewRequestWithContext(c.Request.Context(), "POST", "https://synthetic.invalid"+fixture.path, strings.NewReader(`{"model":"synthetic-fixture","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"synthetic-input-summary"}],"encrypted_content":"private-cipher"},{"role":"user","content":"synthetic telemetry fixture"}]}`))
			require.NoError(t, e)
			resp, e := u.Do(req, "", 999999, 1)
			require.NoError(t, e)
			_, e = io.Copy(c.Writer, resp.Body)
			require.NoError(t, e)
			require.NoError(t, resp.Body.Close())
		})
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest("POST", fixture.path, nil))
		require.Equal(t, fixture.body, w.Body.String())
	}
	require.NoError(t, current.Load().provider.ForceFlush(context.Background()))
}
