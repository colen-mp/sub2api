package llmtelemetry

import (
	"context"
	"go.opentelemetry.io/otel/attribute"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type pausedExporter struct {
	*tracetest.InMemoryExporter
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	batchesMu sync.Mutex
	sizes     []int
}

func (e *pausedExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.once.Do(func() { close(e.entered) })
	select {
	case <-e.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.batchesMu.Lock()
	e.sizes = append(e.sizes, len(spans))
	e.batchesMu.Unlock()
	return e.InMemoryExporter.ExportSpans(ctx, spans)
}

// Reproduce an export backlog deterministically, rather than relying on sleeps.
func TestConcurrentRequestsSurviveTemporaryExportBacklog(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBytes = 16 << 20
	cfg.EventMaxBytes = 32 << 20
	cfg.QueueSize = 64
	cfg.BatchSize = 1
	cfg.Timeout = 10 * time.Second
	exp := &pausedExporter{InMemoryExporter: tracetest.NewInMemoryExporter(), entered: make(chan struct{}), release: make(chan struct{})}
	r := newRuntime(cfg, exp)
	defer r.provider.Shutdown(context.Background())
	_, first := r.tracer.Start(context.Background(), "export-in-flight")
	first.End()
	select {
	case <-exp.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("export did not start")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, root := r.tracer.Start(context.Background(), "sub2api.request")
			_, child := r.tracer.Start(ctx, "sub2api.upstream")
			child.End()
			root.End()
		}()
	}
	wg.Wait()
	close(exp.release)
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	spans := exp.GetSpans()
	require.Len(t, spans, 65)
	traces := map[string]int{}
	for _, s := range spans {
		if s.Name != "export-in-flight" {
			traces[s.SpanContext.TraceID().String()]++
		}
	}
	require.Len(t, traces, 32)
	for _, n := range traces {
		require.Equal(t, 2, n)
	}
}

func TestThousandConcurrentRequestsWithSlowExporter(t *testing.T) {
	cfg := testConfig()
	cfg.QueueSize = 2048
	cfg.Timeout = 10 * time.Second
	exp := &pausedExporter{InMemoryExporter: tracetest.NewInMemoryExporter(), entered: make(chan struct{}), release: make(chan struct{})}
	r := newRuntime(cfg, exp)
	defer r.provider.Shutdown(context.Background())
	_, first := r.tracer.Start(context.Background(), "in-flight")
	first.End()
	select {
	case <-exp.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("export did not start")
	}
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, root := r.tracer.Start(context.Background(), "request")
			_, child := r.tracer.Start(ctx, "upstream")
			child.End()
			root.End()
		}()
	}
	wg.Wait()
	close(exp.release)
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	require.Len(t, exp.GetSpans(), 2001)
	exp.batchesMu.Lock()
	max := 0
	for _, n := range exp.sizes {
		if n > max {
			max = n
		}
	}
	exp.batchesMu.Unlock()
	require.Greater(t, max, 1)
	require.LessOrEqual(t, max, 32)
	require.Zero(t, r.DroppedSpans.Load())
}

func TestQueueCountOverflowAndFlushCancellation(t *testing.T) {
	cfg := testConfig()
	cfg.QueueSize = 2
	cfg.Timeout = 10 * time.Second
	exp := &pausedExporter{InMemoryExporter: tracetest.NewInMemoryExporter(), entered: make(chan struct{}), release: make(chan struct{})}
	r := newRuntime(cfg, exp)
	defer r.provider.Shutdown(context.Background())
	_, first := r.tracer.Start(context.Background(), "in-flight")
	first.End()
	<-exp.entered
	for i := 0; i < 3; i++ {
		_, span := r.tracer.Start(context.Background(), "queued")
		span.End()
	}
	require.EqualValues(t, 1, r.DroppedSpans.Load())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, r.provider.ForceFlush(ctx), context.Canceled)
	close(exp.release)
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	require.Len(t, exp.GetSpans(), 3)
}

func TestQueuePayloadBudgetAndLargeSpansExportAlone(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBytes = 16 << 20
	cfg.QueueSize = 1024
	cfg.Timeout = 10 * time.Second
	exp := &pausedExporter{InMemoryExporter: tracetest.NewInMemoryExporter(), entered: make(chan struct{}), release: make(chan struct{})}
	r := newRuntime(cfg, exp)
	defer r.provider.Shutdown(context.Background())
	_, first := r.tracer.Start(context.Background(), "in-flight")
	first.End()
	<-exp.entered
	text := strings.Repeat("x", 16<<20)
	for i := 0; i < 4; i++ {
		_, span := r.tracer.Start(context.Background(), "large")
		span.SetAttributes(attribute.String("langfuse.observation.input", text))
		span.End()
	}
	// The in-flight payload is included, so three of four 16 MiB spans fit.
	require.EqualValues(t, 1, r.DroppedSpans.Load())
	close(exp.release)
	require.NoError(t, r.provider.ForceFlush(context.Background()))
	require.Len(t, exp.GetSpans(), 4)
	exp.batchesMu.Lock()
	defer exp.batchesMu.Unlock()
	for _, n := range exp.sizes {
		require.Equal(t, 1, n)
	}
}

func TestSlowExportShutdownHasBoundedWait(t *testing.T) {
	cfg := testConfig()
	cfg.Timeout = 10 * time.Second
	exp := &pausedExporter{InMemoryExporter: tracetest.NewInMemoryExporter(), entered: make(chan struct{}), release: make(chan struct{})}
	var dropped atomic.Uint64
	p := newContentProcessor(cfg, exp, &dropped)
	// Obtain an immutable SDK span without involving another processor queue.
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	_, span := provider.Tracer("test").Start(context.Background(), "slow")
	span.End()
	p.OnEnd(recorder.Ended()[0])
	<-exp.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, p.Shutdown(ctx), context.DeadlineExceeded)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	require.NoError(t, p.Shutdown(context.Background()))
	require.NoError(t, p.ForceFlush(context.Background()))
	require.NoError(t, provider.Shutdown(context.Background()))
}
