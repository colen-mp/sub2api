package llmtelemetry

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/attribute"
)

var eventTypeName = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

var credentialText = regexp.MustCompile(`(?i)(Bearer\s+[^\s"\\]+|\b(?:sk|pk)-(?:lf-)?[A-Za-z0-9_-]{8,})`)
var credentialJSON = regexp.MustCompile(`(?i)("(?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|authorization|cookie|token)"\s*:\s*)"(?:\\.|[^"\\])*(?:"|$)`)

func redactText(s string) string {
	s = credentialJSON.ReplaceAllString(s, `${1}"[REDACTED]"`)
	return credentialText.ReplaceAllString(s, "[REDACTED]")
}
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}
func sensitiveKey(k string) bool {
	k = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
	switch k {
	case "authorization", "apikey", "accesstoken", "refreshtoken", "idtoken", "token", "password", "secret", "clientsecret", "cookie", "setcookie", "headers", "encryptedcontent", "signature", "thinkingsignature", "reasoningdetails", "data", "imagedata", "audio":
		return true
	}
	return false
}
func sanitize(v any, depth int) any { return sanitizeContent(v, depth, false) }

func sanitizeContent(v any, depth int, reasoning bool) any {
	if depth > 20 {
		return "[DEPTH LIMIT]"
	}
	switch x := v.(type) {
	case map[string]any:
		if kind, _ := x["type"].(string); kind == "redacted_thinking" || (!reasoning && (kind == "thinking" || kind == "reasoning" || kind == "reasoning_text" || kind == "summary_text")) {
			return "[REDACTED REASONING]"
		}
		if kind, _ := x["type"].(string); reasoning && (kind == "thinking" || kind == "reasoning") {
			// Whitelist public plaintext only; provider opaque fields stay out.
			allowed := map[string]any{"type": kind}
			for _, k := range []string{"thinking", "text", "summary", "content"} {
				if v, ok := x[k]; ok {
					allowed[k] = sanitizeContent(v, depth+1, true)
				}
			}
			return allowed
		}
		for k, value := range x {
			if !reasoning && (k == "reasoning_content" || k == "reasoning" || k == "thinking") {
				x[k] = "[REDACTED REASONING]"
			} else if sensitiveKey(k) {
				x[k] = "[REDACTED]"
			} else {
				if k == "arguments" {
					if text, ok := value.(string); ok {
						var decoded any
						if decodeJSON([]byte(text), &decoded) == nil {
							encoded, _ := json.Marshal(sanitizeContent(decoded, depth+1, reasoning))
							value = string(encoded)
						}
					}
				}
				x[k] = sanitizeContent(value, depth+1, reasoning)
			}
		}
		return x
	case []any:
		for i, value := range x {
			x[i] = sanitizeContent(value, depth+1, reasoning)
		}
		return x
	case string:
		return redactText(x)
	default:
		return v
	}
}

// boundedBuffer stops retaining bytes at the limit without changing transport reads.
type boundedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) add(p []byte) {
	n := b.limit - b.Len()
	if len(p) > n {
		b.truncated = true
		p = p[:max(n, 0)]
	}
	_, _ = b.Write(p)
}
func (b *boundedBuffer) text() string { return strings.ToValidUTF8(b.String(), "�") }

type observation struct {
	eventTypes                                                   map[string]int
	terminalEvent                                                string
	readErrorKind                                                string
	cfg                                                          Config
	capture                                                      bool
	started                                                      time.Time
	firstToken                                                   time.Time
	stream                                                       bool
	jsonBody                                                     boundedBuffer
	projection                                                   *jsonProjection
	sniff                                                        []byte
	detected                                                     bool
	line, event                                                  []byte
	dropLine, dropEvent                                          bool
	droppedEvents                                                int
	output                                                       boundedBuffer
	structured                                                   outputCapture
	terminal, failed, readError, complete                        bool
	model                                                        string
	inputTokens, outputTokens, cachedTokens, cacheCreationTokens int64
	usageSeen                                                    bool
}

func newObservation(cfg Config, capture, stream bool, start time.Time) *observation {
	return &observation{cfg: cfg, capture: capture, stream: stream, started: start, jsonBody: boundedBuffer{limit: cfg.EventMaxBytes}, output: boundedBuffer{limit: cfg.MaxBytes}}
}

func (o *observation) feed(p []byte) {
	if !o.stream && !o.detected {
		o.sniff = append(o.sniff, p...)
		trimmed := bytes.TrimSpace(o.sniff)
		if len(trimmed) == 0 && len(o.sniff) < 64 {
			return
		}
		if len(trimmed) > 0 && (trimmed[0] == 'd' || trimmed[0] == 'e' || trimmed[0] == ':') {
			o.stream = true
		}
		o.detected = true
		p = o.sniff
		o.sniff = nil
	}
	if !o.stream {
		if o.projection != nil {
			o.projection.feed(p)
		} else if o.jsonBody.Len()+len(p) > o.jsonBody.limit {
			o.projection = newJSONProjection(o.jsonBody.limit, o.cfg.CaptureReasoning)
			o.projection.feed(o.jsonBody.Bytes())
			o.projection.feed(p)
		}
		o.jsonBody.add(p)
		return
	}
	// Bounded incremental SSE lines: oversized events are skipped; subsequent
	// terminal usage is still parsed even after the content buffer fills.
	for _, b := range p {
		if b == '\n' {
			o.consumeLine()
			o.line = o.line[:0]
			o.dropLine = false
			continue
		}
		if !o.dropLine {
			if len(o.line) >= o.cfg.EventMaxBytes {
				o.dropLine = true
				o.dropEvent = true
				o.line = nil
			} else {
				o.line = append(o.line, b)
			}
		}
	}
}
func (o *observation) consumeLine() {
	if o.dropLine {
		return
	}
	line := bytes.TrimSuffix(o.line, []byte{'\r'})
	if len(line) == 0 {
		if o.dropEvent {
			o.droppedEvents++
		} else if len(o.event) > 0 {
			o.eventJSON(bytes.TrimSpace(o.event))
		}
		o.event = o.event[:0]
		o.dropEvent = false
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) || o.dropEvent {
		return
	}
	data := bytes.TrimPrefix(line, []byte("data:"))
	data = bytes.TrimPrefix(data, []byte(" "))
	if len(o.event)+len(data)+1 > o.cfg.EventMaxBytes {
		o.dropEvent = true
		o.event = nil
		return
	}
	if len(o.event) > 0 {
		o.event = append(o.event, '\n')
	}
	o.event = append(o.event, data...)
}
func (o *observation) finish() {
	if o.stream {
		if len(o.line) > 0 {
			o.consumeLine()
			o.line = nil
		}
		if len(o.event) > 0 && !o.dropEvent {
			o.eventJSON(bytes.TrimSpace(o.event))
			o.event = nil
		}
	} else if o.projection != nil {
		if body := o.projection.bytes(); body != nil {
			o.document(gjson.ParseBytes(body), true)
			o.terminal = true
		}
	} else if !o.jsonBody.truncated && gjson.ValidBytes(o.jsonBody.Bytes()) {
		o.document(gjson.ParseBytes(o.jsonBody.Bytes()), true)
		o.terminal = true
	}
}
func (o *observation) text(s string) { o.content("text", "text", s, false) }
func (o *observation) usage(v gjson.Result) {
	if !v.IsObject() {
		return
	}
	o.usageSeen = true
	for _, f := range []struct {
		keys []string
		dst  *int64
	}{
		{[]string{"input_tokens", "prompt_tokens"}, &o.inputTokens},
		{[]string{"output_tokens", "completion_tokens"}, &o.outputTokens},
		{[]string{"cache_read_input_tokens", "input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens"}, &o.cachedTokens},
		{[]string{"cache_creation_input_tokens"}, &o.cacheCreationTokens},
	} {
		for _, key := range f.keys {
			if x := v.Get(key); x.Exists() {
				*f.dst = x.Int()
				break
			}
		}
	}
}
func (o *observation) eventJSON(b []byte) {
	if bytes.Equal(b, []byte("[DONE]")) {
		o.terminal = true
		o.terminalEvent = "[DONE]"
		return
	}
	if !gjson.ValidBytes(b) {
		o.droppedEvents++
		return
	}
	v := gjson.ParseBytes(b)
	kind := v.Get("type").String()
	if len(kind) <= 64 && eventTypeName.MatchString(kind) {
		if o.eventTypes == nil {
			o.eventTypes = map[string]int{}
		}
		if len(o.eventTypes) < 24 || o.eventTypes[kind] > 0 {
			o.eventTypes[kind]++
		}
	}
	switch kind {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error", "message_stop":
		o.terminalEvent = kind
	}

	o.outputEvent(v)
	switch kind {
	case "message_start":
		o.model = v.Get("message.model").String()
		o.usage(v.Get("message.usage"))
	case "message_delta":
		o.usage(v.Get("usage"))
	case "message_stop":
		o.terminal = true
	case "response.completed", "response.done":
		o.terminal = true
		o.document(v.Get("response"), true)
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		o.failed = true
		o.terminal = true
		o.usage(v.Get("response.usage"))
	default:
		if v.Get("error").Exists() {
			o.failed = true
			o.terminal = true
		}
		o.document(v, false)
		v.Get("choices").ForEach(func(_, c gjson.Result) bool {
			if c.Get("finish_reason").Type != gjson.Null && c.Get("finish_reason").Exists() {
				o.terminal = true
			}
			return true
		})
	}
}
func (o *observation) document(v gjson.Result, content bool) {
	if model := v.Get("model").String(); model != "" {
		o.model = model
	}
	o.usage(v.Get("usage"))
	switch v.Get("status").String() {
	case "failed", "incomplete", "cancelled", "canceled":
		o.failed = true
	}
	if v.Get("error").Exists() && v.Get("error").Type != gjson.Null {
		o.failed = true
	}
	if !content {
		return
	}
	o.outputDocument(v)
}

// A provider terminal success precedes some transport cancellations. This does
// not prove client delivery; it only keeps that later cancellation from being
// misreported as a provider failure. Other read errors remain errors.
func (o *observation) canceledAfterCompleted() bool {
	if o.readErrorKind != "context_canceled" || o.failed || !o.terminal {
		return false
	}
	switch o.terminalEvent {
	case "response.completed", "response.done", "message_stop", "[DONE]":
		return true
	}
	return false
}
func (o *observation) attributes() []attribute.KeyValue {
	rendered, limited := o.renderedOutput()
	a := []attribute.KeyValue{attribute.Bool("sub2api.reasoning.capture_enabled", o.capture && o.cfg.CaptureReasoning), attribute.Bool("sub2api.stream", o.stream), attribute.Bool("sub2api.stream.incomplete", o.stream && !o.terminal), attribute.Bool("sub2api.output.truncated", limited || o.output.truncated || o.jsonBody.truncated), attribute.Int("sub2api.events.skipped", o.droppedEvents), attribute.Bool("sub2api.usage.observed", o.usageSeen)}
	if o.model != "" {
		a = append(a, attribute.String("gen_ai.response.model", clip(o.model, 256)))
	}
	if o.stream && !o.firstToken.IsZero() {
		a = append(a, attribute.Int64("sub2api.first_token_ms", o.firstToken.Sub(o.started).Milliseconds()))
	}
	if o.usageSeen {
		a = append(a, attribute.Int64("gen_ai.usage.input_tokens", o.inputTokens), attribute.Int64("gen_ai.usage.output_tokens", o.outputTokens), attribute.Int64("gen_ai.usage.cache_read.input_tokens", o.cachedTokens), attribute.Int64("gen_ai.usage.cache_creation.input_tokens", o.cacheCreationTokens))
		usage, _ := json.Marshal(map[string]int64{"input": o.inputTokens, "output": o.outputTokens, "cache_read_input_tokens": o.cachedTokens, "cache_creation_input_tokens": o.cacheCreationTokens})
		a = append(a, attribute.String("langfuse.observation.usage_details", string(usage)))
	}
	events, _ := json.Marshal(o.eventTypes)
	a = append(a, attribute.String("sub2api.events.types", string(events)), attribute.String("sub2api.response.terminal_event", o.terminalEvent), attribute.Bool("sub2api.response.failed", o.failed), attribute.Bool("sub2api.response.read_error", o.readError), attribute.String("sub2api.response.read_error_kind", o.readErrorKind), attribute.Bool("sub2api.response.canceled_after_completed", o.canceledAfterCompleted()))
	if o.capture && rendered != "" {
		text := rendered
		if o.cfg.Backend == "langfuse" {
			return append(a, attribute.String("langfuse.observation.output", text))
		}
		messages, _ := json.Marshal([]any{map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": text}}}})
		a = append(a, attribute.String("gen_ai.output.messages", string(messages)), attribute.String("langfuse.observation.output", text))
	}
	return a
}

func inputAttributes(body []byte, truncated, capture bool, maxBytes int, backend ...string) []attribute.KeyValue {
	a := []attribute.KeyValue{attribute.Bool("sub2api.input.truncated", truncated)}
	if model := gjson.GetBytes(body, "model").String(); model != "" {
		a = append(a, attribute.String("gen_ai.request.model", clip(model, 256)), attribute.String("langfuse.observation.model.name", clip(model, 256)))
	}
	// Never export a raw truncated JSON prefix (could contain unparsed credentials).
	if !capture || truncated || !gjson.ValidBytes(body) {
		return a
	}
	var doc map[string]any
	if decodeJSON(body, &doc) != nil {
		return a
	}
	projected := map[string]any{}
	for _, key := range []string{"system", "messages", "input", "instructions", "prompt", "tools"} {
		if v, ok := doc[key]; ok {
			projected[key] = sanitizeContent(v, 0, len(backend) > 1 && backend[1] == "reasoning")
		}
	}
	if len(backend) > 0 && backend[0] == "langfuse" {
		text, limited := langfuseInput(projected, maxBytes)
		a[0] = attribute.Bool("sub2api.input.truncated", limited)
		return append(a, attribute.String("langfuse.observation.input", text))
	}
	encoded, _ := json.Marshal(projected)
	text := clip(string(encoded), maxBytes)
	if len(encoded) > maxBytes {
		a[0] = attribute.Bool("sub2api.input.truncated", true)
	}
	msg, _ := json.Marshal([]any{map[string]any{"role": "user", "parts": []any{map[string]any{"type": "text", "content": text}}}})
	return append(a, attribute.String("gen_ai.input.messages", string(msg)), attribute.String("langfuse.observation.input", text))
}
