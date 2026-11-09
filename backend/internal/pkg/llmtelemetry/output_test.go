package llmtelemetry

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func capturedOutput(t *testing.T, body string, stream, reasoning bool) (*observation, string) {
	t.Helper()
	cfg := testConfig()
	cfg.Backend = "langfuse"
	cfg.MaxBytes = 16384
	cfg.EventMaxBytes = 32768
	cfg.CaptureReasoning = reasoning
	o := newObservation(cfg, true, stream, time.Now())
	// Deliberately split inside UTF-8 and JSON strings/SSE framing.
	for i := 0; i < len(body); i += 7 {
		o.feed([]byte(body[i:min(i+7, len(body))]))
	}
	o.finish()
	s, _ := o.renderedOutput()
	return o, s
}
func sse(events ...string) string { return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n" }
func outputCalls(t *testing.T, text string) []gjson.Result {
	t.Helper()
	require.True(t, json.Valid([]byte(text)))
	calls := []gjson.Result{}
	gjson.Get(text, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("tool_calls").ForEach(func(_, c gjson.Result) bool { calls = append(calls, c); return true })
		return true
	})
	return calls
}
func TestStructuredResponsesParallelAndFinalSnapshot(t *testing.T) {
	body := sse(
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"it_a","type":"function_call","call_id":"call_a","name":"lookup","arguments":""}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"it_b","type":"custom_tool_call","call_id":"call_b","name":"shell","input":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"it_a","delta":"{\"n\":9007199254740993,"}`,
		`{"type":"response.custom_tool_call_input.delta","output_index":1,"delta":"echo "}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"password\":\"synthetic-secret\"}"}`,
		`{"type":"response.custom_tool_call_input.delta","output_index":1,"delta":"你好"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"n\":9007199254740993,\"password\":\"synthetic-secret\"}"}`,
		`{"type":"response.custom_tool_call_input.done","output_index":1,"input":"echo 你好"}`,
		`{"type":"response.completed","response":{"output":[{"id":"it_a","type":"function_call","call_id":"call_a","name":"lookup","arguments":"{\"n\":9007199254740993,\"password\":\"synthetic-secret\"}"},{"id":"it_b","type":"custom_tool_call","call_id":"call_b","name":"shell","input":"echo 你好"}],"usage":{"input_tokens":3,"output_tokens":4}}}`)
	o, text := capturedOutput(t, body, true, false)
	calls := outputCalls(t, text)
	require.Len(t, calls, 2)
	require.Equal(t, "call_a", calls[0].Get("id").String())
	require.Equal(t, "9007199254740993", gjson.Get(calls[0].Get("function.arguments").String(), "n").Raw)
	require.NotContains(t, text, "synthetic-secret")
	require.Equal(t, "complete", calls[0].Get("status").String())
	require.Equal(t, "custom", calls[1].Get("type").String())
	require.Equal(t, "echo 你好", calls[1].Get("custom.input").String())
	require.EqualValues(t, 4, o.outputTokens)
}
func TestStructuredChatParallelDeltaAndChoiceIsolation(t *testing.T) {
	body := sse(
		`{"choices":[{"index":0,"delta":{"content":"running","tool_calls":[{"index":0,"id":"one","type":"function","function":{"name":"look","arguments":"{\"a\":"}},{"index":1,"id":"two","type":"function","function":{"name":"sum","arguments":"{\"b\":"}}]}}]}`,
		`{"choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"three","type":"function","function":{"name":"third","arguments":"{}"}}]},"finish_reason":"tool_calls"},{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}},{"index":0,"function":{"name":"up","arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
		`[DONE]`)
	_, text := capturedOutput(t, body, true, false)
	calls := outputCalls(t, text)
	require.Len(t, calls, 3)
	require.Equal(t, "lookup", calls[0].Get("function.name").String())
	require.JSONEq(t, `{"a":1}`, calls[0].Get("function.arguments").String())
	require.JSONEq(t, `{"b":2}`, calls[1].Get("function.arguments").String())
	require.Equal(t, "three", calls[2].Get("id").String())
	for _, c := range calls {
		require.Equal(t, "complete", c.Get("status").String())
	}
	require.Contains(t, text, "running")
}
func TestStructuredMessagesParallelAndJSON(t *testing.T) {
	body := sse(
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"one","name":"lookup","input":{}}}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"two","name":"sum","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"n\":9007199254740993}"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"v\":2}"}}`,
		`{"type":"content_block_stop","index":1}`, `{"type":"content_block_stop","index":2}`, `{"type":"message_stop"}`)
	_, text := capturedOutput(t, body, true, false)
	calls := outputCalls(t, text)
	require.Len(t, calls, 2)
	require.Equal(t, `{"n":9007199254740993}`, calls[0].Get("function.arguments").String())
	require.Equal(t, "complete", calls[1].Get("status").String())
	for _, body := range []string{
		`{"content":[{"type":"text","text":"visible"},{"type":"tool_use","id":"one","name":"lookup","input":{"n":9007199254740993,"secret":"private-test"}}]}`,
		`{"choices":[{"message":{"content":"visible","tool_calls":[{"id":"one","type":"function","function":{"name":"lookup","arguments":"{\"n\":9007199254740993,\"secret\":\"private-test\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"output":[{"type":"message","content":[{"type":"output_text","text":"visible"}]},{"type":"function_call","id":"item","call_id":"one","name":"lookup","arguments":"{\"n\":9007199254740993,\"secret\":\"private-test\"}"}]}`,
	} {
		_, text := capturedOutput(t, body, false, false)
		calls := outputCalls(t, text)
		require.Len(t, calls, 1)
		require.Contains(t, text, "visible")
		require.NotContains(t, text, "private-test")
		require.Contains(t, calls[0].Get("function.arguments").String(), "9007199254740993")
	}
}
func TestStructuredIncompleteAndBounded(t *testing.T) {
	start := `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item","call_id":"one","name":"lookup","arguments":""}}`
	for _, tail := range []string{"", sse(`{"type":"response.failed"}`), sse(`{"type":"response.completed","response":{}}`)} {
		o, text := capturedOutput(t, sse(start, `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":"}`)+tail, true, false)
		calls := outputCalls(t, text)
		require.Len(t, calls, 1)
		require.Equal(t, "incomplete", calls[0].Get("status").String())
		o.readError = true
		o.readErrorKind = "io_error"
		text, _ = o.renderedOutput()
		require.Contains(t, text, "incomplete")
	}
	cfg := testConfig()
	cfg.Backend = "langfuse"
	o := newObservation(cfg, true, true, time.Now())
	o.feed([]byte(sse(start, `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"`+strings.Repeat("x", 2000)+`"}`, `{"type":"response.completed","response":{"usage":{"output_tokens":17}}}`)))
	o.finish()
	text, truncated := o.renderedOutput()
	require.True(t, truncated)
	require.True(t, json.Valid([]byte(text)))
	require.LessOrEqual(t, len(text), cfg.MaxBytes)
	require.EqualValues(t, 17, o.outputTokens)
	cfg.MaxBytes = 65536
	o = newObservation(cfg, true, true, time.Now())
	for i := 0; i < 1000; i++ {
		o.part(strings.Repeat("k", i+1), "tool")
	}
	require.LessOrEqual(t, len(o.structured.parts), 256)
	require.LessOrEqual(t, o.structured.used, cfg.MaxBytes)
	require.True(t, o.structured.truncated)
}
func TestReasoningOutputProtocolsAndDisabled(t *testing.T) {
	fixtures := []struct {
		body   string
		stream bool
	}{
		{`{"content":[{"type":"thinking","thinking":"public-thought","signature":"opaque-signature"},{"type":"redacted_thinking","data":"opaque-redacted"},{"type":"text","text":"answer"}]}`, false},
		{`{"choices":[{"message":{"reasoning_content":"public-thought","content":"answer"}}]}`, false},
		{`{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"public-thought"}],"encrypted_content":"opaque-cipher"},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`, false},
		{sse(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"public-thought"}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-signature"}}`, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`, `{"type":"message_stop"}`), true},
		{sse(`{"choices":[{"index":0,"delta":{"reasoning_content":"public-thought"}}]}`, `{"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`, `[DONE]`), true},
		{sse(`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"public-thought"}`, `{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"public-thought"}`, `{"type":"response.output_text.delta","output_index":1,"delta":"answer"}`, `{"type":"response.completed","response":{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"public-thought"}],"encrypted_content":"opaque-cipher"},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}}`), true},
	}
	for _, f := range fixtures {
		for _, enabled := range []bool{false, true} {
			_, text := capturedOutput(t, f.body, f.stream, enabled)
			require.Contains(t, text, "answer")
			if enabled {
				require.Contains(t, text, "reasoning_content")
				require.Equal(t, 1, strings.Count(text, "public-thought"))
			} else {
				require.NotContains(t, text, "public-thought")
				require.Equal(t, "answer", text)
			}
			for _, s := range []string{"opaque-signature", "opaque-redacted", "opaque-cipher"} {
				require.NotContains(t, text, s)
			}
		}
	}
}
func TestReasoningInputPolicyAndOversizedProjection(t *testing.T) {
	body := `{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"public-thought Bearer synthetic-credential"}],"encrypted_content":"opaque-cipher","signature":"opaque-signature"},{"role":"user","content":"hello"}],"messages_unused":"` + strings.Repeat("x", 5000) + `"}`
	for _, allowed := range []bool{false, true} {
		opts := []string{"langfuse"}
		if allowed {
			opts = append(opts, "reasoning")
		}
		for _, projected := range []bool{false, true} {
			input := body
			if projected {
				p := newJSONProjection(1024, allowed)
				p.feed([]byte(body))
				input = string(p.bytes())
				require.NotEmpty(t, input)
			}
			a := inputAttributes([]byte(input), false, true, 16384, opts...)
			text := attr(a, "langfuse.observation.input").(string)
			if allowed {
				require.Contains(t, text, "public-thought")
			} else {
				require.NotContains(t, text, "public-thought")
			}
			require.NotContains(t, text, "synthetic-credential")
			require.NotContains(t, text, "opaque-cipher")
			require.NotContains(t, text, "opaque-signature")
			require.Nil(t, attr(inputAttributes([]byte(input), false, false, 16384, opts...), "langfuse.observation.input"))
		}
		// Exercise the actual oversized request reader rather than only the projector.
		r := &observedInput{inner: io.NopCloser(strings.NewReader(body)), buf: boundedBuffer{limit: 1024}, reasoning: allowed}
		got, e := io.ReadAll(r)
		require.NoError(t, e)
		require.Equal(t, body, string(got))
		a := r.attrs(true, 16384, opts...)
		text := attr(a, "langfuse.observation.input").(string)
		require.Equal(t, allowed, strings.Contains(text, "public-thought"))
	}
	cfg := testConfig()
	cfg.CaptureReasoning = true
	cfg.CaptureGroups = map[int64]bool{7: true}
	for _, group := range []int64{0, 8} {
		o := newObservation(cfg, cfg.capture(group), false, time.Now())
		o.feed([]byte(`{"choices":[{"message":{"content":"answer","reasoning_content":"public-thought"}}]}`))
		o.finish()
		require.Nil(t, attr(o.attributes(), "langfuse.observation.output"))
		require.Empty(t, o.structured.parts)
	}
}
func TestReasoningConfig(t *testing.T) {
	t.Setenv("SUB2API_TELEMETRY_ENABLED", "true")
	t.Setenv("SUB2API_TELEMETRY_BACKEND", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318/v1/traces")
	t.Setenv("SUB2API_TELEMETRY_CAPTURE_REASONING", "")
	cfg, e := LoadConfig()
	require.NoError(t, e)
	require.False(t, cfg.CaptureReasoning)
	t.Setenv("SUB2API_TELEMETRY_CAPTURE_REASONING", "true")
	cfg, e = LoadConfig()
	require.NoError(t, e)
	require.True(t, cfg.CaptureReasoning)
	require.False(t, cfg.capture(1))
	t.Setenv("SUB2API_TELEMETRY_CAPTURE_REASONING", "invalid")
	_, e = LoadConfig()
	require.Error(t, e)
}
