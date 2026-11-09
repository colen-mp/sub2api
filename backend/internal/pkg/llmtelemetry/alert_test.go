package llmtelemetry

import (
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type stepExporter struct {
	entered chan struct{}
	release chan struct{}
}

func (e *stepExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	e.entered <- struct{}{}
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (*stepExporter) Shutdown(context.Context) error { return nil }

type alertLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *alertLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}
func (b *alertLog) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.text.String() }

func TestQueueAlertDoesNotWaitForQueueToDrain(t *testing.T) {
	cfg := testConfig()
	cfg.QueueSize = 2
	cfg.BatchSize = 1
	cfg.Timeout = 5 * time.Second
	exp := &stepExporter{entered: make(chan struct{}, 8), release: make(chan struct{}, 8)}
	logs := &alertLog{}
	before := log.Writer()
	log.SetOutput(logs)
	defer log.SetOutput(before)
	r := newRuntime(cfg, exp)
	defer r.provider.Shutdown(context.Background())
	defer close(exp.release)
	_, first := r.tracer.Start(context.Background(), "first")
	first.End()
	<-exp.entered
	for i := 0; i < 3; i++ {
		_, s := r.tracer.Start(context.Background(), "queued")
		s.End()
	}
	require.EqualValues(t, 1, r.DroppedSpans.Load())
	exp.release <- struct{}{}
	select {
	case <-exp.entered:
	case <-time.After(time.Second):
		t.Fatal("second batch missing")
	}
	// The second export is blocked and a third span remains queued.
	require.Contains(t, logs.String(), "LLM telemetry queue capacity exceeded")
	require.NotContains(t, logs.String(), "queued")
}
