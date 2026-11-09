package llmtelemetry

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Only selected content is retained. Both bytes and cardinality are bounded;
// deltas never retain the entire event stream. Snapshots replace, not append.
type outputPart struct {
	key, kind, id, itemID, name, toolType string
	value                                 strings.Builder
	done, truncated, delta                bool
}
type outputCapture struct {
	parts     []*outputPart
	used      int
	truncated bool
}

func (o *observation) part(key, kind string) *outputPart {
	if !o.capture {
		return nil
	}
	if kind == "reasoning" && !o.cfg.CaptureReasoning {
		return nil
	}
	for _, p := range o.structured.parts {
		if p.key == key {
			return p
		}
	}
	if len(o.structured.parts) >= 256 || o.structured.used+len(key)+64 > o.cfg.MaxBytes {
		o.structured.truncated = true
		return nil
	}
	p := &outputPart{key: key, kind: kind, toolType: "function"}
	o.structured.used += len(key) + 64
	o.structured.parts = append(o.structured.parts, p)
	return p
}
func (o *observation) field(p *outputPart, dst *string, s string, replace bool) {
	if p == nil {
		return
	}
	if replace {
		o.structured.used -= len(*dst)
		*dst = ""
	}
	remaining := max(o.cfg.MaxBytes-o.structured.used, 0)
	if len(s) > remaining {
		s = clip(s, remaining)
		p.truncated = true
		o.structured.truncated = true
	}
	*dst += s
	o.structured.used += len(s)
}
func (o *observation) value(p *outputPart, s string, replace bool) {
	if p == nil {
		return
	}
	if replace {
		if p.value.String() == s {
			return
		}
		o.structured.used -= p.value.Len()
		p.value.Reset()
	}
	remaining := max(o.cfg.MaxBytes-o.structured.used, 0)
	if len(s) > remaining {
		s = clip(s, remaining)
		p.truncated = true
		o.structured.truncated = true
	}
	p.value.WriteString(s)
	o.structured.used += len(s)
}
func (o *observation) content(key, kind, s string, replace bool) {
	if s == "" {
		return
	}
	if kind != "reasoning" && o.firstToken.IsZero() {
		o.firstToken = time.Now()
	}
	p := o.part(key, kind)
	if p == nil {
		return
	}
	wasEmpty := p.value.Len() == 0
	o.value(p, s, replace)
	// Keep the existing text preview; final snapshots do not duplicate deltas.
	if kind == "text" && (!replace || wasEmpty) {
		o.output.add([]byte(s))
	}
}
func (o *observation) toolPart(key string, v gjson.Result, protocol string, done bool) *outputPart {
	if o.firstToken.IsZero() {
		o.firstToken = time.Now()
	}
	p := o.part(key, "tool")
	if p == nil {
		return nil
	}
	id, name, kind := v.Get("call_id").String(), v.Get("name").String(), v.Get("type").String()
	if protocol == "messages" {
		id = v.Get("id").String()
	}
	if protocol == "chat" {
		id = v.Get("id").String()
		name = v.Get("function.name").String()
	}
	if id != "" {
		o.field(p, &p.id, id, true)
	}
	if itemID := v.Get("id").String(); protocol == "responses" && itemID != "" {
		o.field(p, &p.itemID, itemID, true)
	}
	if name != "" {
		o.field(p, &p.name, name, protocol != "chat" || done)
	}
	if kind == "custom_tool_call" || kind == "custom" {
		p.toolType = "custom"
	}
	arg := v.Get("arguments")
	if protocol == "messages" {
		arg = v.Get("input")
	}
	if protocol == "chat" {
		arg = v.Get("function.arguments")
		if p.toolType == "custom" {
			name = v.Get("custom.name").String()
			if name != "" {
				o.field(p, &p.name, name, done)
			}
			arg = v.Get("custom.input")
		}
	}
	if p.toolType == "custom" && protocol == "responses" {
		arg = v.Get("input")
	}
	if arg.Exists() {
		s := arg.String()
		if arg.Type != gjson.String {
			s = arg.Raw
		}
		if done || !p.delta {
			o.value(p, s, true)
		} else if protocol == "chat" {
			o.value(p, s, false)
		}
		if protocol == "chat" && !done {
			p.delta = true
		}
	}
	if done {
		p.done = true
		switch v.Get("status").String() {
		case "in_progress", "incomplete", "failed":
			p.done = false
		}
	}
	return p
}
func (o *observation) toolDelta(key, s string) {
	p := o.part(key, "tool")
	if p == nil {
		return
	}
	if !p.delta {
		o.value(p, "", true)
		p.delta = true
	}
	o.value(p, s, false)
	if s != "" && o.firstToken.IsZero() {
		o.firstToken = time.Now()
	}
}
func (o *observation) toolsDone(prefix string) {
	for _, p := range o.structured.parts {
		if p.kind == "tool" && strings.HasPrefix(p.key, prefix) {
			p.done = true
		}
	}
}
func responseKey(v gjson.Result) string {
	// output_index is stable across added/delta/done/final snapshots.
	if x := v.Get("output_index"); x.Exists() {
		return "r:" + x.String()
	}
	if id := v.Get("item_id").String(); id != "" {
		return "ri:" + id
	}
	return "r:0"
}
func (o *observation) responsePartKey(v gjson.Result) string {
	if !v.Get("output_index").Exists() {
		id := v.Get("item_id").String()
		for _, p := range o.structured.parts {
			if id != "" && p.itemID == id {
				return p.key
			}
		}
	}
	return responseKey(v)
}
func (o *observation) responseItem(key string, item gjson.Result, done bool) {
	if id := item.Get("id").String(); id != "" {
		for _, p := range o.structured.parts {
			if p.itemID == id || p.key == "ri:"+id {
				key = p.key
				break
			}
		}
	}
	switch item.Get("type").String() {
	case "function_call", "custom_tool_call":
		o.toolPart(key, item, "responses", done)
	case "reasoning":
		item.Get("summary").ForEach(func(i, x gjson.Result) bool {
			if x.Get("type").String() == "summary_text" {
				o.content(key+":s:"+i.String(), "reasoning", x.Get("text").String(), true)
			}
			return true
		})
		item.Get("content").ForEach(func(i, x gjson.Result) bool {
			if x.Get("type").String() == "reasoning_text" {
				o.content(key+":q:"+i.String(), "reasoning", x.Get("text").String(), true)
			}
			return true
		})
	default:
		item.Get("content").ForEach(func(i, x gjson.Result) bool {
			if x.Get("type").String() == "output_text" || x.Get("type").String() == "text" {
				o.content(key+":t:"+i.String(), "text", x.Get("text").String(), true)
			}
			return true
		})
	}
}
func (o *observation) outputEvent(v gjson.Result) {
	kind := v.Get("type").String()
	key := o.responsePartKey(v)
	switch kind {
	case "response.output_item.added", "response.output_item.done":
		o.responseItem(key, v.Get("item"), kind == "response.output_item.done")
	case "response.output_text.delta", "response.output_text.done":
		s := v.Get("delta").String()
		done := kind == "response.output_text.done"
		if done {
			s = v.Get("text").String()
		}
		o.content(key+":t:"+indexString(v, "content_index"), "text", s, done)
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		o.toolDelta(key, v.Get("delta").String())
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		p := o.part(key, "tool")
		if p != nil {
			field := "arguments"
			if strings.Contains(kind, "custom_") {
				field = "input"
				p.toolType = "custom"
			}
			if v.Get(field).Exists() {
				o.value(p, v.Get(field).String(), true)
			}
			if n := v.Get("name").String(); n != "" {
				o.field(p, &p.name, n, true)
			}
			p.done = true
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done":
		done := strings.HasSuffix(kind, ".done")
		s := v.Get("delta").String()
		if done {
			s = v.Get("text").String()
		}
		infix, idx := ":s:", "summary_index"
		if strings.HasPrefix(kind, "response.reasoning_text.") {
			infix = ":q:"
			idx = "content_index"
		}
		o.content(key+infix+indexString(v, idx), "reasoning", s, done)
	case "content_block_start":
		key = "a:" + indexString(v, "index")
		b := v.Get("content_block")
		switch b.Get("type").String() {
		case "tool_use":
			o.toolPart(key, b, "messages", false)
		case "text":
			o.content(key, "text", b.Get("text").String(), true)
		case "thinking":
			o.content(key, "reasoning", b.Get("thinking").String(), true)
		}
	case "content_block_delta":
		key = "a:" + indexString(v, "index")
		d := v.Get("delta")
		switch d.Get("type").String() {
		case "text_delta":
			o.content(key, "text", d.Get("text").String(), false)
		case "input_json_delta":
			o.toolDelta(key, d.Get("partial_json").String())
		case "thinking_delta":
			o.content(key, "reasoning", d.Get("thinking").String(), false)
		}
	case "content_block_stop":
		for _, p := range o.structured.parts {
			if p.key == "a:"+indexString(v, "index") {
				p.done = true
			}
		}
	}
	v.Get("choices").ForEach(func(_, c gjson.Result) bool {
		prefix := "c:" + indexString(c, "index") + ":"
		d := c.Get("delta")
		o.content(prefix+"text", "text", d.Get("content").String(), false)
		for _, f := range []string{"reasoning_content", "reasoning"} {
			if x := d.Get(f); x.Type == gjson.String {
				o.content(prefix+"reasoning", "reasoning", x.String(), false)
				break
			}
		}
		d.Get("tool_calls").ForEach(func(_, t gjson.Result) bool {
			o.toolPart(prefix+"tool:"+indexString(t, "index"), t, "chat", false)
			return true
		})
		switch c.Get("finish_reason").String() {
		case "tool_calls", "stop":
			o.toolsDone(prefix)
		}
		return true
	})
}
func indexString(v gjson.Result, key string) string {
	if x := v.Get(key); x.Exists() {
		return x.String()
	}
	return "0"
}
func (o *observation) outputDocument(v gjson.Result) {
	v.Get("content").ForEach(func(i, b gjson.Result) bool {
		key := "a:" + i.String()
		switch b.Get("type").String() {
		case "text":
			o.content(key, "text", b.Get("text").String(), true)
		case "tool_use":
			o.toolPart(key, b, "messages", true)
		case "thinking":
			o.content(key, "reasoning", b.Get("thinking").String(), true)
		}
		return true
	})
	v.Get("choices").ForEach(func(i, c gjson.Result) bool {
		prefix := "c:" + i.String() + ":"
		if c.Get("index").Exists() {
			prefix = "c:" + c.Get("index").String() + ":"
		}
		m := c.Get("message")
		o.content(prefix+"text", "text", m.Get("content").String(), true)
		for _, f := range []string{"reasoning_content", "reasoning"} {
			if x := m.Get(f); x.Type == gjson.String {
				o.content(prefix+"reasoning", "reasoning", x.String(), true)
				break
			}
		}
		m.Get("tool_calls").ForEach(func(j, t gjson.Result) bool { o.toolPart(prefix+"tool:"+j.String(), t, "chat", true); return true })
		if r := c.Get("finish_reason").String(); r == "length" || r == "content_filter" {
			for _, p := range o.structured.parts {
				if strings.HasPrefix(p.key, prefix) && p.kind == "tool" {
					p.done = false
				}
			}
		}
		return true
	})
	v.Get("output").ForEach(func(i, item gjson.Result) bool { o.responseItem("r:"+i.String(), item, true); return true })
}
func (o *observation) renderedOutput() (string, bool) {
	if !o.capture {
		return "", false
	}
	special := false
	for _, p := range o.structured.parts {
		if p.kind != "text" {
			special = true
			break
		}
	}
	if !special {
		var b strings.Builder
		for _, p := range o.structured.parts {
			if p.kind == "text" {
				b.WriteString(p.value.String())
			}
		}
		if b.Len() == 0 {
			text := redactText(o.output.text())
			return clip(text, o.cfg.MaxBytes), o.output.truncated || o.structured.truncated || len(text) > o.cfg.MaxBytes
		}
		text := redactText(b.String())
		return clip(text, o.cfg.MaxBytes), o.structured.truncated || len(text) > o.cfg.MaxBytes
	}
	messages := []any{}
	incomplete := o.failed || !o.terminal || o.readError && !o.canceledAfterCompleted() || o.droppedEvents > 0 || o.jsonBody.truncated
	for _, p := range o.structured.parts {
		m := map[string]any{"role": "assistant"}
		switch p.kind {
		case "text":
			m["content"] = p.value.String()
		case "reasoning":
			m["content"] = ""
			m["reasoning_content"] = p.value.String()
		case "tool":
			args := p.value.String()
			var decoded any
			if decodeJSON([]byte(args), &decoded) == nil {
				b, _ := json.Marshal(sanitizeContent(decoded, 0, o.cfg.CaptureReasoning))
				args = string(b)
			}
			call := map[string]any{"id": p.id, "type": p.toolType, "status": "complete"}
			if p.toolType == "custom" {
				call["custom"] = map[string]any{"name": p.name, "input": args}
			} else {
				call["function"] = map[string]any{"name": p.name, "arguments": args}
			}
			if p.itemID != "" {
				call["item_id"] = p.itemID
			}
			if incomplete || !p.done || p.truncated || p.id == "" || p.name == "" {
				call["status"] = "incomplete"
			}
			if p.truncated {
				call["truncated"] = true
			}
			m["tool_calls"] = []any{call}
		}
		messages = append(messages, sanitizeContent(m, 0, o.cfg.CaptureReasoning))
	}
	truncated := o.structured.truncated
	// Marshal each message once: dropping a long tail must not repeatedly encode
	// a large prefix on the response path. Reserve the truncation envelope up front.
	kept := []json.RawMessage{}
	remaining := o.cfg.MaxBytes - len(`{"messages":[],"truncated":true}`)
	for _, m := range messages {
		b, _ := json.Marshal(m)
		cost := len(b)
		if len(kept) > 0 {
			cost++
		}
		if cost > remaining {
			truncated = true
			break
		}
		kept = append(kept, b)
		remaining -= cost
	}
	doc := map[string]any{"messages": kept}
	if truncated {
		doc["truncated"] = true
	}
	b, _ := json.Marshal(doc)
	return string(b), truncated
}
