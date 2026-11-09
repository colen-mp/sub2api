package llmtelemetry

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthenticatedEmailIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, email, want string
		userID            int64
	}{
		{"email", "member@example.com", "member@example.com", 7},
		{"trim", " member@example.com ", "member@example.com", 7},
		{"missing email", "", "7", 7},
		{"blank email", "  ", "7", 7},
		{"unauthenticated", "spoof@example.com", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := identityAttributes(Identity{UserID: tc.userID, UserEmail: tc.email})
			require.Equal(t, tc.userID, attr(a, "sub2api.user_id"))
			if tc.want == "" {
				require.Nil(t, attr(a, "user.id"))
				require.Nil(t, attr(a, "langfuse.user.id"))
			} else {
				require.Equal(t, tc.want, attr(a, "user.id"))
				require.Equal(t, tc.want, attr(a, "langfuse.user.id"))
			}
		})
	}
}

// Compare complete exported JSON, including item order, roles and tool results;
// a matching prefix alone cannot establish that the latest prompt survived.
func TestInputFidelityAcrossProtocols(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"responses", `{"model":"m","instructions":"Answer in Chinese","input":[{"id":"at_first","type":"message","role":"user","content":[{"type":"input_text","text":"第一条\nquestion"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"city\":\"上海\"}"},{"type":"function_call_output","call_id":"call_1","output":"result"},{"role":"user","content":"最新问题"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
			`{"messages":[{"role":"system","content":"Answer in Chinese"},{"id":"at_first","type":"message","role":"user","content":[{"type":"text","text":"第一条\nquestion"}]},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"上海\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"},{"role":"user","content":"最新问题"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`},
		{"messages", `{"model":"m","system":[{"type":"text","text":"system"}],"messages":[{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"lookup","input":{"city":"上海"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"result"}]}]}`,
			`{"messages":[{"role":"system","content":[{"type":"text","text":"system"}]},{"role":"user","content":[{"type":"text","text":"question"}]},{"role":"assistant","tool_calls":[{"id":"tool_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"上海\"}"}}]},{"role":"tool","tool_call_id":"tool_1","content":"result"}]}`},
		{"chat completions", `{"model":"m","messages":[{"role":"system","content":"system"},{"role":"user","content":"question"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"password\":\"fixture-secret\",\"city\":\"上海\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"},{"role":"user","content":"latest"}]}`,
			`{"messages":[{"role":"system","content":"system"},{"role":"user","content":"question"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"上海\",\"password\":\"[REDACTED]\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"},{"role":"user","content":"latest"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &observedInput{inner: io.NopCloser(strings.NewReader(tc.body)), buf: boundedBuffer{limit: 32768}}
			var forwarded strings.Builder
			chunk := make([]byte, 7) // Deliberately split JSON and UTF-8 boundaries.
			_, err := io.CopyBuffer(&forwarded, b, chunk)
			require.NoError(t, err)
			require.Equal(t, tc.body, forwarded.String())
			a := b.attrs(true, 16384, "langfuse")
			require.Equal(t, false, attr(a, "sub2api.input.truncated"))
			require.JSONEq(t, tc.want, attr(a, "langfuse.observation.input").(string))
		})
	}
}
