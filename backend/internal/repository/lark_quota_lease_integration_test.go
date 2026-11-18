//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestLarkQuotaLeaseReclaimsCompletedState(t *testing.T) {
	repo := &idempotencyRepository{sql: testTx(t)}
	ctx := context.Background()
	now := time.Now().UTC()
	record := &service.IdempotencyRecord{Scope: uniqueTestValue(t, "lark-quota"), IdempotencyKeyHash: hashedTestValue(t, "lark-window"), RequestFingerprint: "v1", Status: service.IdempotencyStatusProcessing, LockedUntil: ptrTime(now.Add(45 * time.Second)), ExpiresAt: now.Add(8 * 24 * time.Hour)}
	created, err := repo.CreateProcessing(ctx, record)
	require.NoError(t, err)
	require.True(t, created)
	body := `{"highest_sent":95,"attempts":1}`
	require.NoError(t, repo.MarkSucceeded(ctx, record.ID, 200, body, record.ExpiresAt))
	claimed, err := repo.TryReclaim(ctx, record.ID, service.IdempotencyStatusSucceeded, now, now.Add(45*time.Second), record.ExpiresAt)
	require.NoError(t, err)
	require.True(t, claimed)
	got, err := repo.GetByScopeAndKeyHash(ctx, record.Scope, record.IdempotencyKeyHash)
	require.NoError(t, err)
	require.Equal(t, body, *got.ResponseBody)
	claimed, err = repo.TryReclaim(ctx, record.ID, service.IdempotencyStatusProcessing, now, now.Add(45*time.Second), record.ExpiresAt)
	require.NoError(t, err)
	require.False(t, claimed, "another instance cannot acquire a live window lease")
	require.NoError(t, repo.MarkSucceeded(ctx, record.ID, 200, `{"highest_sent":100}`, record.ExpiresAt))
	got, err = repo.GetByScopeAndKeyHash(ctx, record.Scope, record.IdempotencyKeyHash)
	require.NoError(t, err)
	require.Contains(t, *got.ResponseBody, `"highest_sent":100`)
}
