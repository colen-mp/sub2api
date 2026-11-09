package llmtelemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func testConfig() Config {
	return Config{Enabled: true, Backend: "otlp", ServiceName: "sub2api-test", Headers: map[string]string{}, CaptureGroups: map[int64]bool{}, CaptureContent: true, MaxBytes: 1024, EventMaxBytes: 4096, QueueSize: 8, BatchSize: 2, Timeout: 100 * time.Millisecond, FlushInterval: 10 * time.Millisecond}
}
func attr(attrs []attribute.KeyValue, key string) any {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value.AsInterface()
		}
	}
	return nil
}

func TestConfigurationDestinationsAndNoSecretErrors(t *testing.T) {
	t.Setenv("SUB2API_TELEMETRY_ENABLED", "false")
	t.Setenv("LANGFUSE_SECRET_KEY", "secret-do-not-log")
	c, e := LoadConfig()
	require.NoError(t, e)
	require.False(t, c.Enabled)
	t.Setenv("SUB2API_TELEMETRY_ENABLED", "true")
	t.Setenv("SUB2API_TELEMETRY_BACKEND", "langfuse")
	t.Setenv("LANGFUSE_HOST", "https://langfuse.example/")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	c, e = LoadConfig()
	require.NoError(t, e)
	require.Equal(t, "https://langfuse.example/api/public/otel/v1/traces", c.Endpoint)
	require.True(t, strings.HasPrefix(c.Headers["Authorization"], "Basic "))
	require.Equal(t, "4", c.Headers["x-langfuse-ingestion-version"])
	t.Setenv("SUB2API_TELEMETRY_BACKEND", "datadog")
	t.Setenv("DD_SITE", "ap1.datadoghq.com")
	t.Setenv("DD_API_KEY", "dd-test")
	c, e = LoadConfig()
	require.NoError(t, e)
	require.Equal(t, "https://otlp.ap1.datadoghq.com/v1/traces", c.Endpoint)
	require.Equal(t, "dd-test", c.Headers["dd-api-key"])
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://secret-do-not-log@bad.example/traces")
	_, e = LoadConfig()
	require.Error(t, e)
	require.NotContains(t, e.Error(), "secret-do-not-log")
}

func TestGroupCapturePolicy(t *testing.T) {
	c := testConfig()
	require.False(t, c.capture(0))
	require.True(t, c.capture(9))
	c.CaptureGroups = map[int64]bool{7: true}
	require.False(t, c.capture(9))
	require.True(t, c.capture(7))
	c.CaptureContent = false
	require.False(t, c.capture(7))
}

func TestTruncatedToolSecretsAndEscapedOutputRemainSafe(t *testing.T) {
	require.NotContains(t, redactText(`{"token":"secret-cut-off`), "secret-cut-off")
	input := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"password\":\"nested-secret\"}"}}]},{"type":"thinking","thinking":"private-reasoning"}]}`)
	a := inputAttributes(input, false, true, 4096)
	text := attr(a, "langfuse.observation.input").(string)
	require.NotContains(t, text, "nested-secret")
	require.NotContains(t, text, "private-reasoning")
	o := newObservation(testConfig(), true, true, time.Now())
	o.text(strings.Repeat("\x01", 1024))
	encoded := attr(o.attributes(), "gen_ai.output.messages").(string)
	require.True(t, json.Valid([]byte(encoded)))
}

func TestSSETruncationStillReadsTailUsageAndTools(t *testing.T) {
	c := testConfig()
	o := newObservation(c, true, true, time.Now())
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", 1800) + "\"}\r\n\r\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{}\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"test-model\",\"usage\":{\"input_tokens\":123,\"output_tokens\":456,\"input_tokens_details\":{\"cached_tokens\":17}}}}\n\n"
	for i := 0; i < len(body); i += 7 {
		o.feed([]byte(body[i:min(i+7, len(body))]))
	}
	o.finish()
	require.True(t, o.terminal)
	require.True(t, o.output.truncated)
	require.EqualValues(t, 123, o.inputTokens)
	require.EqualValues(t, 456, o.outputTokens)
	require.EqualValues(t, 17, o.cachedTokens)
	require.LessOrEqual(t, o.output.Len(), c.MaxBytes)
	require.Equal(t, "test-model", attr(o.attributes(), "gen_ai.response.model"))
}

func TestOversizedSSEEventIsSkippedWithoutLosingLaterUsage(t *testing.T) {
	c := testConfig()
	o := newObservation(c, true, true, time.Now())
	o.feed([]byte("data: " + strings.Repeat("x", 10000) + "\n\n"))
	require.LessOrEqual(t, len(o.line)+len(o.event), 2*c.EventMaxBytes)
	o.feed([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":99}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
	o.finish()
	require.Positive(t, o.droppedEvents)
	require.EqualValues(t, 99, o.outputTokens)
	require.True(t, o.terminal)
}

func TestCaptureDisabledRedactionAndPartialJSON(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"Bearer abc-secret sk-abcdefghijk"}],"tools":[{"token":"tool-secret"}],"metadata":{"token":"hidden"}}`)
	a := inputAttributes(body, false, false, 4096)
	require.Nil(t, attr(a, "langfuse.observation.input"))
	require.Equal(t, "m", attr(a, "gen_ai.request.model"))
	a = inputAttributes(body, false, true, 4096)
	text := attr(a, "langfuse.observation.input").(string)
	require.NotContains(t, text, "abc-secret")
	require.NotContains(t, text, "sk-abcdefghijk")
	require.NotContains(t, text, "tool-secret")
	require.NotContains(t, text, "metadata")
	a = inputAttributes([]byte(`{"messages":[{"api_key":"secret`), true, true, 4096)
	require.Nil(t, attr(a, "langfuse.observation.input"))
	require.NotContains(t, redactText(`tool {"token":"secret-value"}`), "secret-value")
}

func TestProtocolParsers(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		stream     bool
		in, out    int64
		text       string
	}{
		{"anthropic", "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":11,\"cache_read_input_tokens\":5}}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\ndata: {\"type\":\"message_stop\"}\n\n", true, 11, 7, "hello"},
		{"chat", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n", true, 9, 3, "hello"},
		{"json", `{"model":"m","output":[{"content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":4,"output_tokens":2}}`, false, 4, 2, "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newObservation(testConfig(), true, tc.stream, time.Now())
			o.feed([]byte(tc.body))
			o.finish()
			require.True(t, o.terminal)
			require.Equal(t, tc.in, o.inputTokens)
			require.Equal(t, tc.out, o.outputTokens)
			require.Contains(t, o.output.String(), tc.text)
		})
	}
}

type mockUpstream struct {
	do      func(*http.Request) (*http.Response, error)
	profile *tlsfingerprint.Profile
}

func (m *mockUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return m.do(r)
}
func (m *mockUpstream) DoWithTLS(r *http.Request, _ string, _ int64, _ int, p *tlsfingerprint.Profile) (*http.Response, error) {
	m.profile = p
	return m.do(r)
}

type countedBody struct {
	io.Reader
	closes atomic.Int64
}

func (b *countedBody) Close() error { b.closes.Add(1); return nil }

func TestTransportPreservesOversizedRequestResponseAndClose(t *testing.T) {
	c := testConfig()
	exp := tracetest.NewInMemoryExporter()
	r := newRuntime(c, exp)
	defer r.provider.Shutdown(context.Background())
	request := `{"model":"test","input":"` + strings.Repeat("z", 10000) + `"}`
	response := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"
	innerBody := &countedBody{Reader: strings.NewReader(response)}
	inner := &mockUpstream{do: func(req *http.Request) (*http.Response, error) {
		body, e := io.ReadAll(req.Body)
		require.NoError(t, e)
		require.Equal(t, request, string(body))
		require.NoError(t, req.Body.Close())
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: innerBody}, nil
	}}
	u := &upstream{inner: inner, runtime: r}
	ctx := context.WithValue(context.Background(), requestKey{}, &requestState{identity: func(context.Context) Identity { return Identity{UserID: 3, GroupID: 4, Platform: "openai"} }})
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/v1/responses", strings.NewReader(request))
	require.NoError(t, e)
	profile := &tlsfingerprint.Profile{}
	resp, e := u.DoWithTLS(req, "proxy-is-not-exported", 10, 5, profile)
	require.NoError(t, e)
	require.Same(t, profile, inner.profile)
	got, e := io.ReadAll(resp.Body)
	require.NoError(t, e)
	require.Equal(t, response, string(got))
	require.NoError(t, resp.Body.Close())
	require.NoError(t, resp.Body.Close())
	require.EqualValues(t, 1, innerBody.closes.Load())
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, int64(10), attr(spans[0].Attributes, "sub2api.account_id"))
	require.Equal(t, "3", attr(spans[0].Attributes, "user.id"))
	require.Equal(t, true, attr(spans[0].Attributes, "sub2api.input.truncated"))
	require.Contains(t, attr(spans[0].Attributes, "langfuse.observation.input"), "[TRUNCATED: observation limit]")
	require.Equal(t, int64(2), attr(spans[0].Attributes, "gen_ai.usage.output_tokens"))
}

func TestEarlyCloseEndsOnceAndMarksIncomplete(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	r := newRuntime(testConfig(), exp)
	defer r.provider.Shutdown(context.Background())
	b := &countedBody{Reader: strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")}
	inner := &mockUpstream{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: b}, nil
	}}
	ctx := context.WithValue(context.Background(), requestKey{}, &requestState{identity: func(context.Context) Identity { return Identity{UserID: 1, GroupID: 1} }})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://test.invalid/v1/responses", nil)
	resp, e := (&upstream{inner: inner, runtime: r}).Do(req, "", 1, 1)
	require.NoError(t, e)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, resp.Body.Close())
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Error, spans[0].Status.Code)
	require.Equal(t, true, attr(spans[0].Attributes, "sub2api.stream.incomplete"))
	require.EqualValues(t, 1, b.closes.Load())
}

func TestDisabledAndBackgroundCallsAreNotObserved(t *testing.T) {
	old := current.Swap(nil)
	defer current.Store(old)
	inner := &mockUpstream{do: func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 204}, nil }}
	require.Same(t, inner, Wrap(inner))
	exp := tracetest.NewInMemoryExporter()
	r := newRuntime(testConfig(), exp)
	defer r.provider.Shutdown(context.Background())
	req, _ := http.NewRequest("POST", "https://test.invalid/v1/responses", nil)
	_, e := (&upstream{inner: inner, runtime: r}).Do(req, "", 1, 1)
	require.NoError(t, e)
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	require.Empty(t, exp.GetSpans())
}

func TestRequestStatusDistinguishesCancellationAndFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, contextError, description string
		status                          int
		write, cancel, deadline, panic  bool
		code                            codes.Code
	}{
		{name: "success", status: 200, write: true},
		{name: "cancel after write", status: 200, write: true, cancel: true, contextError: "context_canceled"},
		{name: "cancel before write", status: 200, cancel: true, contextError: "context_canceled"},
		{name: "HTTP failure", status: 502, write: true, code: codes.Error, description: "request HTTP error"},
		{name: "HTTP failure and cancel", status: 502, write: true, cancel: true, contextError: "context_canceled", code: codes.Error, description: "request HTTP error"},
		{name: "deadline after write", status: 200, write: true, deadline: true, contextError: "deadline_exceeded", code: codes.Error, description: "request deadline exceeded"},
		{name: "panic before write", status: 200, panic: true, code: codes.Error, description: "request panicked"},
		{name: "panic after write and cancel", status: 200, write: true, cancel: true, panic: true, contextError: "context_canceled", code: codes.Error, description: "request panicked"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			r := newRuntime(testConfig(), exp)
			old := current.Swap(r)
			defer current.Store(old)
			defer r.provider.Shutdown(context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.deadline {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			engine := gin.New()
			engine.Use(RequestMiddleware(func(context.Context) Identity { return Identity{} }))
			engine.POST("/v1/responses", func(c *gin.Context) {
				if tt.write {
					c.String(tt.status, "response")
				}
				if tt.cancel {
					cancel()
				}
				if tt.panic {
					panic("private-panic-content")
				}
			})
			serve := func() {
				engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx))
			}
			if tt.panic {
				require.PanicsWithValue(t, "private-panic-content", serve)
			} else {
				serve()
			}
			require.NoError(t, r.provider.ForceFlush(context.Background()))
			spans := exp.GetSpans()
			require.Len(t, spans, 1)
			s := spans[0]
			require.Equal(t, tt.code, s.Status.Code)
			require.Equal(t, tt.description, s.Status.Description)
			require.Equal(t, int64(tt.status), attr(s.Attributes, "http.response.status_code"))
			require.Equal(t, tt.write, attr(s.Attributes, "sub2api.request.response_written"))
			if tt.contextError != "" {
				require.Equal(t, tt.contextError, attr(s.Attributes, "sub2api.request.context_error"))
			} else {
				require.Nil(t, attr(s.Attributes, "sub2api.request.context_error"))
			}
			if tt.cancel {
				require.Equal(t, true, attr(s.Attributes, "sub2api.request.canceled"))
			} else {
				require.Nil(t, attr(s.Attributes, "sub2api.request.canceled"))
			}
			encoded, err := json.Marshal(s)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "private-panic-content")
		})
	}
}

func TestCanceledHTTP200KeepsUpstreamStreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"failed\"}}}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
	} {
		t.Run(body, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			r := newRuntime(testConfig(), exp)
			old := current.Swap(r)
			defer current.Store(old)
			defer r.provider.Shutdown(context.Background())
			u := Wrap(&mockUpstream{do: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			engine := gin.New()
			engine.Use(RequestMiddleware(func(context.Context) Identity { return Identity{} }))
			engine.POST("/v1/responses", func(c *gin.Context) {
				req, err := http.NewRequestWithContext(c.Request.Context(), "POST", "https://test.invalid/v1/responses", nil)
				require.NoError(t, err)
				resp, err := u.Do(req, "", 1, 1)
				require.NoError(t, err)
				_, err = io.Copy(c.Writer, resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				cancel()
			})
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx))
			require.Equal(t, 200, w.Code)
			require.Equal(t, body, w.Body.String())
			require.NoError(t, r.provider.ForceFlush(context.Background()))
			spans := exp.GetSpans()
			require.Len(t, spans, 2)
			for _, s := range spans {
				if s.Name == "sub2api.request" {
					require.Equal(t, codes.Unset, s.Status.Code)
					require.Equal(t, true, attr(s.Attributes, "sub2api.request.canceled"))
				} else {
					require.Equal(t, codes.Error, s.Status.Code)
					require.Equal(t, "upstream failed or incomplete", s.Status.Description)
				}
			}
		})
	}
}

func TestRequestAndRetrySpansShareTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	exp := tracetest.NewInMemoryExporter()
	r := newRuntime(testConfig(), exp)
	old := current.Swap(r)
	defer current.Store(old)
	defer r.provider.Shutdown(context.Background())
	var calls int
	inner := &mockUpstream{do: func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("secret must not be logged")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":2,"output_tokens":1},"choices":[{"message":{"content":"ok"}}]}`)), Header: http.Header{}}, nil
	}}
	u := Wrap(inner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine := gin.New()
	engine.Use(RequestMiddleware(func(context.Context) Identity {
		return Identity{UserID: 7, UserEmail: "member@example.com", GroupID: 8}
	}))
	engine.POST("/v1/responses", func(c *gin.Context) {
		for i := 0; i < 2; i++ {
			req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", "https://test.invalid/v1/responses", strings.NewReader(`{"model":"m"}`))
			resp, e := u.Do(req, "", int64(i+1), 1)
			if e == nil {
				_, _ = io.Copy(c.Writer, resp.Body)
				_ = resp.Body.Close()
			}
		}
		cancel() // Cancellation at completion must not overwrite a recovered request.
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx))
	require.Equal(t, 200, w.Code)
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	spans := exp.GetSpans()
	require.Len(t, spans, 3)
	var root *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "sub2api.request" {
			root = &spans[i]
		}
	}
	require.NotNil(t, root)
	require.Equal(t, codes.Unset, root.Status.Code)
	require.Equal(t, true, attr(root.Attributes, "sub2api.request.canceled"))
	for _, s := range spans {
		require.Equal(t, root.SpanContext.TraceID(), s.SpanContext.TraceID())
		require.Equal(t, "member@example.com", attr(s.Attributes, "user.id"))
		require.Equal(t, "member@example.com", attr(s.Attributes, "langfuse.user.id"))
		require.EqualValues(t, 7, attr(s.Attributes, "sub2api.user_id"))
		if s.Name == "sub2api.upstream" {
			require.Equal(t, root.SpanContext.SpanID(), s.Parent.SpanID())
			if attr(s.Attributes, "sub2api.account_id") == int64(1) {
				require.Equal(t, codes.Error, s.Status.Code)
			} else {
				require.Equal(t, codes.Unset, s.Status.Code)
			}
		}
		require.NotContains(t, s.Status.Description, "secret")
	}
}

func TestRealOTLPHTTPExportAuthenticationAndContent(t *testing.T) {
	for _, backend := range []string{"langfuse", "datadog"} {
		t.Run(backend, func(t *testing.T) {
			var mu sync.Mutex
			var received []*collector.ExportTraceServiceRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				require.Equal(t, "/v1/traces", req.URL.Path)
				require.Equal(t, "application/x-protobuf", req.Header.Get("Content-Type"))
				if backend == "langfuse" {
					require.True(t, strings.HasPrefix(req.Header.Get("Authorization"), "Basic "))
					require.Equal(t, "4", req.Header.Get("x-langfuse-ingestion-version"))
				} else {
					require.Equal(t, "local-test-key", req.Header.Get("dd-api-key"))
					require.Equal(t, "llmobs", req.Header.Get("dd-otlp-source"))
				}
				body, _ := io.ReadAll(req.Body)
				var msg collector.ExportTraceServiceRequest
				if e := proto.Unmarshal(body, &msg); e != nil {
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				received = append(received, &msg)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/x-protobuf")
				_, _ = w.Write([]byte{})
			}))
			defer srv.Close()
			t.Setenv("SUB2API_TELEMETRY_ENABLED", "true")
			t.Setenv("SUB2API_TELEMETRY_BACKEND", backend)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", srv.URL+"/v1/traces")
			t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-local")
			t.Setenv("LANGFUSE_SECRET_KEY", "sk-local")
			t.Setenv("DD_API_KEY", "local-test-key")
			shutdown, e := InitFromEnv(context.Background())
			require.NoError(t, e)
			r := current.Load()
			_, span := r.tracer.Start(context.Background(), "local-export-test")
			span.SetAttributes(attribute.String("gen_ai.operation.name", "chat"))
			span.End()
			require.NoError(t, r.provider.ForceFlush(context.Background()))
			require.NoError(t, shutdown(context.Background()))
			mu.Lock()
			defer mu.Unlock()
			require.NotEmpty(t, received)
			encoded, _ := proto.Marshal(received[0])
			require.True(t, bytes.Contains(encoded, []byte("local-export-test")))
		})
	}
}

type blockedExporter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *blockedExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	e.once.Do(func() { close(e.started) })
	select {
	case <-e.release:
		return errors.New("sensitive-response-body")
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *blockedExporter) Shutdown(context.Context) error { return nil }
func TestFullQueueAndExporterOutageDoNotBlockRequests(t *testing.T) {
	c := testConfig()
	c.QueueSize = 2
	c.BatchSize = 1
	c.Timeout = time.Second
	e := &blockedExporter{started: make(chan struct{}), release: make(chan struct{})}
	r := newRuntime(c, e)
	_, s := r.tracer.Start(context.Background(), "first")
	s.End()
	select {
	case <-e.started:
	case <-time.After(time.Second):
		t.Fatal("export never started")
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_, s := r.tracer.Start(context.Background(), "request")
			s.End()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("span enqueue blocked on exporter")
	}
	close(e.release)
	require.NoError(t, r.provider.Shutdown(context.Background()))
	require.Positive(t, r.ExportFailures.Load())
}
