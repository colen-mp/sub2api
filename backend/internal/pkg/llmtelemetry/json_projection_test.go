package llmtelemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestLargeJSONProjectionKeepsMessagesAndTailUsage(t *testing.T) {
	cfg := testConfig()
	body := `{"model":"m","output":[{"type":"reasoning","encrypted_content":"` + strings.Repeat("secret", 2000) + `"},{"type":"message","content":[{"type":"output_text","text":"visible answer"}]}],"usage":{"input_tokens":17,"output_tokens":4}}`
	o := newObservation(cfg, true, false, time.Now())
	for i := 0; i < len(body); i += 7 {
		o.feed([]byte(body[i:min(i+7, len(body))]))
	}
	o.finish()
	require.True(t, o.terminal)
	require.EqualValues(t, 17, o.inputTokens)
	require.EqualValues(t, 4, o.outputTokens)
	require.Equal(t, "visible answer", o.output.String())
	require.NotContains(t, string(o.projection.bytes()), "secret")
	require.True(t, o.jsonBody.truncated)
}
func TestLargeInputProjectionRetainsRecentPromptAndRedacts(t *testing.T) {
	cfg := testConfig()
	body := `{"model":"m","instructions":"` + strings.Repeat("x", 10000) + `","input":[{"role":"user","content":"old"},{"role":"user","content":"current prompt","password":"private-value"}]}`
	b := &observedInput{inner: io.NopCloser(strings.NewReader(body)), buf: boundedBuffer{limit: cfg.EventMaxBytes}}
	got, e := io.ReadAll(b)
	require.NoError(t, e)
	require.Equal(t, body, string(got))
	attrs := b.attrs(true, cfg.MaxBytes)
	text := attr(attrs, "langfuse.observation.input").(string)
	require.Contains(t, text, "current prompt")
	require.NotContains(t, text, "private-value")
	require.True(t, attr(attrs, "sub2api.input.truncated").(bool))
	require.Nil(t, attr(b.attrs(false, cfg.MaxBytes), "langfuse.observation.input"))
}
func TestProjectionRejectsUnfinishedJSONAndBoundsArray(t *testing.T) {
	p := newJSONProjection(1024)
	p.feed([]byte(`{"input":[` + strings.Repeat(`{"text":"message"},`, 10000) + `{"text":"last"}]`))
	require.Nil(t, p.bytes())
	p.feed([]byte(`}`))
	require.NotNil(t, p.bytes())
	require.True(t, json.Valid(p.bytes()))
	require.Contains(t, string(p.bytes()), "last")
	require.Less(t, len(p.bytes()), 1200)
	p = newJSONProjection(1024)
	p.feed([]byte(`{"input":{"secret":"unfinished`))
	require.Nil(t, p.bytes())
}
func TestSSEBodyWithJSONHeaderAndEarlyCloseAtTerminal(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	r := newRuntime(testConfig(), exp)
	defer r.provider.Shutdown(context.Background())
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	inner := &mockUpstream{do: func(req *http.Request) (*http.Response, error) {
		_, e := io.ReadAll(req.Body)
		require.NoError(t, e)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
	}}
	ctx := context.WithValue(context.Background(), requestKey{}, &requestState{identity: func(context.Context) Identity { return Identity{GroupID: 1} }})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.invalid/v1/responses", strings.NewReader(`{"input":"hello"}`))
	resp, e := (&upstream{inner: inner, runtime: r}).Do(req, "", 1, 1)
	require.NoError(t, e)
	buf := make([]byte, len(body))
	_, e = io.ReadFull(resp.Body, buf)
	require.NoError(t, e)
	require.Equal(t, body, string(buf))
	require.NoError(t, resp.Body.Close())
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Unset, spans[0].Status.Code)
	require.Equal(t, "visible", attr(spans[0].Attributes, "langfuse.observation.output"))
	require.EqualValues(t, 1, attr(spans[0].Attributes, "gen_ai.usage.output_tokens"))
}

func TestRequestContentUsesFinalAttemptAndNoOtherAttributes(t *testing.T) {
	s := &requestState{}
	s.recordContent([]attribute.KeyValue{attribute.String("langfuse.observation.input", "first"), attribute.String("langfuse.observation.output", "failed attempt"), attribute.String("authorization", "private")})
	s.recordContent([]attribute.KeyValue{attribute.String("langfuse.observation.input", "final"), attribute.String("langfuse.observation.output", "answer")})
	require.Equal(t, "final", attr(s.contentAttributes(), "langfuse.observation.input"))
	require.Equal(t, "answer", attr(s.contentAttributes(), "langfuse.observation.output"))
	require.Nil(t, attr(s.contentAttributes(), "authorization"))
	s.recordContent(nil)
	require.Empty(t, s.contentAttributes())
}

func TestCustomToolCallOutputAndRedaction(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"deltas", "data: {\"type\":\"response.custom_tool_call_input.delta\",\"delta\":\"echo hello\"}\n\ndata: {\"type\":\"response.custom_tool_call_input.done\",\"input\":\"echo hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"custom_tool_call\",\"input\":\"echo hello\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"},
		{"done only", "data: {\"type\":\"response.custom_tool_call_input.done\",\"input\":\"echo hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"},
		{"json", `{"output":[{"type":"custom_tool_call","name":"exec","input":"echo hello","token":"private-tool-token"},{"type":"reasoning","summary":[{"text":"hidden-reasoning"}]}],"usage":{"output_tokens":3}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newObservation(testConfig(), true, tc.name != "json", time.Now())
			o.feed([]byte(tc.body))
			o.finish()
			output := attr(o.attributes(), "langfuse.observation.output").(string)
			require.Contains(t, output, "echo hello")
			require.Equal(t, 1, strings.Count(output, "echo hello"))
			require.NotContains(t, output, "private-tool-token")
			require.NotContains(t, output, "hidden-reasoning")
			require.True(t, o.terminal)
		})
	}
}

type errorAfterBody struct {
	data string
	err  error
}

func (b *errorAfterBody) Read(p []byte) (int, error) {
	if b.data == "" {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if b.data == "" {
		return n, b.err
	}
	return n, nil
}
func (b *errorAfterBody) Close() error { return nil }
func TestCancellationAfterCompletedDoesNotHideOtherFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		err        error
		code       codes.Code
	}{
		{"success then cancel", "data: {\"type\":\"response.completed\",\"response\":{}}\n\n", context.Canceled, codes.Unset},
		{"failed then cancel", "data: {\"type\":\"response.failed\",\"response\":{}}\n\n", context.Canceled, codes.Error},
		{"partial then cancel", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", context.Canceled, codes.Error},
		{"success then read failure", "data: {\"type\":\"response.completed\",\"response\":{}}\n\n", io.ErrUnexpectedEOF, codes.Error},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			r := newRuntime(testConfig(), exp)
			defer r.provider.Shutdown(context.Background())
			inner := &mockUpstream{do: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &errorAfterBody{data: tc.body, err: tc.err}, ContentLength: -1}, nil
			}}
			ctx := context.WithValue(context.Background(), requestKey{}, &requestState{identity: func(context.Context) Identity { return Identity{GroupID: 1} }})
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.invalid/v1/responses", nil)
			resp, e := (&upstream{inner: inner, runtime: r}).Do(req, "", 1, 1)
			require.NoError(t, e)
			got, e := io.ReadAll(resp.Body)
			require.ErrorIs(t, e, tc.err)
			require.Equal(t, tc.body, string(got))
			require.NoError(t, resp.Body.Close())
			require.NoError(t, r.provider.ForceFlush(context.Background()))
			spans := exp.GetSpans()
			require.Len(t, spans, 1)
			require.Equal(t, tc.code, spans[0].Status.Code)
			require.Equal(t, true, attr(spans[0].Attributes, "sub2api.response.read_error"))
		})
	}
}
