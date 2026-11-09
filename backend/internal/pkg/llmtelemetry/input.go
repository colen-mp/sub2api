package llmtelemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

// Keep numbers such as tool identifiers exact rather than rounding via float64.
func decodeJSON(body []byte, dst any) error {
	if !json.Valid(body) {
		return errors.New("invalid observation JSON")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	return d.Decode(dst)
}

func jsonText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func inputMessages(doc map[string]any) []any {
	messages := []any{}
	for _, key := range []string{"system", "instructions"} {
		if v, ok := doc[key]; ok {
			messages = append(messages, map[string]any{"role": "system", "content": readableContent(v)})
		}
	}
	var items any
	for _, key := range []string{"messages", "input", "prompt"} {
		if v, ok := doc[key]; ok {
			items = v
			break
		}
	}
	if s, ok := items.(string); ok {
		return append(messages, map[string]any{"role": "user", "content": s})
	}
	rows, _ := items.([]any)
	for _, item := range rows {
		m, ok := item.(map[string]any)
		if !ok {
			// Redacted reasoning remains a typed assistant placeholder so it
			// cannot invalidate Langfuse's conversation format detection.
			messages = append(messages, map[string]any{"role": "assistant", "content": jsonText(item)})
			continue
		}
		kind, _ := m["type"].(string)
		switch kind {
		case "reasoning", "thinking":
			value := m["summary"]
			if kind == "thinking" {
				value = m["thinking"]
			} else if value == nil {
				value = m["content"]
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": "", "reasoning_content": value})
		case "function_call", "custom_tool_call":
			args := m["arguments"]
			if kind == "custom_tool_call" {
				args = m["input"]
			}
			messages = append(messages, toolMessage(m["call_id"], m["name"], jsonText(args)))
		case "function_call_output", "custom_tool_call_output":
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": m["call_id"], "content": readableContent(m["output"])})
		default:
			if _, hasRole := m["role"]; !hasRole {
				// Retain unfamiliar Responses items as JSON passthrough. The
				// enclosing messages key keeps supported conversation items usable.
				messages = append(messages, m)
				continue
			}
			blocks, isBlocks := m["content"].([]any)
			if !isBlocks || !hasToolBlocks(blocks) {
				if content, exists := m["content"]; exists {
					m["content"] = readableContent(content)
				}
				messages = append(messages, m)
				continue
			}
			// Anthropic interleaves text, tool_use and tool_result blocks.
			// Emit in block order, retaining the actual tool IDs.
			for _, block := range blocks {
				b, ok := block.(map[string]any)
				if !ok {
					messages = append(messages, map[string]any{"role": m["role"], "content": block})
					continue
				}
				switch b["type"] {
				case "tool_use":
					messages = append(messages, toolMessage(b["id"], b["name"], jsonText(b["input"])))
				case "tool_result":
					result := map[string]any{"role": "tool", "tool_call_id": b["tool_use_id"], "content": readableContent(b["content"])}
					if v, ok := b["is_error"]; ok {
						result["is_error"] = v
					}
					messages = append(messages, result)
				default:
					messages = append(messages, map[string]any{"role": m["role"], "content": readableContent([]any{b})})
				}
			}
		}
	}
	return messages
}

func toolMessage(id, name any, args string) map[string]any {
	return map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
		"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args},
	}}}
}

func hasToolBlocks(blocks []any) bool {
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && (m["type"] == "tool_use" || m["type"] == "tool_result") {
			return true
		}
	}
	return false
}

func readableContent(v any) any {
	blocks, ok := v.([]any)
	if !ok {
		return v
	}
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && (m["type"] == "input_text" || m["type"] == "output_text") {
			m["type"] = "text"
		}
	}
	return blocks
}

const inputLimitMarker = "[TRUNCATED: observation limit]"

func langfuseInput(projected map[string]any, limit int) (string, bool) {
	messages := inputMessages(projected)
	doc := map[string]any{"messages": messages}
	if tools, ok := projected["tools"]; ok {
		doc["tools"] = tools
	}
	encoded, _ := json.Marshal(doc)
	if len(encoded) <= limit {
		return string(encoded), false
	}
	// Never slice a JSON string. Drop complete old items, keep recent content,
	// and use a valid message placeholder if even one item exceeds the budget.
	marker, _ := json.Marshal(map[string]any{"role": "system", "content": inputLimitMarker})
	remaining := limit - len(`{"messages":[]}`) - len(marker)
	kept := []json.RawMessage{}
	for i := len(messages) - 1; i >= 0; i-- {
		b, _ := json.Marshal(messages[i])
		if len(b)+1 > remaining {
			text := jsonText(messages[i])
			role := "assistant"
			if m, ok := messages[i].(map[string]any); ok {
				if r, ok := m["role"].(string); ok {
					role = r
				}
				if content, ok := m["content"]; ok {
					text = jsonText(content)
				}
			}
			lo, hi := 0, min(len(text), max(remaining, 0))
			var fitted []byte
			for lo <= hi {
				n := (lo + hi) / 2
				tail := text[len(text)-n:]
				for len(tail) > 0 && !utf8.ValidString(tail) {
					tail = tail[1:]
				}
				candidate, _ := json.Marshal(map[string]any{"role": role, "content": inputLimitMarker + tail})
				if len(candidate)+1 <= remaining {
					fitted = candidate
					lo = n + 1
				} else {
					hi = n - 1
				}
			}
			if fitted != nil {
				kept = append(kept, fitted)
			}
			break
		}
		kept = append(kept, b)
		remaining -= len(b) + 1
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	kept = append([]json.RawMessage{marker}, kept...)
	encoded, _ = json.Marshal(map[string]any{"messages": kept})
	return string(encoded), true
}

// Project only already-sanitized captured content; never read the request again.
func currentTurnAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := append([]attribute.KeyValue(nil), attrs...)
	for i, a := range out {
		if a.Key != "langfuse.observation.input" {
			continue
		}
		var doc map[string]any
		if decodeJSON([]byte(a.Value.AsString()), &doc) != nil {
			continue
		}
		rows, ok := doc["messages"].([]any)
		if !ok {
			continue
		}
		start := -1
		for j, row := range rows {
			if m, ok := row.(map[string]any); ok && m["role"] == "user" {
				start = j
			}
		}
		var messages []any
		if start >= 0 {
			messages = rows[start:]
		} else {
			messages = []any{map[string]any{"role": "system", "content": "[No user turn identified; see upstream input]"}}
		}
		body, _ := json.Marshal(map[string]any{"messages": messages})
		out[i] = attribute.String("langfuse.observation.input", string(body))
		out = append(out, attribute.String("sub2api.input.view", "current_turn"), attribute.Int("sub2api.input.history_messages_omitted", max(start, 0)))
	}
	return out
}
