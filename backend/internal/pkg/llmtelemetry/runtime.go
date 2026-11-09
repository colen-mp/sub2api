package llmtelemetry

import (
	"context"
	"errors"
	"log"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var current atomic.Pointer[Runtime]

type Runtime struct {
	cfg            Config
	provider       *sdktrace.TracerProvider
	tracer         trace.Tracer
	ExportFailures atomic.Uint64
	DroppedSpans   atomic.Uint64
}

// InitFromEnv creates a private provider: unrelated application OTel is untouched.
// Invalid enabled configuration fails startup without printing endpoint/headers.
func InitFromEnv(ctx context.Context) (func(context.Context) error, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.Endpoint), otlptracehttp.WithHeaders(cfg.Headers), otlptracehttp.WithTimeout(cfg.Timeout), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
	if err != nil {
		return nil, errors.New("could not initialize OTLP exporter")
	}
	r := newRuntime(cfg, exp)
	current.Store(r)
	log.Printf("LLM telemetry enabled: backend=%s content=%t", cfg.Backend, cfg.CaptureContent)
	return func(ctx context.Context) error { current.CompareAndSwap(r, nil); return r.provider.Shutdown(ctx) }, nil
}

func newRuntime(cfg Config, exporter sdktrace.SpanExporter) *Runtime {
	r := &Runtime{cfg: cfg}
	attrs := []attribute.KeyValue{attribute.String("service.name", cfg.ServiceName)}
	if env := os.Getenv("DD_ENV"); env != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", env))
	}
	r.provider = sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attrs...)),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(newContentProcessor(cfg, &safeExporter{SpanExporter: exporter, runtime: r}, &r.DroppedSpans)),
		// JSON escaping of control characters can expand one byte to six bytes.
		// Preserve valid structured messages instead of SDK-truncating their JSON.
		sdktrace.WithSpanLimits(sdktrace.SpanLimits{AttributeCountLimit: 64, EventCountLimit: 0, LinkCountLimit: 0, AttributeValueLengthLimit: 6*cfg.MaxBytes + 4096}),
	)
	r.tracer = r.provider.Tracer("sub2api.native-llm", trace.WithInstrumentationVersion("0.2.19-otel.1"))
	return r
}

type safeExporter struct {
	sdktrace.SpanExporter
	runtime        *Runtime
	lastFailureLog atomic.Int64
}

func (e *safeExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := e.SpanExporter.ExportSpans(ctx, spans); err != nil {
		n := e.runtime.ExportFailures.Add(1)
		now := time.Now().Unix()
		previous := e.lastFailureLog.Load()
		if (n == 1 || now-previous >= 60) && e.lastFailureLog.CompareAndSwap(previous, now) {
			log.Printf("LLM telemetry export failed (batches=%d); gateway continues", n)
		}
		// HTTP exporters can include response bodies in errors. Never log those.
		return errors.New("LLM telemetry export failed")
	}
	return nil
}

func ShutdownTimeout(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		log.Print("LLM telemetry shutdown incomplete")
	}
}
