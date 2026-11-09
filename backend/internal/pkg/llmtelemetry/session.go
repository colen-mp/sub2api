package llmtelemetry

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/attribute"
)

var sessionIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,255}$`)

type clientSession struct {
	mu          sync.Mutex
	raw, source string
	projection  *jsonProjection
	parseUserID func(string) string
}

func validSession(v string) string {
	v = strings.TrimSpace(v)
	if !sessionIdentifier.MatchString(v) {
		return ""
	}
	return v
}

// Inspect only the original ingress. Upstream account isolation/fingerprint
// rewriting must never decide which Langfuse conversations are grouped.
func observeClientSession(req *http.Request, parseUserID ...func(string) string) *clientSession {
	s := &clientSession{}
	if len(parseUserID) > 0 {
		s.parseUserID = parseUserID[0]
	}
	for _, key := range []string{"session-id", "session_id", "x-claude-code-session-id", "conversation-id", "conversation_id"} {
		if v := validSession(req.Header.Get(key)); v != "" {
			s.raw, s.source = v, key
			return s
		}
	}
	if meta := req.Header.Get("x-codex-turn-metadata"); len(meta) <= 4096 && gjson.Valid(meta) {
		if v := gjson.Get(meta, "session_id"); v.Type == gjson.String {
			if id := validSession(v.String()); id != "" {
				s.raw, s.source = id, "x-codex-turn-metadata"
				return s
			}
		}
	}
	if req.Body != nil && req.Body != http.NoBody {
		s.projection = newJSONProjection(4096)
		s.projection.keys = map[string]bool{"metadata": true, "client_metadata": true, "session_id": true, "sessionId": true}
		req.Body = &sessionBody{ReadCloser: req.Body, session: s}
	}
	return s
}

type sessionBody struct {
	io.ReadCloser
	session *clientSession
}

func (b *sessionBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.session.mu.Lock()
		b.session.projection.feed(p[:n])
		b.session.mu.Unlock()
	}
	return n, err
}

func (s *clientSession) identifier() (string, string) {
	if s == nil {
		return "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.raw != "" {
		return s.raw, s.source
	}
	if s.projection == nil {
		return "", ""
	}
	body := s.projection.bytes()
	if body == nil {
		return "", ""
	}
	for _, path := range []string{"session_id", "sessionId", "metadata.session_id", "metadata.sessionId", "metadata.conversation_id", "client_metadata.session_id", "client_metadata.sessionId"} {
		v := gjson.GetBytes(body, path)
		if v.Type == gjson.String {
			if id := validSession(v.String()); id != "" {
				return id, path
			}
		}
	}
	if s.parseUserID != nil {
		v := gjson.GetBytes(body, "metadata.user_id")
		if v.Type == gjson.String {
			if id := validSession(s.parseUserID(v.String())); id != "" {
				return id, "metadata.user_id"
			}
		}
	}
	return "", ""
}

func (s *requestState) sessionAttributes(id Identity) []attribute.KeyValue {
	if id.UserID <= 0 {
		return nil
	}
	raw, source := s.session.identifier()
	if raw == "" {
		return nil
	}
	if id.APIKeyID > 0 {
		// A shared console user can issue independent employee keys. Scope by
		// immutable key ID so renaming a label does not rewrite session identity.
		hash := sha256.Sum256([]byte(fmt.Sprintf("%d\x00key:%d\x00%s", id.UserID, id.APIKeyID, raw)))
		return []attribute.KeyValue{
			attribute.String("langfuse.session.id", fmt.Sprintf("sub2api:%d:key:%d:%x", id.UserID, id.APIKeyID, hash)),
			attribute.String("sub2api.session.source", source),
		}
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", id.UserID, raw)))
	return []attribute.KeyValue{
		attribute.String("langfuse.session.id", fmt.Sprintf("sub2api:%d:%x", id.UserID, hash)),
		attribute.String("sub2api.session.source", source),
	}
}
