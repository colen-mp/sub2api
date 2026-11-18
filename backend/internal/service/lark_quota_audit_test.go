package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLarkQuotaAuditSecretsRedacted(t *testing.T) {
	result := RedactAuditBody([]byte(`{"webhook":"https://open.larksuite.com/open-apis/bot/v2/hook/synthetic-token","signing_secret":"synthetic-signing-secret","thresholds":[95,100],"enabled":true}`), "application/json")
	require.NotContains(t, result, "synthetic-token")
	require.NotContains(t, result, "synthetic-signing-secret")
	require.Contains(t, result, "thresholds")
}
