package llmtelemetry

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
)

func TestFieldMappingConfiguration(t *testing.T) {
	for _, raw := range []string{
		`{"future.safe.field":"arbitrary.destination.path"}`,
		`{"sub2api.user_name":"langfuse.user.id","sub2api.api_key_name":"langfuse.trace.tags"}`,
		`{"sub2api.user_email":"langfuse.session.id"}`,
		`{"sub2api.account_id":"langfuse.observation.metadata.account"}`,
	} {
		_, err := parseFieldMapping(raw)
		require.NoError(t, err)
	}
	for _, raw := range []string{"", " \t\n", "{}"} {
		mapping, err := parseFieldMapping(raw)
		require.NoError(t, err)
		require.Empty(t, mapping)
	}
	for _, raw := range []string{
		`{"":"target"}`, `{"source":"bad target"}`, `{"source":"a..b"}`,
		`{"source":"a.__proto__.b"}`,
		`{"a":"user.id","b":"langfuse.user.id"}`,
		`{"a":"langfuse.team","b":"langfuse.observation.metadata.team"}`,
		"null", "[]", "{", `{"sub2api.api_key_name":1}`,
		`{"sub2api.api_key_name":"langfuse.attributes"}`,
		`{"sub2api.api_key_name":"langfuse.same","sub2api.user_name":"langfuse.same"}`,
		`{"secret-do-not-log.__proto__":"langfuse.key_name"}`,
		strings.Repeat("x", 4097),
	} {
		_, err := parseFieldMapping(raw)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret-do-not-log")
	}
	t.Setenv("SUB2API_TELEMETRY_ENABLED", "true")
	t.Setenv("SUB2API_TELEMETRY_BACKEND", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/v1/traces")
	previous, existed := os.LookupEnv("SUB2API_TELEMETRY_FIELD_MAPPING")
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("SUB2API_TELEMETRY_FIELD_MAPPING", previous)
		} else {
			_ = os.Unsetenv("SUB2API_TELEMETRY_FIELD_MAPPING")
		}
	})
	require.NoError(t, os.Unsetenv("SUB2API_TELEMETRY_FIELD_MAPPING"))
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Empty(t, cfg.FieldMapping)
	for _, raw := range []string{"", " \t\n", "{}"} {
		t.Setenv("SUB2API_TELEMETRY_FIELD_MAPPING", raw)
		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.Empty(t, cfg.FieldMapping)
	}
	tags, err := parseFieldMapping(`{"sub2api.group_name":"langfuse.trace.tags"}`)
	require.NoError(t, err)
	require.Equal(t, "langfuse.trace.tags", tags["sub2api.group_name"])
	native, err := parseFieldMapping(`{"sub2api.api_key_name":"langfuse.user.id"}`)
	require.NoError(t, err)
	require.Equal(t, "langfuse.user.id", native["sub2api.api_key_name"])
	t.Setenv("SUB2API_TELEMETRY_FIELD_MAPPING", `{"sub2api.api_key_name":"langfuse.employee_key"}`)
	cfg, err = LoadConfig()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"sub2api.api_key_name": "langfuse.employee_key"}, cfg.FieldMapping)
}

func TestNamedIdentityDefaultCustomAndDisabledMapping(t *testing.T) {
	id := Identity{UserID: 7, UserEmail: "owner@example.com", UserName: "member", GroupID: 3, GroupName: "codex-pool", APIKeyID: 100, APIKeyName: "employee-key"}
	defaults := identityAttributes(id)
	for source, value := range map[string]string{"api_key_name": "employee-key", "group_name": "codex-pool", "user_name": "member"} {
		require.Equal(t, value, attr(defaults, "sub2api."+source))
		require.Nil(t, attr(defaults, "langfuse.observation.metadata."+source))
		require.Nil(t, attr(defaults, "langfuse.trace.metadata."+source))
	}
	custom := identityAttributes(id, map[string]string{"sub2api.api_key_name": "langfuse.key_name", "sub2api.group_name": "langfuse.team"})
	require.Equal(t, "employee-key", attr(custom, "langfuse.observation.metadata.key_name"))
	require.Equal(t, "codex-pool", attr(custom, "langfuse.observation.metadata.team"))
	require.Nil(t, attr(custom, "langfuse.observation.metadata.api_key_name"))
	require.Nil(t, attr(custom, "langfuse.observation.metadata.user_name"))
	require.Equal(t, "member", attr(custom, "sub2api.user_name"))
	disabled := identityAttributes(id, map[string]string{})
	for alias := range map[string]bool{"api_key_name": true, "group_name": true, "user_name": true} {
		require.Nil(t, attr(disabled, "langfuse.observation.metadata."+alias))
		require.Nil(t, attr(disabled, "langfuse.trace.metadata."+alias))
	}
	for _, attrs := range [][]attribute.KeyValue{defaults, identityAttributes(id, nil), custom, disabled} {
		require.Equal(t, "owner@example.com", attr(attrs, "user.id"))
		require.Equal(t, "owner@example.com", attr(attrs, "langfuse.user.id"))
		require.Equal(t, "employee-key", attr(attrs, "sub2api.api_key_name"))
		require.Equal(t, "codex-pool", attr(attrs, "sub2api.group_name"))
		require.Equal(t, "member", attr(attrs, "sub2api.user_name"))

	}
	for _, attrs := range [][]attribute.KeyValue{defaults, identityAttributes(id, nil), disabled} {
		for _, a := range attrs {
			require.False(t, strings.HasPrefix(string(a.Key), "langfuse.observation.metadata."))
			require.False(t, strings.HasPrefix(string(a.Key), "langfuse.trace.metadata."))
		}
	}
	missing := identityAttributes(Identity{UserID: 7, UserName: "", GroupName: "untrusted", APIKeyName: "untrusted"})
	require.Nil(t, attr(missing, "sub2api.user_name"))
	require.Nil(t, attr(missing, "sub2api.group_name"))
	require.Nil(t, attr(missing, "sub2api.api_key_name"))
}

func TestNativeKeyNameUserID(t *testing.T) {
	mapping := map[string]string{"sub2api.api_key_name": "langfuse.user.id"}
	for _, tc := range []struct {
		name, keyName, email, wantUser, wantSource string
		userID, keyID                              int64
	}{
		{"email key", "employee@example.com", "owner@example.com", "employee@example.com", "api_key_name", 7, 100},
		{"ordinary key", "employee-key", "owner@example.com", "employee-key", "api_key_name", 7, 100},
		{"trim", " employee@example.com ", " owner@example.com ", "employee@example.com", "api_key_name", 7, 100},
		{"empty key", "", "owner@example.com", "owner@example.com", "user_email", 7, 100},
		{"unsafe key", "sk-fixture-secret-12345", "owner@example.com", "owner@example.com", "user_email", 7, 100},
		{"control", "employee\nkey", "owner@example.com", "owner@example.com", "user_email", 7, 100},
		{"oversized", strings.Repeat("x", 257), "", "7", "user_id", 7, 100},
		{"missing key ID", "employee@example.com", "owner@example.com", "owner@example.com", "user_email", 7, 0},
		{"missing email", "employee-key", "", "employee-key", "api_key_name", 7, 100},
		{"empty fallback", "", "", "7", "user_id", 7, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := identityAttributes(Identity{UserID: tc.userID, APIKeyID: tc.keyID, APIKeyName: tc.keyName, UserEmail: tc.email}, mapping)
			require.Equal(t, tc.wantUser, attr(attrs, "user.id"))
			require.Equal(t, tc.wantUser, attr(attrs, "langfuse.user.id"))
			require.Equal(t, tc.wantSource, attr(attrs, "sub2api.user_identity_source"))
			if tc.email != "" {
				require.Equal(t, strings.TrimSpace(tc.email), attr(attrs, "sub2api.user_email"))
			}
			require.EqualValues(t, 7, attr(attrs, "sub2api.user_id"))
			for _, a := range attrs {
				require.False(t, strings.HasPrefix(string(a.Key), "langfuse.observation.metadata."))
				require.False(t, strings.HasPrefix(string(a.Key), "langfuse.trace.metadata."))
			}
		})
	}
	attrs := identityAttributes(Identity{APIKeyID: 100, APIKeyName: "spoof", UserEmail: "owner@example.com"}, mapping)
	require.Nil(t, attr(attrs, "user.id"))
	require.Nil(t, attr(attrs, "sub2api.user_email"))
}

func TestNativeGroupNameTags(t *testing.T) {
	mapping := map[string]string{"sub2api.group_name": "langfuse.trace.tags"}
	for _, tc := range []struct {
		name, group string
		groupID     int64
		want        []string
	}{
		{"valid", "codex-pool", 3, []string{"codex-pool"}},
		{"trim", " codex-pool ", 3, []string{"codex-pool"}},
		{"literal", "team,a", 3, []string{"team,a"}},
		{"unicode", "研发", 3, []string{"研发"}},
		{"empty", "", 3, nil},
		{"missing ID", "codex-pool", 0, nil},
		{"control", "team\nname", 3, nil},
		{"credential", "sk-fixture-secret-12345", 3, nil},
		{"oversized", strings.Repeat("x", 257), 3, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := Identity{UserID: 7, UserEmail: "owner@example.com", GroupID: tc.groupID, GroupName: tc.group}
			attrs := identityAttributes(id, mapping)
			if tc.want == nil {
				require.Nil(t, attr(attrs, "langfuse.trace.tags"))
			} else {
				require.Equal(t, tc.want, attr(attrs, "langfuse.trace.tags"))
				require.Equal(t, tc.want[0], attr(attrs, "sub2api.group_name"))
			}
			require.Equal(t, "owner@example.com", attr(attrs, "user.id"))
			require.EqualValues(t, tc.groupID, attr(attrs, "sub2api.group_id"))
			require.Nil(t, attr(attrs, "langfuse.observation.metadata.trace.tags"))
			require.Nil(t, attr(attrs, "langfuse.trace.metadata.trace.tags"))
			require.Nil(t, attr(identityAttributes(id), "langfuse.trace.tags"))
		})
	}
	require.Nil(t, attr(identityAttributes(Identity{GroupID: 3, GroupName: "untrusted"}, mapping), "langfuse.trace.tags"))
}
