package llmtelemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestAuthenticatedUserAndKeyNameMetadata(t *testing.T) {
	for _, tc := range []struct{ name, keyName, email, wantUser, wantKeyName, source string }{
		{"key email", "employee@example.com", "admin@example.com", "admin@example.com", "employee@example.com", "user_email"},
		{"trim", " employee+codex@example.com ", " admin@example.com ", "admin@example.com", "employee+codex@example.com", "user_email"},
		{"ordinary name", "employee-codex", "admin@example.com", "admin@example.com", "employee-codex", "user_email"},
		{"missing", "", "", "7", "", "user_id"},
		{"no user email", "employee@example.com", "", "7", "employee@example.com", "user_id"},
		{"numeric name", "123", "admin@example.com", "admin@example.com", "123", "user_email"},
		{"JSON name", `{"team":"qa"}`, "admin@example.com", "admin@example.com", `{"team":"qa"}`, "user_email"},
		{"Chinese name", "测试同事", "admin@example.com", "admin@example.com", "测试同事", "user_email"},
		{"control", "employee@exa\nmple.com", "", "7", "", "user_id"},
		{"credential", "sk-fixture-secret-12345", "", "7", "", "user_id"},
		{"too long", strings.Repeat("x", 257), "", "7", "", "user_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := identityAttributes(Identity{UserID: 7, APIKeyID: 100, APIKeyName: tc.keyName, UserEmail: tc.email}, map[string]string{"sub2api.api_key_name": "langfuse.api_key_name"})
			require.Equal(t, tc.wantUser, attr(attrs, "user.id"))
			require.Equal(t, tc.wantUser, attr(attrs, "langfuse.user.id"))
			require.Equal(t, tc.source, attr(attrs, "sub2api.user_identity_source"))
			require.EqualValues(t, 7, attr(attrs, "sub2api.user_id"))
			require.EqualValues(t, 100, attr(attrs, "sub2api.api_key_id"))
			if tc.wantKeyName == "" {
				require.Nil(t, attr(attrs, "sub2api.api_key_name"))
			} else {
				require.Equal(t, tc.wantKeyName, attr(attrs, "sub2api.api_key_name"))
			}
			for _, key := range []string{"langfuse.observation.metadata.api_key_name", "langfuse.trace.metadata.api_key_name"} {
				if tc.wantKeyName == "" {
					require.Nil(t, attr(attrs, key))
				} else {
					name := attr(attrs, key).(string)
					require.Equal(t, tc.wantKeyName, name)
				}
			}
		})
	}
	require.Nil(t, attr(identityAttributes(Identity{APIKeyID: 100, APIKeyName: "spoof@example.com"}), "user.id"))
	require.Nil(t, attr(identityAttributes(Identity{APIKeyID: 100, APIKeyName: "spoof@example.com"}), "langfuse.observation.metadata.api_key_name"))
	require.Equal(t, "7", attr(identityAttributes(Identity{UserID: 7, APIKeyName: "spoof@example.com"}), "user.id"))
	require.Nil(t, attr(identityAttributes(Identity{UserID: 7, APIKeyName: "spoof@example.com"}), "langfuse.observation.metadata.api_key_name"))
}

func TestEmployeeKeySessionsAndAttempts(t *testing.T) {
	for mode, mapped := range map[string]bool{"unset": false, "custom": true, "native": true, "native_tags": true} {
		t.Run(mode, func(t *testing.T) {

			gin.SetMode(gin.TestMode)
			exp := tracetest.NewInMemoryExporter()
			cfg := testConfig()
			if mapped {
				cfg.FieldMapping = map[string]string{"sub2api.api_key_name": "langfuse.employee_key", "sub2api.group_name": "langfuse.team", "sub2api.user_name": "langfuse.member"}
			}
			if mode == "native" || mode == "native_tags" {
				cfg.FieldMapping["sub2api.api_key_name"] = "langfuse.user.id"
			}
			if mode == "native_tags" {
				cfg.FieldMapping["sub2api.group_name"] = "langfuse.trace.tags"
			}
			r := newRuntime(cfg, exp)
			old := current.Swap(r)
			defer current.Store(old)
			defer r.provider.Shutdown(context.Background())
			identity := Identity{UserID: 7, UserEmail: "admin@example.com", UserName: "member", GroupID: 1, GroupName: "codex-pool", APIKeyID: 100, APIKeyName: "employee-a@example.com"}
			u := Wrap(&mockUpstream{do: func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.JSONEq(t, `{"model":"m","input":"probe"}`, string(body))
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":2,"output_tokens":1},"choices":[{"message":{"content":"OK"}}]}`)), Header: http.Header{}}, nil
			}})
			engine := gin.New()
			engine.Use(RequestMiddleware(func(context.Context) Identity { return identity }))
			engine.POST("/v1/responses", func(c *gin.Context) {
				req, err := http.NewRequestWithContext(c.Request.Context(), "POST", "https://test.invalid/v1/responses", strings.NewReader(`{"model":"m","input":"probe"}`))
				require.NoError(t, err)
				resp, err := u.Do(req, "", 3, 1)
				require.NoError(t, err)
				_, err = io.Copy(c.Writer, resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			})
			for turn := 0; turn < 3; turn++ {
				if turn == 2 {
					identity.APIKeyID, identity.APIKeyName = 101, "employee-b@example.com"
				}
				req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"user":"spoof@example.com"}`))
				req.Header.Set("session-id", "same-client-session")
				req.Header.Set("X-User-Email", "spoof@example.com")
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code)
			}
			require.NoError(t, r.provider.ForceFlush(context.Background()))
			spans := exp.GetSpans()
			require.Len(t, spans, 6)
			sessions := map[string]string{}
			counts := map[string]int{}
			for _, span := range spans {
				name := attr(span.Attributes, "sub2api.api_key_name").(string)
				wantUser, wantSource := "admin@example.com", "user_email"
				if mode == "native" || mode == "native_tags" {
					wantUser, wantSource = name, "api_key_name"
				}
				require.Equal(t, wantUser, attr(span.Attributes, "user.id"))
				require.Equal(t, wantUser, attr(span.Attributes, "langfuse.user.id"))
				require.Equal(t, wantSource, attr(span.Attributes, "sub2api.user_identity_source"))
				require.Equal(t, "admin@example.com", attr(span.Attributes, "sub2api.user_email"))
				require.Equal(t, name, attr(span.Attributes, "sub2api.api_key_name"))
				require.Equal(t, "codex-pool", attr(span.Attributes, "sub2api.group_name"))
				require.Equal(t, "member", attr(span.Attributes, "sub2api.user_name"))
				if mapped {
					if mode == "custom" {
						require.Equal(t, name, attr(span.Attributes, "langfuse.observation.metadata.employee_key"))
					} else {
						require.Nil(t, attr(span.Attributes, "langfuse.observation.metadata.employee_key"))
						require.Nil(t, attr(span.Attributes, "langfuse.observation.metadata.user.id"))
					}
					if mode == "native_tags" {
						require.Equal(t, []string{"codex-pool"}, attr(span.Attributes, "langfuse.trace.tags"))
						require.Nil(t, attr(span.Attributes, "langfuse.observation.metadata.team"))
					} else {
						require.Equal(t, "codex-pool", attr(span.Attributes, "langfuse.observation.metadata.team"))
						require.Nil(t, attr(span.Attributes, "langfuse.trace.tags"))
					}
					require.Equal(t, "member", attr(span.Attributes, "langfuse.observation.metadata.member"))
				} else {
					for _, a := range span.Attributes {
						require.False(t, strings.HasPrefix(string(a.Key), "langfuse.observation.metadata."))
						require.False(t, strings.HasPrefix(string(a.Key), "langfuse.trace.metadata."))
					}
				}
				require.Nil(t, attr(span.Attributes, "langfuse.observation.metadata.api_key_name"))
				require.Contains(t, []string{"employee-a@example.com", "employee-b@example.com"}, name)
				session := attr(span.Attributes, "langfuse.session.id").(string)
				if old := sessions[name]; old != "" {
					require.Equal(t, old, session)
				}
				sessions[name] = session
				counts[name]++
				require.NotContains(t, session, "same-client-session")
				require.EqualValues(t, 7, attr(span.Attributes, "sub2api.user_id"))
			}
			require.Equal(t, 4, counts["employee-a@example.com"])
			require.Equal(t, 2, counts["employee-b@example.com"])
			require.NotEqual(t, sessions["employee-a@example.com"], sessions["employee-b@example.com"])
			state := &requestState{session: &clientSession{raw: "same-client-session", source: "session-id"}}
			a := Identity{UserID: 7, APIKeyID: 100, APIKeyName: "ordinary-a"}
			b := Identity{UserID: 7, APIKeyID: 101, APIKeyName: "ordinary-b"}
			require.NotEqual(t, state.sessionAttributes(a), state.sessionAttributes(b))
			b.APIKeyID = a.APIKeyID // Renaming a key does not split its existing session.
			require.Equal(t, state.sessionAttributes(a), state.sessionAttributes(b))

		})
	}
}
