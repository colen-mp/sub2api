package llmtelemetry

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestGenericMappingUsesCompletedSpanAttributes(t *testing.T) {
	cfg := testConfig()
	cfg.FieldMapping = map[string]string{
		"sub2api.user_name":         "langfuse.user.id",
		"custom.regions":            "langfuse.trace.tags",
		"sub2api.account_id":        "langfuse.observation.metadata.account",
		"http.response.status_code": "company.response.status",
		"gen_ai.response.model":     "langfuse.version",
		"future.missing":            "new.destination",
	}
	exp := tracetest.NewInMemoryExporter()
	r := newRuntime(cfg, exp)
	defer r.provider.Shutdown(context.Background())
	_, s := r.tracer.Start(context.Background(), "mapping-probe", trace.WithAttributes(identityAttributes(Identity{UserID: 7, UserEmail: "owner@example.com", UserName: "member"})...))
	// These values become available after Span start, including upstream results.
	s.SetAttributes(attribute.StringSlice("custom.regions", []string{"team-a", "team-b"}), attribute.Int64("sub2api.account_id", 42), attribute.Int("http.response.status_code", 201), attribute.String("gen_ai.response.model", "model-v2"))
	s.End()
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	a := spans[0].Attributes
	require.Equal(t, "member", attr(a, "user.id"))
	require.Equal(t, "member", attr(a, "langfuse.user.id"))
	require.Equal(t, "user_name", attr(a, "sub2api.user_identity_source"))
	require.Equal(t, "owner@example.com", attr(a, "sub2api.user_email"))
	require.Equal(t, []string{"team-a", "team-b"}, attr(a, "langfuse.trace.tags"))
	require.EqualValues(t, 42, attr(a, "langfuse.observation.metadata.account"))
	require.EqualValues(t, 201, attr(a, "company.response.status"))
	require.Equal(t, "model-v2", attr(a, "langfuse.version"))
	require.Nil(t, attr(a, "new.destination"))
	require.EqualValues(t, 7, attr(a, "sub2api.user_id"))
}

func TestGenericMappingSnapshotAndNativeTypes(t *testing.T) {
	before := []attribute.KeyValue{attribute.String("a", "new"), attribute.String("b", "old"), attribute.Int("status", 200), attribute.String("group", "team,one")}
	attrs := mapAttributes(before, map[string]string{"a": "b", "b": "c", "status": "user.id", "group": "langfuse.trace.tags"})
	require.Equal(t, "new", attr(attrs, "b"))
	require.Equal(t, "old", attr(attrs, "c"))  // No cascading through the mapped b.
	require.Equal(t, "old", attr(before, "b")) // Original snapshot is untouched.
	require.Equal(t, "200", attr(attrs, "langfuse.user.id"))
	require.Equal(t, []string{"team,one"}, attr(attrs, "langfuse.trace.tags"))
	attrs = mapAttributes(before, map[string]string{"a": "new.nested.attribute"})
	require.Equal(t, "new", attr(attrs, "new.nested.attribute"))
	attrs = mapAttributes(before, map[string]string{"absent": "user.id"})
	require.Nil(t, attr(attrs, "user.id"))
}

func TestMappedPayloadStillUsesQueueByteBudget(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBytes = 16 << 20
	cfg.FieldMapping = map[string]string{"a": "copy.a", "b": "copy.b", "c": "copy.c"}
	exp := tracetest.NewInMemoryExporter()
	r := newRuntime(cfg, exp)
	defer r.provider.Shutdown(context.Background())
	value := strings.Repeat("x", 12<<20)
	_, s := r.tracer.Start(context.Background(), "budget-probe", trace.WithAttributes(attribute.String("a", value), attribute.String("b", value), attribute.String("c", value)))
	s.End() // Original 36 MiB fits; mapped 72 MiB must be rejected before enqueue.
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	require.EqualValues(t, 1, r.DroppedSpans.Load())
	require.Empty(t, exp.GetSpans())
}
