package llmtelemetry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLargeSingleItemPreservesTailAndRedacts(t *testing.T) {
	cfg := testConfig()
	cfg.Backend = "langfuse"
	cfg.MaxBytes, cfg.EventMaxBytes = 16<<20, 32<<20
	text := strings.Repeat("x", 12<<20) + "large-content-tail"
	body, err := json.Marshal(map[string]any{"input": text, "tools": []any{map[string]any{"password": "private-value"}}})
	require.NoError(t, err)
	a := inputAttributes(body, false, true, cfg.MaxBytes, cfg.Backend)
	require.Nil(t, attr(a, "gen_ai.input.messages"))
	input := attr(a, "langfuse.observation.input").(string)
	require.Contains(t, input, "large-content-tail")
	require.NotContains(t, input, "private-value")
	require.Equal(t, false, attr(a, "sub2api.input.truncated"))
	response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": text}}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 34}})
	require.NoError(t, err)
	o := newObservation(cfg, true, false, time.Now())
	for start := 0; start < len(response); start += 8192 {
		end := start + 8192
		if end > len(response) {
			end = len(response)
		}
		o.feed(response[start:end])
	}
	o.finish()
	require.Nil(t, attr(o.attributes(), "gen_ai.output.messages"))
	require.Equal(t, text, attr(o.attributes(), "langfuse.observation.output"))
	require.Equal(t, int64(12), o.inputTokens)
	require.Equal(t, int64(34), o.outputTokens)
	require.Nil(t, attr(inputAttributes(body, false, false, cfg.MaxBytes), "langfuse.observation.input"))
}

func TestLargeContentConfigBoundaries(t *testing.T) {
	t.Setenv("SUB2API_TELEMETRY_ENABLED", "true")
	t.Setenv("SUB2API_TELEMETRY_BACKEND", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318/v1/traces")
	t.Setenv("SUB2API_TELEMETRY_CAPTURE_MAX_BYTES", "16777216")
	t.Setenv("SUB2API_TELEMETRY_EVENT_MAX_BYTES", "33554432")
	t.Setenv("SUB2API_TELEMETRY_QUEUE_SIZE", "256")
	c, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 32, c.BatchSize)
	require.Equal(t, 256, c.QueueSize)
	t.Setenv("SUB2API_TELEMETRY_CAPTURE_MAX_BYTES", "16777217")
	_, err = LoadConfig()
	require.Error(t, err)
	t.Setenv("SUB2API_TELEMETRY_CAPTURE_MAX_BYTES", "16777216")
	t.Setenv("SUB2API_TELEMETRY_EVENT_MAX_BYTES", "33554433")
	_, err = LoadConfig()
	require.Error(t, err)
}
