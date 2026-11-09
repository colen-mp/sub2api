package llmtelemetry

import (
	"bytes"
	"encoding/json"
)

// jsonProjection splits a JSON object at top-level values and array items.
// It never exports an unparsed prefix. Oversized items are replaced, while
// subsequent messages and trailing usage remain observable within fixed memory.
type jsonProjection struct {
	limit                                              int
	reasoning                                          bool
	token                                              boundedBuffer
	doc                                                map[string]any
	keys                                               map[string]bool
	key                                                string
	state                                              byte
	depth                                              int
	quoted, escaped, started, done, invalid, truncated bool
	array                                              bool
	items                                              []json.RawMessage
	itemBytes                                          int
}

func newJSONProjection(limit int, reasoning ...bool) *jsonProjection {
	allowed := len(reasoning) > 0 && reasoning[0]
	return &jsonProjection{reasoning: allowed, limit: limit, token: boundedBuffer{limit: limit}, doc: map[string]any{}, state: '{'}
}
func (p *jsonProjection) reset() {
	p.token.Reset()
	p.token.truncated = false
	p.started = false
	p.depth = 0
	p.quoted = false
	p.escaped = false
}
func jsonSpace(b byte) bool { return b == ' ' || b == '\n' || b == '\r' || b == '\t' }
func (p *jsonProjection) value() any {
	if p.token.truncated {
		p.truncated = true
		return "[TRUNCATED: observation limit]"
	}
	var v any
	if decodeJSON(p.token.Bytes(), &v) != nil {
		p.invalid = true
		return nil
	}
	return v
}
func (p *jsonProjection) item() {
	if !p.keepKey() {
		p.reset()
		return
	}
	v := p.value()
	b, _ := json.Marshal(sanitizeContent(v, 0, p.reasoning))
	if len(b) > p.limit {
		b = []byte(`"[TRUNCATED: observation limit]"`)
		p.truncated = true
	}
	for len(p.items) > 0 && p.itemBytes+len(b) > p.limit {
		p.itemBytes -= len(p.items[0])
		p.items = p.items[1:]
		p.truncated = true
	}
	p.items = append(p.items, b)
	p.itemBytes += len(b)
	p.reset()
}
func (p *jsonProjection) keepKey() bool {
	if p.keys != nil {
		return p.keys[p.key]
	}
	switch p.key {
	case "model", "input", "system", "messages", "instructions", "prompt", "tools", "content", "choices", "output", "usage", "error", "status":
		return true
	}
	return false
}
func (p *jsonProjection) endValue() {
	if p.keepKey() {
		p.doc[p.key] = sanitizeContent(p.value(), 0, p.reasoning)
	}
	p.reset()
	p.state = ','
}
func (p *jsonProjection) feed(data []byte) {
	for _, b := range data {
		if p.invalid {
			return
		}
		if p.done {
			if !jsonSpace(b) {
				p.invalid = true
			}
			continue
		}
		switch p.state {
		case '{':
			if jsonSpace(b) {
				continue
			}
			if b != '{' {
				p.invalid = true
				continue
			}
			p.state = 'k'
		case 'k':
			if !p.started && jsonSpace(b) {
				continue
			}
			if !p.started && b == '}' {
				p.done = true
				continue
			}
			if !p.started {
				if b != '"' {
					p.invalid = true
					continue
				}
				p.started = true
				p.quoted = true
				p.token.add([]byte{b})
				continue
			}
			p.token.add([]byte{b})
			if p.escaped {
				p.escaped = false
			} else if b == '\\' {
				p.escaped = true
			} else if b == '"' {
				if p.token.truncated || json.Unmarshal(p.token.Bytes(), &p.key) != nil {
					p.invalid = true
					continue
				}
				p.reset()
				p.state = ':'
			}
		case ':':
			if jsonSpace(b) {
				continue
			}
			if b != ':' {
				p.invalid = true
				continue
			}
			p.state = 'v'
		case ',':
			if jsonSpace(b) {
				continue
			}
			if b == '}' {
				p.done = true
			} else if b == ',' {
				p.state = 'k'
			} else {
				p.invalid = true
			}
		case 'v':
			if !p.started && jsonSpace(b) {
				continue
			}
			if !p.started && !p.array && b == '[' {
				p.array = true
				continue
			}
			if p.array && !p.started && b == ']' {
				if p.keepKey() {
					p.doc[p.key] = p.items
				}
				p.items = nil
				p.itemBytes = 0
				p.array = false
				p.state = ','
				continue
			}
			if !p.quoted && p.depth == 0 && (b == ',' || b == '}' || (p.array && b == ']')) {
				if !p.started {
					p.invalid = true
					continue
				}
				if p.array {
					p.item()
					if b == ']' {
						if p.keepKey() {
							p.doc[p.key] = p.items
						}
						p.items = nil
						p.itemBytes = 0
						p.array = false
						p.state = ','
					} else if b != ',' {
						p.invalid = true
					}
				} else {
					p.endValue()
					if b == '}' {
						p.done = true
					} else {
						p.state = 'k'
					}
				}
				continue
			}
			p.started = true
			if p.keepKey() {
				p.token.add([]byte{b})
			}
			if p.quoted {
				if p.escaped {
					p.escaped = false
				} else if b == '\\' {
					p.escaped = true
				} else if b == '"' {
					p.quoted = false
				}
			} else {
				switch b {
				case '"':
					p.quoted = true
				case '{', '[':
					p.depth++
				case '}', ']':
					p.depth--
					if p.depth < 0 {
						p.invalid = true
					}
				}
			}
		}
	}
}
func (p *jsonProjection) bytes() []byte {
	if !p.done || p.invalid {
		return nil
	}
	b, _ := json.Marshal(p.doc)
	// The selected field count is fixed; array storage and each value are bounded.
	return bytes.TrimSpace(b)
}
