package service

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type subscriptionAlertSample struct {
	progress  *UsageProgress
	sampledAt time.Time
}

type subscriptionAlertSamples map[string]subscriptionAlertSample

// Each window needs its own sampling time. A partial response must not make a
// different window's old utilization/reset pair look fresh.
func subscriptionSamplesFromExtra(account *Account, now time.Time) subscriptionAlertSamples {
	samples := make(subscriptionAlertSamples)
	for _, window := range []string{"5h", "7d"} {
		prefix := "lark_quota_" + window
		if account.Platform == PlatformOpenAI {
			prefix = "codex_" + window
		}
		raw, _ := account.Extra[prefix+"_sampled_at"].(string)
		updated, err := time.Parse(time.RFC3339, raw)
		if err != nil || !freshLarkQuotaUsage(&UsageInfo{UpdatedAt: &updated}, now) {
			continue
		}
		var progress *UsageProgress
		if account.Platform == PlatformOpenAI {
			// These stamps are written only with a real utilization/reset pair.
			progress = buildCodexUsageProgressFromExtra(account.Extra, window, now)
		} else {
			progress = buildPassiveUsageWindow(account.Extra, prefix+"_utilization", prefix+"_reset")
		}
		if progress != nil {
			samples[window] = subscriptionAlertSample{progress: progress, sampledAt: updated}
		}
	}
	return samples
}

// SubscriptionAlertUsage reads real quota data without interactive usage's
// synthetic /responses probe, estimated 429 utilization or reset-credit consume.
func (s *AccountUsageService) SubscriptionAlertUsage(ctx context.Context, account *Account, refresh bool) (subscriptionAlertSamples, error) {
	if !supportsLarkQuotaAccount(account) {
		return nil, nil
	}
	now := time.Now()
	samples := subscriptionSamplesFromExtra(account, now)
	if len(samples) == 2 || !refresh {
		return samples, nil
	}
	if account.Platform == PlatformOpenAI {
		if s.openAIQuotaService == nil {
			return samples, nil
		}
		quota, err := s.openAIQuotaService.QueryUsage(ctx, account.ID)
		if err != nil {
			return samples, nil // A failed refresh does not invalidate fresh passive windows.
		}
		if quota == nil || quota.FetchedAt <= 0 {
			return samples, nil
		}
		updated := time.Unix(quota.FetchedAt, 0)
		updates := buildOpenAIAutoResetUsageUpdates(quota, updated)
		// Build exclusively from this response; never mix an omitted window with
		// old percentages or reset times from a previous response.
		current := subscriptionSamplesFromExtra(&Account{Platform: PlatformOpenAI, Extra: updates}, now)
		for window, sample := range current {
			samples[window] = sample
		}
		if len(updates) > 0 {
			if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
				return nil, err
			}
		}
		return samples, nil
	}
	if !account.CanGetUsage() {
		return samples, nil
	}
	// Share the existing OAuth usage cache/flight with the admin page.
	result, err, _ := s.cache.apiFlight.Do(fmt.Sprintf("usage:%d", account.ID), func() (any, error) {
		if cached, ok := s.cache.apiCache.Load(account.ID); ok {
			if cache, ok := cached.(*apiUsageCache); ok {
				if cache.err != nil && time.Since(cache.timestamp) < apiErrorCacheTTL {
					return nil, cache.err
				}
				if cache.response != nil && time.Since(cache.timestamp) < apiCacheTTL {
					return cache.response, nil
				}
			}
		}
		response, err := s.fetchOAuthUsageRaw(ctx, account)
		s.cache.apiCache.Store(account.ID, &apiUsageCache{response: response, err: err, timestamp: time.Now()})
		return response, err
	})
	if err != nil {
		return samples, nil
	}
	response, ok := result.(*ClaudeUsageResponse)
	if !ok || response == nil {
		return nil, errors.New("subscription usage unavailable")
	}
	// Preserve the actual cached sampling time instead of refreshing it on reads.
	cached, ok := s.cache.apiCache.Load(account.ID)
	if !ok {
		return samples, nil
	}
	cache, ok := cached.(*apiUsageCache)
	if !ok || cache.response != response {
		return samples, nil
	}
	usage := s.buildUsageInfo(response, &cache.timestamp)
	if !freshLarkQuotaUsage(usage, now) {
		return samples, nil
	}
	for window, progress := range map[string]*UsageProgress{"5h": usage.FiveHour, "7d": usage.SevenDay} {
		if progress != nil {
			samples[window] = subscriptionAlertSample{progress: progress, sampledAt: cache.timestamp}
		}
	}
	return samples, nil
}
