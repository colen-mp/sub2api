package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

const larkQuotaSnapshotTTL = 5 * time.Minute

type subscriptionAlertUsageSource interface {
	SubscriptionAlertUsage(context.Context, *Account, bool) (subscriptionAlertSamples, error)
}

type larkQuotaScanCursor struct{ page, offset int }

// LarkQuotaAlertService owns optional subscription notifications only. Existing
// billing/quota enforcement and account recovery remain with their current owners.
type LarkQuotaAlertService struct {
	settings             SettingRepository
	accounts             AccountRepository
	usage                subscriptionAlertUsageSource
	encryptor            SecretEncryptor
	encryptionConfigured bool
	leases               IdempotencyRepository
	client               *http.Client
	worker               *usageNotificationWorker
	pending              sync.Map
	ctx                  context.Context
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
	start                sync.Once
	stop                 sync.Once
	scanCursors          map[string]larkQuotaScanCursor
	scanFirstPlatform    int
	enabled              atomic.Bool
}

func NewLarkQuotaAlertService(settings SettingRepository, accounts AccountRepository, usage *AccountUsageService, encryptor SecretEncryptor, leases IdempotencyRepository, cfg *config.Config) *LarkQuotaAlertService {
	ctx, cancel := context.WithCancel(context.Background())
	return &LarkQuotaAlertService{settings: settings, accounts: accounts, usage: usage, encryptor: encryptor, leases: leases,
		encryptionConfigured: cfg != nil && cfg.Totp.EncryptionKeyConfigured,
		client:               &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		worker:               newUsageNotificationWorker(), ctx: ctx, cancel: cancel, scanCursors: make(map[string]larkQuotaScanCursor)}
}

var subscriptionAlertNotifier struct {
	sync.RWMutex
	service *LarkQuotaAlertService
}

func notifySubscriptionQuotaAlert(accountID int64) {
	subscriptionAlertNotifier.RLock()
	s := subscriptionAlertNotifier.service
	subscriptionAlertNotifier.RUnlock()
	if s != nil {
		s.Notify(accountID)
	}
}

func (s *LarkQuotaAlertService) Start() {
	s.start.Do(func() {
		subscriptionAlertNotifier.Lock()
		subscriptionAlertNotifier.service = s
		subscriptionAlertNotifier.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			s.scan()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					s.scan()
				}
			}
		}()
	})
}

func (s *LarkQuotaAlertService) Stop() {
	s.stop.Do(func() {
		subscriptionAlertNotifier.Lock()
		if subscriptionAlertNotifier.service == s {
			subscriptionAlertNotifier.service = nil
		}
		subscriptionAlertNotifier.Unlock()
		s.cancel()
		s.wg.Wait()
		s.worker.stop()
	})
}

func (s *LarkQuotaAlertService) scan() {
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	cfg, err := s.loadSettings(ctx)
	s.enabled.Store(err == nil && cfg.Enabled && cfg.Webhook != "")
	if err != nil || !cfg.Enabled || cfg.Webhook == "" {
		return
	}
	platforms := []string{PlatformOpenAI, PlatformAnthropic}
	for offset := range platforms {
		platform := platforms[(s.scanFirstPlatform+offset)%len(platforms)]
		cursor := s.scanCursors[platform]
		if cursor.page < 1 {
			cursor.page = 1
		}
		accounts, info, err := s.accounts.ListWithFilters(ctx, pagination.PaginationParams{Page: cursor.page, PageSize: 100}, platform, "", StatusActive, "", 0, "")
		if err != nil {
			slog.Warn("lark_quota_scan_failed", "platform", platform)
			continue
		}
		for cursor.offset < len(accounts) && ctx.Err() == nil {
			account := &accounts[cursor.offset]
			if supportsLarkQuotaAccount(account) && !s.enqueue(account.ID, true) {
				break // Resume here next time; a full queue must not starve later accounts.
			}
			cursor.offset++
		}
		if cursor.offset >= len(accounts) {
			cursor.offset = 0
			cursor.page++
			if len(accounts) < 100 || info == nil || cursor.page > info.Pages {
				cursor.page = 1
			}
		}
		s.scanCursors[platform] = cursor
	}
	s.scanFirstPlatform = (s.scanFirstPlatform + 1) % len(platforms)
}

func supportsLarkQuotaAccount(a *Account) bool {
	return a != nil && a.IsActive() && !a.IsShadow() && !a.IsSyntheticUITest() && ((a.Platform == PlatformOpenAI && a.Type == AccountTypeOAuth) || (a.Platform == PlatformAnthropic && a.IsAnthropicOAuthOrSetupToken()))
}

// Notify never performs I/O on the forwarding path; a later scan compensates
// for queue saturation. A pending account occupies at most one worker slot.
func (s *LarkQuotaAlertService) Notify(accountID int64) bool {
	return s.enqueue(accountID, false)
}

func (s *LarkQuotaAlertService) enqueue(accountID int64, refresh bool) bool {
	if accountID <= 0 || s.ctx.Err() != nil || !s.enabled.Load() {
		return false
	}
	if _, loaded := s.pending.LoadOrStore(accountID, struct{}{}); loaded {
		return true
	}
	if !s.worker.submit(func(ctx context.Context) {
		defer s.pending.Delete(accountID)
		if err := s.evaluateAccount(ctx, accountID, refresh); err != nil && ctx.Err() == nil && !errors.Is(err, ErrIdempotencyInProgress) {
			slog.Warn("lark_quota_evaluation_failed", "account_id", accountID)
		}
	}) {
		s.pending.Delete(accountID)
		return false
	}
	return true
}

func (s *LarkQuotaAlertService) evaluateAccount(ctx context.Context, accountID int64, refresh bool) error {
	cfg, err := s.loadSettings(ctx)
	if err != nil || !cfg.Enabled || cfg.Webhook == "" {
		return err
	}
	account, err := s.accounts.GetByID(ctx, accountID)
	if err != nil || !supportsLarkQuotaAccount(account) {
		return err
	}
	usage, err := s.usage.SubscriptionAlertUsage(ctx, account, refresh)
	if err != nil {
		return err
	}
	for _, window := range cfg.Windows {
		sample, ok := usage[window]
		if !ok {
			continue
		}
		if err := s.evaluateWindow(ctx, account, window, &UsageInfo{UpdatedAt: &sample.sampledAt}, sample.progress); err != nil {
			return err
		}
	}
	return nil
}

func freshLarkQuotaUsage(usage *UsageInfo, now time.Time) bool {
	return usage != nil && usage.Error == "" && usage.UpdatedAt != nil && !usage.UpdatedAt.After(now.Add(time.Minute)) && now.Sub(*usage.UpdatedAt) <= larkQuotaSnapshotTTL
}

// Stored in the existing idempotency record response, under an account/window
// lease. No PII, credentials or message bodies are persisted here.
type larkQuotaCycleState struct {
	ResetAt      time.Time                   `json:"reset_at"`
	LastObserved time.Time                   `json:"last_observed"`
	LastUsed     float64                     `json:"last_used"`
	HighestSent  float64                     `json:"highest_sent"`
	Attempts     map[string]larkQuotaAttempt `json:"attempts,omitempty"`
	LastError    string                      `json:"last_error,omitempty"`
}

type larkQuotaAttempt struct {
	Count      int       `json:"count"`
	RetryAfter time.Time `json:"retry_after"`
}

func (s *LarkQuotaAlertService) evaluateWindow(ctx context.Context, account *Account, window string, usage *UsageInfo, progress *UsageProgress) error {
	now := time.Now()
	if !freshLarkQuotaUsage(usage, now) || progress == nil || progress.ResetsAt == nil || !progress.ResetsAt.After(now) || progress.ResetsAt.After(now.Add(8*24*time.Hour)) || math.IsNaN(progress.Utilization) || math.IsInf(progress.Utilization, 0) || progress.Utilization < 0 {
		return nil
	}
	return s.withLease(ctx, strconv.FormatInt(account.ID, 10)+":"+window, func(ctx context.Context, record *IdempotencyRecord) (any, error) {
		state := larkQuotaCycleState{}
		if record.ResponseBody != nil && json.Unmarshal([]byte(*record.ResponseBody), &state) != nil {
			return nil, errors.New("Lark quota state invalid")
		}
		if usage.UpdatedAt.Before(state.LastObserved) {
			return state, nil
		}
		// A few seconds of reset-header drift is common. A substantially changed
		// reset plus a drop in usage also recognizes manually reset subscriptions.
		resetChanged := math.Abs(progress.ResetsAt.Sub(state.ResetAt).Seconds()) > 300
		if state.ResetAt.IsZero() || !now.Before(state.ResetAt) || (resetChanged && progress.Utilization < state.LastUsed) {
			state = larkQuotaCycleState{ResetAt: *progress.ResetsAt}
		}
		state.LastObserved, state.LastUsed = *usage.UpdatedAt, progress.Utilization
		// Re-read after acquiring the shared lease, so queued work respects a
		// disabled/cleared robot and updated thresholds at delivery time.
		cfg, webhook, secret, err := s.runtimeSettings(ctx)
		if err != nil {
			return state, err
		}
		selected := false
		for _, w := range cfg.Windows {
			if w == window {
				selected = true
			}
		}
		if !cfg.Enabled || webhook == "" || !selected {
			return state, nil
		}
		threshold := float64(0)
		for _, value := range cfg.Thresholds {
			if value <= progress.Utilization && value > state.HighestSent && value > threshold {
				threshold = value
			}
		}
		if threshold == 0 {
			return state, nil
		}
		if state.Attempts == nil {
			state.Attempts = make(map[string]larkQuotaAttempt)
		}
		thresholdKey := strconv.FormatFloat(threshold, 'g', -1, 64)
		attempt := state.Attempts[thresholdKey]
		if attempt.Count >= 3 || now.Before(attempt.RetryAfter) {
			return state, nil
		}
		attempt.Count++
		attempt.RetryAfter = now.Add(time.Minute)
		state.Attempts[thresholdKey] = attempt
		if err := s.send(ctx, webhook, secret, larkQuotaMessage(account, window, progress, threshold)); err != nil {
			state.LastError = err.Error()
			slog.Warn("lark_quota_delivery_failed", "account_id", account.ID, "window", window, "attempt", attempt.Count, "reason", state.LastError)
			return state, nil
		}
		state.HighestSent, state.LastError = threshold, ""
		slog.Info("lark_quota_notification_sent", "account_id", account.ID, "window", window, "threshold", threshold)
		return state, nil
	})
}

// Reuse the DB's conditional lease acquisition. Unlike HTTP request replay,
// completed records may be reclaimed to evaluate a later sample in the cycle.
func (s *LarkQuotaAlertService) withLease(ctx context.Context, key string, fn func(context.Context, *IdempotencyRecord) (any, error)) error {
	if s.leases == nil {
		return errors.New("Lark notification persistence unavailable")
	}
	// Complete operations before the 45-second lease can be reclaimed.
	ctx, operationCancel := context.WithTimeout(ctx, 30*time.Second)
	defer operationCancel()
	now := time.Now()
	hash := sha256.Sum256([]byte(key))
	lockedUntil, expires := now.Add(45*time.Second), now.Add(8*24*time.Hour)
	record := &IdempotencyRecord{Scope: "lark-subscription-quota", IdempotencyKeyHash: hex.EncodeToString(hash[:]), RequestFingerprint: "v1", Status: IdempotencyStatusProcessing, LockedUntil: &lockedUntil, ExpiresAt: expires}
	created, err := s.leases.CreateProcessing(ctx, record)
	if err != nil {
		return err
	}
	if !created {
		record, err = s.leases.GetByScopeAndKeyHash(ctx, record.Scope, record.IdempotencyKeyHash)
		if err != nil {
			return err
		}
		if record == nil {
			return ErrIdempotencyInProgress
		}
		if key == "test" && now.Sub(record.UpdatedAt) < 10*time.Second {
			return ErrIdempotencyRetryBackoff
		}
		claimed, err := s.leases.TryReclaim(ctx, record.ID, record.Status, now, lockedUntil, expires)
		if err != nil {
			return err
		}
		if !claimed {
			return ErrIdempotencyInProgress
		}
		// Another owner may have completed between our read and acquisition.
		// Always evaluate the state protected by the lock we actually acquired.
		record, err = s.leases.GetByScopeAndKeyHash(ctx, record.Scope, record.IdempotencyKeyHash)
		if err != nil {
			return err
		}
		if record == nil {
			return ErrIdempotencyInProgress
		}
	}
	state, operationErr := fn(ctx, record)
	body := "{}"
	if record.ResponseBody != nil {
		body = *record.ResponseBody
	}
	if state != nil {
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		body = string(data)
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.leases.MarkSucceeded(persistCtx, record.ID, http.StatusOK, body, expires); err != nil {
		return err
	}
	return operationErr
}
