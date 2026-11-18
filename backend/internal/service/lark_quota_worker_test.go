package service

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type larkTestAccounts struct {
	AccountRepository
	get   func(context.Context, int64) (*Account, error)
	list  func(pagination.PaginationParams, string) ([]Account, *pagination.PaginationResult, error)
	extra map[string]any
}

func (r *larkTestAccounts) GetByID(ctx context.Context, id int64) (*Account, error) {
	return r.get(ctx, id)
}
func (r *larkTestAccounts) ListWithFilters(_ context.Context, p pagination.PaginationParams, platform, _, _, _ string, _ int64, _ string) ([]Account, *pagination.PaginationResult, error) {
	return r.list(p, platform)
}
func (r *larkTestAccounts) UpdateExtra(_ context.Context, _ int64, values map[string]any) error {
	for key, value := range values {
		r.extra[key] = value
	}
	return nil
}

type larkTestUsage func(context.Context, *Account, bool) (subscriptionAlertSamples, error)

func (f larkTestUsage) SubscriptionAlertUsage(ctx context.Context, a *Account, refresh bool) (subscriptionAlertSamples, error) {
	return f(ctx, a, refresh)
}

func TestLarkDisabledSkipsAllAccountQueries(t *testing.T) {
	s := newLarkTestService(t) // nil repositories would panic if touched
	require.NoError(t, s.evaluateAccount(context.Background(), 1, true))
	s.scan()
	require.False(t, s.Notify(1))
	saveLarkTestSettings(t, s)
	_, err := s.UpdateSettings(context.Background(), LarkQuotaSettings{Thresholds: []float64{95, 100}, Windows: []string{"5h"}})
	require.NoError(t, err)
	require.NoError(t, s.evaluateAccount(context.Background(), 1, true))
	s.scan()
	require.False(t, s.Notify(1))
}

func TestLarkWorkerCoalescesAndCancels(t *testing.T) {
	s := newLarkTestService(t)
	saveLarkTestSettings(t, s)
	s.accounts = &larkTestAccounts{get: func(context.Context, int64) (*Account, error) {
		return &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive}, nil
	}}
	entered := make(chan struct{}, 2)
	var queries atomic.Int32
	s.usage = larkTestUsage(func(ctx context.Context, _ *Account, refresh bool) (subscriptionAlertSamples, error) {
		require.False(t, refresh, "snapshot events must not initiate upstream polls")
		queries.Add(1)
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	require.True(t, s.Notify(1))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	require.True(t, s.Notify(1))
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker failed to cancel")
	}
	require.EqualValues(t, 1, queries.Load())
	require.False(t, s.Notify(1))
}

func TestLarkScanResumesAfterQueueSaturation(t *testing.T) {
	s := newLarkTestService(t)
	saveLarkTestSettings(t, s)
	s.accounts = &larkTestAccounts{list: func(p pagination.PaginationParams, platform string) ([]Account, *pagination.PaginationResult, error) {
		if platform != PlatformOpenAI {
			return nil, nil, nil
		}
		accounts := make([]Account, 100)
		for i := range accounts {
			accounts[i] = Account{ID: int64((p.Page-1)*100 + i + 1), Platform: platform, Type: AccountTypeOAuth, Status: StatusActive}
		}
		return accounts, &pagination.PaginationResult{Pages: 3}, nil
	}, get: func(context.Context, int64) (*Account, error) { return nil, nil }}
	// Occupy every worker/queue slot until cancellation.
	for s.worker.submit(func(ctx context.Context) { <-ctx.Done() }) {
	}
	s.scan()
	require.Equal(t, larkQuotaScanCursor{page: 1}, s.scanCursors[PlatformOpenAI])
	s.worker.stop()
	s.worker = newUsageNotificationWorker()
	s.scan()
	require.Equal(t, larkQuotaScanCursor{page: 2}, s.scanCursors[PlatformOpenAI])
}

// Return a stale completed record on the first read to reproduce an owner
// completing between another instance's state read and lease acquisition.
type larkStaleReadLeases struct {
	*larkTestLeases
	stale *IdempotencyRecord
}

func (r *larkStaleReadLeases) GetByScopeAndKeyHash(ctx context.Context, scope, key string) (*IdempotencyRecord, error) {
	if r.stale != nil {
		stale := r.stale
		r.stale = nil
		return stale, nil
	}
	return r.larkTestLeases.GetByScopeAndKeyHash(ctx, scope, key)
}

func TestLarkLeaseRereadsStateAfterAcquisition(t *testing.T) {
	s := newLarkTestService(t)
	saveLarkTestSettings(t, s)
	count := 0
	s.client.Transport = larkTestTransport(func(*http.Request) (*http.Response, error) { count++; return larkTestResponse(200, `{"code":0}`), nil })
	now, reset := time.Now(), time.Now().Add(time.Hour)
	a, usage := &Account{ID: 17}, &UsageInfo{UpdatedAt: &now}
	progress := &UsageProgress{Utilization: 95, ResetsAt: &reset}
	require.NoError(t, s.evaluateWindow(context.Background(), a, "5h", usage, progress))
	store := s.leases.(*larkTestLeases)
	var stale *IdempotencyRecord
	for _, record := range store.records {
		if record.ResponseBody != nil && *record.ResponseBody != "{}" {
			copy := *record
			stale = &copy
		}
	}
	require.NotNil(t, stale)
	progress.Utilization = 100
	require.NoError(t, s.evaluateWindow(context.Background(), a, "5h", usage, progress))
	s.leases = &larkStaleReadLeases{larkTestLeases: store, stale: stale}
	require.NoError(t, s.evaluateWindow(context.Background(), a, "5h", usage, progress))
	require.Equal(t, 2, count)
}

func TestLarkAnthropicRequiresRealPairedSamples(t *testing.T) {
	now, reset := time.Now(), time.Now().Add(time.Hour)
	a := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Status: StatusActive, SessionWindowEnd: &reset, SessionWindowStatus: "rejected", Extra: map[string]any{"passive_usage_sampled_at": now.Format(time.RFC3339)}}
	s := &AccountUsageService{}
	samples, err := s.SubscriptionAlertUsage(context.Background(), a, true)
	require.NoError(t, err)
	require.Empty(t, samples, "rejected status must never be estimated as 100 percent")
	rates := &RateLimitService{accountRepo: &larkTestAccounts{extra: a.Extra}}
	headers := http.Header{}
	for _, window := range []string{"5h", "7d"} {
		headers.Set("anthropic-ratelimit-unified-"+window+"-utilization", "1.0")
		headers.Set("anthropic-ratelimit-unified-"+window+"-reset", strconv.FormatInt(reset.Unix(), 10))
	}
	rates.samplePassiveUsageFromHeaders(context.Background(), a, headers)
	samples, err = s.SubscriptionAlertUsage(context.Background(), a, false)
	require.NoError(t, err)
	require.Len(t, samples, 2)
	require.Equal(t, 100.0, samples["5h"].progress.Utilization)
	headers = http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.96") // Missing reset invalidates just this pair.
	rates.samplePassiveUsageFromHeaders(context.Background(), a, headers)
	samples, err = s.SubscriptionAlertUsage(context.Background(), a, false)
	require.NoError(t, err)
	require.NotContains(t, samples, "5h")
	require.Contains(t, samples, "7d")
}

func TestLarkOpenAIPartialAndStaleWindowsSkipped(t *testing.T) {
	now := time.Now().UTC()
	used, reset, minutes := 100.0, 3600, 300
	updates := buildCodexUsageExtraUpdates(&OpenAICodexUsageSnapshot{PrimaryUsedPercent: &used, PrimaryResetAfterSeconds: &reset, PrimaryWindowMinutes: &minutes}, now)
	a := &Account{Platform: PlatformOpenAI, Extra: updates}
	require.Contains(t, subscriptionSamplesFromExtra(a, now), "5h")
	partial := buildCodexUsageExtraUpdates(&OpenAICodexUsageSnapshot{PrimaryUsedPercent: &used, PrimaryWindowMinutes: &minutes}, now)
	for k, v := range partial {
		updates[k] = v
	}
	require.Empty(t, subscriptionSamplesFromExtra(a, now))
	updates["codex_5h_sampled_at"] = now.Add(-6 * time.Minute).Format(time.RFC3339)
	require.Empty(t, subscriptionSamplesFromExtra(a, now))
}

func TestLarkScannerRefreshesMissingWindowFromCache(t *testing.T) {
	now, reset := time.Now().UTC(), time.Now().Add(time.Hour).UTC()
	a := &Account{ID: 33, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive, Extra: map[string]any{
		"lark_quota_5h_sampled_at": now.Format(time.RFC3339), "lark_quota_5h_utilization": 0.96, "lark_quota_5h_reset": reset.Unix(),
	}}
	s := &AccountUsageService{cache: NewUsageCache()}
	response := &ClaudeUsageResponse{}
	response.FiveHour.Utilization, response.FiveHour.ResetsAt = 96, reset.Format(time.RFC3339)
	response.SevenDay.Utilization, response.SevenDay.ResetsAt = 100, reset.Add(24*time.Hour).Format(time.RFC3339)
	s.cache.apiCache.Store(a.ID, &apiUsageCache{response: response, timestamp: now})
	samples, err := s.SubscriptionAlertUsage(context.Background(), a, false)
	require.NoError(t, err)
	require.Len(t, samples, 1, "a snapshot event must not refresh a missing window")
	samples, err = s.SubscriptionAlertUsage(context.Background(), a, true)
	require.NoError(t, err)
	require.Len(t, samples, 2, "background scans must fill a missing window even if another window is fresh")
	require.Equal(t, 100.0, samples["7d"].progress.Utilization)
	require.Equal(t, now, samples["7d"].sampledAt)
}
