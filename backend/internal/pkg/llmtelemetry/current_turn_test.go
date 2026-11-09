package llmtelemetry

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"testing"
)

func TestCurrentTurnKeepsToolsAndOriginalContext(t *testing.T) {
	full := `{"messages":[{"role":"system","content":"instructions"},{"role":"user","content":"old"},{"role":"assistant","content":"old answer"},{"role":"user","content":"new"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"done"}],"tools":[{"name":"run"}]}`
	attrs := []attribute.KeyValue{attribute.String("langfuse.observation.input", full), attribute.String("langfuse.observation.output", "final"), attribute.Bool("sub2api.input.truncated", true)}
	got := currentTurnAttributes(attrs)
	var doc map[string]any
	require.NoError(t, decodeJSON([]byte(attr(got, "langfuse.observation.input").(string)), &doc))
	require.Len(t, doc["messages"], 3)
	require.Nil(t, doc["tools"])
	require.Equal(t, full, attrs[0].Value.AsString())
	require.Equal(t, "final", attr(got, "langfuse.observation.output"))
	require.Equal(t, true, attr(got, "sub2api.input.truncated"))
	require.EqualValues(t, 3, attr(got, "sub2api.input.history_messages_omitted"))
	require.Contains(t, got[0].Value.AsString(), "c1")
	require.Empty(t, currentTurnAttributes(nil))
}

func TestCurrentTurnWithoutUserDoesNotExposeInstructions(t *testing.T) {
	got := currentTurnAttributes([]attribute.KeyValue{attribute.String("langfuse.observation.input", `{"messages":[{"role":"system","content":"system only"}]}`)})
	require.NotContains(t, got[0].Value.AsString(), "system only")
	require.Contains(t, got[0].Value.AsString(), "No user turn identified")
}

func TestBillingCorrelationUsesOwnerPrecedence(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "server1")
	require.Equal(t, "local:server1", attr(billingCorrelationAttributes(ctx), "sub2api.billing_request_id"))
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "client1")
	require.Equal(t, "client:client1", attr(billingCorrelationAttributes(ctx), "sub2api.billing_request_id"))
	require.Empty(t, billingCorrelationAttributes(context.WithValue(ctx, ctxkey.ClientRequestID, "sk-do-not-export")))
}
