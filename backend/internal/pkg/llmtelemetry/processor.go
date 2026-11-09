package llmtelemetry

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const telemetryQueueBytes = 64 << 20
const telemetryBatchBytes = 4 << 20
const telemetryBatchSpans = 32

type queuedSpan struct {
	span  sdktrace.ReadOnlySpan
	bytes int
}

// Unlike a span-count-only queue, this also bounds retained attribute payload.
// One worker batches small spans, while an oversized span travels alone.
type contentProcessor struct {
	exporter            sdktrace.SpanExporter
	cfg                 Config
	dropped             *atomic.Uint64
	mu                  sync.Mutex
	queue               []queuedSpan
	bytes               int
	accepted, completed uint64
	stopped             bool
	changed             chan struct{}
	wake                chan struct{}
	done                chan struct{}
	ctx                 context.Context
	cancel              context.CancelFunc
	shutdownOnce        sync.Once
	shutdownErr         error
}

func newContentProcessor(cfg Config, exporter sdktrace.SpanExporter, dropped *atomic.Uint64) *contentProcessor {
	if cfg.BatchSize <= 0 || cfg.BatchSize > telemetryBatchSpans {
		cfg.BatchSize = telemetryBatchSpans
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &contentProcessor{exporter: exporter, cfg: cfg, dropped: dropped, changed: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	go p.run()
	return p
}
func (*contentProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func spanPayloadBytes(s sdktrace.ReadOnlySpan) int {
	// Include a conservative per-span allowance for IDs and small bookkeeping.
	n := 1024 + len(s.Name())
	for _, a := range s.Attributes() {
		n += len(a.Key) + len(a.Value.Emit())
	}
	return n
}
func (p *contentProcessor) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *contentProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	if !s.SpanContext().IsSampled() {
		return
	}
	if len(p.cfg.FieldMapping) > 0 {
		s = &mappedSpan{ReadOnlySpan: s, attrs: mapAttributes(s.Attributes(), p.cfg.FieldMapping)}
	}
	n := spanPayloadBytes(s)
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	if len(p.queue) >= p.cfg.QueueSize || n > telemetryQueueBytes-p.bytes {
		p.dropped.Add(1)
		p.mu.Unlock()
		p.signal()
		return
	}
	p.queue = append(p.queue, queuedSpan{s, n})
	p.bytes += n
	p.accepted++
	p.mu.Unlock()
	p.signal()
}
func (p *contentProcessor) run() {
	defer close(p.done)
	var reported uint64
	var lastReport time.Time
	for {
		// Report between batches even when sustained load never drains the queue.
		if n := p.dropped.Load(); n != reported && (reported == 0 || time.Since(lastReport) >= time.Minute) {
			log.Printf("LLM telemetry queue capacity exceeded (dropped_spans=%d); gateway continues", n)
			reported, lastReport = n, time.Now()
		}
		p.mu.Lock()
		if len(p.queue) == 0 {
			stopped := p.stopped
			p.mu.Unlock()
			if stopped {
				return
			}
			select {
			case <-p.wake:
				continue
			case <-p.ctx.Done():
				return
			}
		}
		count, size := 0, 0
		for count < len(p.queue) && count < telemetryBatchSpans && count < p.cfg.BatchSize {
			next := p.queue[count].bytes
			if count > 0 && size+next > telemetryBatchBytes {
				break
			}
			count++
			size += next
		}
		batch := make([]sdktrace.ReadOnlySpan, count)
		for i := range batch {
			batch[i] = p.queue[i].span
			p.queue[i] = queuedSpan{}
		}
		p.queue = p.queue[count:]
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(p.ctx, p.cfg.Timeout)
		_ = p.exporter.ExportSpans(ctx, batch)
		clear(batch)
		cancel()
		p.mu.Lock()
		p.bytes -= size
		p.completed += uint64(count)
		close(p.changed)
		p.changed = make(chan struct{})
		p.mu.Unlock()
	}
}
func (p *contentProcessor) ForceFlush(ctx context.Context) error {
	p.mu.Lock()
	target := p.accepted
	for p.completed < target {
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			p.mu.Lock()
			complete := p.completed >= target
			p.mu.Unlock()
			if complete {
				return nil
			}
			return errors.New("LLM telemetry processor stopped before flush")
		}
		p.mu.Lock()
	}
	p.mu.Unlock()
	return nil
}
func (p *contentProcessor) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.signal()
	select {
	case <-p.done:
	case <-ctx.Done():
		p.cancel()
		return ctx.Err()
	}
	p.cancel()
	p.shutdownOnce.Do(func() { p.shutdownErr = p.exporter.Shutdown(ctx) })
	return p.shutdownErr
}
