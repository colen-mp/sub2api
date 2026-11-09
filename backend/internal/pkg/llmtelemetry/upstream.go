package llmtelemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// This port is structurally identical to service.HTTPUpstream, avoiding any
// dependency on business services or generated Wire providers.
type Upstream interface {
	Do(*http.Request, string, int64, int) (*http.Response, error)
	DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error)
}
type upstream struct {
	inner   Upstream
	runtime *Runtime
}

func Wrap(inner Upstream) Upstream {
	r := current.Load()
	if r == nil {
		return inner
	}
	return &upstream{inner: inner, runtime: r}
}
func (u *upstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	return u.intercept(req, accountID, func(r *http.Request) (*http.Response, error) { return u.inner.Do(r, proxy, accountID, concurrency) })
}
func (u *upstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.intercept(req, accountID, func(r *http.Request) (*http.Response, error) {
		return u.inner.DoWithTLS(r, proxy, accountID, concurrency, profile)
	})
}

func (u *upstream) intercept(req *http.Request, accountID int64, call func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	state, _ := req.Context().Value(requestKey{}).(*requestState)
	if state == nil || req.Method != http.MethodPost || !llmPath(req.URL.Path) {
		return call(req)
	}
	r := u.runtime
	// The root reflects the latest attempt, including transport failures.
	state.recordContent(nil)
	id := state.identity(req.Context())
	capture := r.cfg.capture(id.GroupID)
	protocol := "responses"
	if strings.HasSuffix(req.URL.Path, "/messages") {
		protocol = "messages"
	}
	if strings.HasSuffix(req.URL.Path, "/chat/completions") {
		protocol = "chat_completions"
	}
	attrs := append(identityAttributes(id), attribute.Int64("sub2api.account_id", accountID), attribute.String("gen_ai.operation.name", "chat"), attribute.String("langfuse.observation.type", "generation"), attribute.String("sub2api.protocol", protocol))
	attrs = append(attrs, state.sessionAttributes(id)...)
	if model, ok := req.Context().Value(ctxkey.Model).(string); ok {
		attrs = append(attrs, attribute.String("sub2api.requested_model", clip(redactText(model), 256)))
	}
	if requestID, ok := req.Context().Value(ctxkey.RequestID).(string); ok {
		attrs = append(attrs, attribute.String("sub2api.request_id", clip(requestID, 256)))
	}
	ctx, span := r.tracer.Start(req.Context(), "sub2api.upstream", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	forward := req.Clone(ctx)
	var input *observedInput
	if req.Body != nil {
		input = &observedInput{inner: req.Body, buf: boundedBuffer{limit: r.cfg.EventMaxBytes}, reasoning: r.cfg.CaptureReasoning}
		forward.Body = input
	}
	started := time.Now()
	resp, err := call(forward)
	if err != nil {
		if input != nil {
			attrs := input.attrs(capture, r.cfg.MaxBytes, r.cfg.contentOptions()...)
			state.recordContent(attrs)
			span.SetAttributes(attrs...)
		}
		span.SetStatus(codes.Error, "upstream transport failed")
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
		span.End()
		return resp, err
	}
	if resp == nil {
		span.SetStatus(codes.Error, "nil upstream response")
		span.End()
		return resp, nil
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if rid := resp.Header.Get("x-request-id"); rid != "" {
		span.SetAttributes(attribute.String("sub2api.upstream_request_id", clip(rid, 256)))
	}
	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	observer := newObservation(r.cfg, capture, isSSE, started)
	finish := func(o *observation) {
		attrs := o.attributes()
		if input != nil {
			attrs = append(attrs, input.attrs(capture, r.cfg.MaxBytes, r.cfg.contentOptions()...)...)
		}
		state.recordContent(attrs)
		span.SetAttributes(attrs...)
		span.SetAttributes(attribute.String("sub2api.response.content_type", clip(resp.Header.Get("Content-Type"), 128)), attribute.Int64("sub2api.response.content_length", resp.ContentLength))
		span.SetAttributes(attribute.Bool("sub2api.response.incomplete", !o.complete && !o.terminal))
		if resp.StatusCode >= 400 || o.failed || (o.readError && !o.canceledAfterCompleted()) || (!o.complete && !o.terminal) || (o.stream && !o.terminal) {
			span.SetStatus(codes.Error, "upstream failed or incomplete")
		}
		span.End()
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		observer.complete = true
		finish(observer)
		return resp, nil
	}
	resp.Body = &observedResponse{inner: resp.Body, observer: observer, finish: finish, expected: resp.ContentLength}
	return resp, nil
}

type observedInput struct {
	inner      io.ReadCloser
	reasoning  bool
	mu         sync.Mutex
	buf        boundedBuffer
	projection *jsonProjection
	closeOnce  sync.Once
	closeErr   error
}

func (b *observedInput) Read(p []byte) (int, error) {
	n, e := b.inner.Read(p)
	if n > 0 {
		b.mu.Lock()
		if b.projection != nil {
			b.projection.feed(p[:n])
		} else if b.buf.Len()+n > b.buf.limit {
			b.projection = newJSONProjection(b.buf.limit, b.reasoning)
			b.projection.feed(b.buf.Bytes())
			b.projection.feed(p[:n])
		}
		b.buf.add(p[:n])
		b.mu.Unlock()
	}
	return n, e
}
func (b *observedInput) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.inner.Close() })
	return b.closeErr
}
func (b *observedInput) attrs(capture bool, limit int, backend ...string) []attribute.KeyValue {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.projection != nil {
		if body := b.projection.bytes(); body != nil {
			attrs := inputAttributes(body, false, capture, limit, backend...)
			attrs[0] = attribute.Bool("sub2api.input.truncated", true)
			return attrs
		}
	}
	return inputAttributes(b.buf.Bytes(), b.buf.truncated, capture, limit, backend...)
}

type observedResponse struct {
	inner          io.ReadCloser
	mu             sync.Mutex
	observer       *observation
	finish         func(*observation)
	ended          bool
	expected, read int64
	closeOnce      sync.Once
	closeErr       error
}

func (b *observedResponse) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ended {
		if n > 0 {
			b.read += int64(n)
			b.observer.feed(p[:n])
		}
		if err != nil {
			b.observer.complete = err == io.EOF
			b.observer.readError = err != io.EOF
			if err != io.EOF {
				b.observer.readErrorKind = "read_failed"
				if errors.Is(err, context.Canceled) {
					b.observer.readErrorKind = "context_canceled"
				}
			}
			b.end()
		}
	}
	return n, err
}
func (b *observedResponse) end() {
	if b.ended {
		return
	}
	b.ended = true
	if b.expected > 0 && b.read == b.expected {
		b.observer.complete = true
	}
	b.observer.finish()
	b.finish(b.observer)
}
func (b *observedResponse) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.inner.Close() })
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closeErr != nil {
		b.observer.readError = true
	}
	b.end()
	return b.closeErr
}
