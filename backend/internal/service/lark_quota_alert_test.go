package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type larkTestSettings struct {
	SettingRepository
	mu     sync.Mutex
	values map[string]string
}

func (r *larkTestSettings) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}
func (r *larkTestSettings) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}

// The lease fake models the existing repository contract; behavior tests keep
// the same store while replacing the service to simulate process restarts.
type larkTestLeases struct {
	IdempotencyRepository
	mu      sync.Mutex
	records map[string]*IdempotencyRecord
	nextID  int64
}

func (r *larkTestLeases) CreateProcessing(_ context.Context, record *IdempotencyRecord) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records[record.IdempotencyKeyHash] != nil {
		return false, nil
	}
	r.nextID++
	record.ID = r.nextID
	record.UpdatedAt = time.Now()
	copy := *record
	r.records[record.IdempotencyKeyHash] = &copy
	return true, nil
}
func (r *larkTestLeases) GetByScopeAndKeyHash(_ context.Context, _, key string) (*IdempotencyRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if value := r.records[key]; value != nil {
		copy := *value
		return &copy, nil
	}
	return nil, nil
}
func (r *larkTestLeases) TryReclaim(_ context.Context, id int64, status string, now, lock, expires time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.records {
		if record.ID == id && record.Status == status && (record.LockedUntil == nil || !now.Before(*record.LockedUntil)) {
			record.Status, record.LockedUntil, record.ExpiresAt = IdempotencyStatusProcessing, &lock, expires
			return true, nil
		}
	}
	return false, nil
}
func (r *larkTestLeases) MarkSucceeded(_ context.Context, id int64, status int, body string, expires time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.records {
		if record.ID == id {
			record.Status, record.LockedUntil, record.ResponseBody, record.UpdatedAt, record.ExpiresAt = IdempotencyStatusSucceeded, nil, &body, time.Now(), expires
			return nil
		}
	}
	return errors.New("missing record")
}

type larkTestCipher struct{}

func (larkTestCipher) Encrypt(value string) (string, error) {
	return base64.StdEncoding.EncodeToString([]byte(value)), nil
}
func (larkTestCipher) Decrypt(value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	return string(raw), err
}

type larkTestTransport func(*http.Request) (*http.Response, error)

func (f larkTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func larkTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func newLarkTestService(t *testing.T) *LarkQuotaAlertService {
	t.Helper()
	s := NewLarkQuotaAlertService(&larkTestSettings{values: map[string]string{}}, nil, nil, larkTestCipher{}, &larkTestLeases{records: map[string]*IdempotencyRecord{}}, nil)
	s.encryptionConfigured = true
	t.Cleanup(s.Stop)
	return s
}

const larkTestWebhook = "https://open.larksuite.com/open-apis/bot/v2/hook/test-robot-token"

func saveLarkTestSettings(t *testing.T, s *LarkQuotaAlertService) {
	t.Helper()
	_, err := s.UpdateSettings(context.Background(), LarkQuotaSettings{Enabled: true, Thresholds: []float64{95, 100}, Windows: []string{"5h", "7d"}, Webhook: larkTestWebhook})
	require.NoError(t, err)
}

func TestLarkSettingsSecretsAndDisabled(t *testing.T) {
	s := newLarkTestService(t)
	ctx := context.Background()
	defaults, err := s.GetSettings(ctx)
	require.NoError(t, err)
	require.False(t, defaults.Enabled)
	require.False(t, defaults.WebhookConfigured)
	// No webhook means no automatic send, even if enabled is requested.
	result, err := s.UpdateSettings(ctx, LarkQuotaSettings{Enabled: true, Thresholds: []float64{100, 95, 95}, Windows: []string{"5h"}})
	require.NoError(t, err)
	require.False(t, result.Enabled)
	require.Equal(t, []float64{95, 100}, result.Thresholds)
	result, err = s.UpdateSettings(ctx, LarkQuotaSettings{Enabled: true, Thresholds: []float64{95, 100}, Windows: []string{"5h"}, Webhook: larkTestWebhook, SigningSecret: "synthetic-signing-secret"})
	require.NoError(t, err)
	require.True(t, result.WebhookConfigured)
	require.True(t, result.SigningConfigured)
	require.Empty(t, result.Webhook)
	require.Empty(t, result.SigningSecret)
	raw, err := s.settings.GetValue(ctx, larkQuotaSettingsKey)
	require.NoError(t, err)
	require.NotContains(t, raw, larkTestWebhook)
	require.NotContains(t, raw, "synthetic-signing-secret")
	_, webhook, secret, err := s.runtimeSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, larkTestWebhook, webhook)
	require.Equal(t, "synthetic-signing-secret", secret)
	// Blank secrets preserve saved values through an ordinary settings save.
	result, err = s.UpdateSettings(ctx, LarkQuotaSettings{Enabled: true, Thresholds: []float64{90, 100}, Windows: []string{"7d"}})
	require.NoError(t, err)
	require.True(t, result.WebhookConfigured)
	require.True(t, result.SigningConfigured)
	result, err = s.UpdateSettings(ctx, LarkQuotaSettings{Enabled: true, Thresholds: []float64{95, 100}, Windows: []string{"5h"}, ClearWebhook: true, ClearSigningSecret: true})
	require.NoError(t, err)
	require.False(t, result.Enabled)
	require.False(t, result.WebhookConfigured)
	require.False(t, result.SigningConfigured)
}

func TestLarkValidation(t *testing.T) {
	for _, url := range []string{"http://open.larksuite.com/open-apis/bot/v2/hook/abc12345", "https://127.0.0.1/open-apis/bot/v2/hook/abc12345", "https://open.larksuite.com.evil.test/open-apis/bot/v2/hook/abc12345", larkTestWebhook + "?key=abc", larkTestWebhook + "/other", "https://open.larksuite.com:443/open-apis/bot/v2/hook/abc12345", "https://user@open.larksuite.com/open-apis/bot/v2/hook/abc12345"} {
		require.False(t, validLarkQuotaWebhook(url), url)
	}
	for _, values := range [][]float64{nil, {0}, {101}, {math.NaN()}, {math.Inf(1)}} {
		input := LarkQuotaSettings{Thresholds: values, Windows: []string{"5h"}}
		require.Error(t, normalizeLarkQuotaSettings(&input))
	}
	s := newLarkTestService(t)
	s.encryptionConfigured = false
	_, err := s.UpdateSettings(context.Background(), LarkQuotaSettings{Thresholds: []float64{95}, Windows: []string{"5h"}, Webhook: larkTestWebhook})
	require.ErrorIs(t, err, ErrSecretEncryptionKeyNotConfigured)
}

func TestLarkSenderSignatureAndFailures(t *testing.T) {
	s := newLarkTestService(t)
	s.client.Transport = larkTestTransport(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
		timestamp := payload["timestamp"].(string)
		mac := hmac.New(sha256.New, []byte(timestamp+"\nsecret"))
		require.Equal(t, base64.StdEncoding.EncodeToString(mac.Sum(nil)), payload["sign"])
		require.Equal(t, "text", payload["msg_type"])
		return larkTestResponse(200, `{"code":0}`), nil
	})
	require.NoError(t, s.send(context.Background(), larkTestWebhook, "secret", "synthetic test"))
	for _, tc := range []struct {
		status int
		body   string
	}{{302, ""}, {500, ""}, {200, `{"code":19021,"msg":"secret"}`}, {200, `{}`}, {200, strings.Repeat("x", 9000)}} {
		s.client.Transport = larkTestTransport(func(*http.Request) (*http.Response, error) { return larkTestResponse(tc.status, tc.body), nil })
		err := s.send(context.Background(), larkTestWebhook, "secret", "test")
		require.Error(t, err)
		require.NotContains(t, err.Error(), "test-robot-token")
		require.NotContains(t, err.Error(), `"msg"`)
	}
	s.client.Transport = larkTestTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New(larkTestWebhook) })
	err := s.send(context.Background(), larkTestWebhook, "", "test")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "test-robot-token")
}

func TestLarkQuotaThresholdsRestartAndReset(t *testing.T) {
	s := newLarkTestService(t)
	saveLarkTestSettings(t, s)
	var messages []string
	s.client.Transport = larkTestTransport(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		messages = append(messages, string(raw))
		return larkTestResponse(200, `{"code":0}`), nil
	})
	now, reset := time.Now(), time.Now().Add(time.Hour)
	a := &Account{ID: 1, Name: "Test account", Platform: PlatformOpenAI, Credentials: map[string]any{"email": "user@example.com"}, Groups: []*Group{{Name: "Codex"}}}
	usage := &UsageInfo{UpdatedAt: &now}
	progress := &UsageProgress{ResetsAt: &reset}
	for _, used := range []float64{94, 95, 99, 100, 100} {
		progress.Utilization = used
		require.NoError(t, s.evaluateWindow(context.Background(), a, "5h", usage, progress))
	}
	require.Len(t, messages, 2)
	require.Contains(t, messages[0], "user@example.com")
	require.Contains(t, messages[0], "95%")
	require.Contains(t, messages[1], "100%")
	// A new process shares persisted state. Header reset drift must not repeat.
	restarted := newLarkTestService(t)
	restarted.settings, restarted.leases, restarted.client = s.settings, s.leases, s.client
	drift := reset.Add(30 * time.Second)
	progress.ResetsAt = &drift
	require.NoError(t, restarted.evaluateWindow(context.Background(), a, "5h", usage, progress))
	require.Len(t, messages, 2)
	// A separately monitored window and a genuinely reset cycle can alert.
	require.NoError(t, restarted.evaluateWindow(context.Background(), a, "7d", usage, progress))
	require.Len(t, messages, 3)
	newReset := reset.Add(5 * time.Hour)
	progress.ResetsAt, progress.Utilization = &newReset, 94
	require.NoError(t, restarted.evaluateWindow(context.Background(), a, "5h", usage, progress))
	progress.Utilization = 95
	require.NoError(t, restarted.evaluateWindow(context.Background(), a, "5h", usage, progress))
	require.Len(t, messages, 4)
}

func TestLarkQuotaJumpFreshnessDisabledAndRetryLimit(t *testing.T) {
	s := newLarkTestService(t)
	saveLarkTestSettings(t, s)
	var count int
	s.client.Transport = larkTestTransport(func(*http.Request) (*http.Response, error) { count++; return larkTestResponse(200, `{"code":0}`), nil })
	now, reset := time.Now(), time.Now().Add(time.Hour)
	a := &Account{ID: 2, Platform: PlatformAnthropic}
	usage := &UsageInfo{UpdatedAt: &now}
	progress := &UsageProgress{Utilization: 100, ResetsAt: &reset}
	require.NoError(t, s.evaluateWindow(context.Background(), a, "5h", usage, progress))
	require.Equal(t, 1, count)
	stale := now.Add(-6 * time.Minute)
	usage.UpdatedAt = &stale
	require.NoError(t, s.evaluateWindow(context.Background(), a, "7d", usage, progress))
	require.Equal(t, 1, count)
	usage.UpdatedAt = nil
	require.NoError(t, s.evaluateWindow(context.Background(), a, "7d", usage, progress))
	require.Equal(t, 1, count)
	usage.UpdatedAt = &now
	_, err := s.UpdateSettings(context.Background(), LarkQuotaSettings{Thresholds: []float64{95, 100}, Windows: []string{"5h", "7d"}})
	require.NoError(t, err)
	require.NoError(t, s.evaluateWindow(context.Background(), a, "7d", usage, progress))
	require.Equal(t, 1, count)
	saveLarkTestSettings(t, s)
	s.client.Transport = larkTestTransport(func(*http.Request) (*http.Response, error) {
		count++
		return larkTestResponse(200, `{"code":19021}`), nil
	})
	store := s.leases.(*larkTestLeases)
	for range 5 {
		require.NoError(t, s.evaluateWindow(context.Background(), a, "7d", usage, progress))
		// Advance only the retry eligibility, preserving the persisted cycle.
		store.mu.Lock()
		for _, rec := range store.records {
			if rec.ResponseBody != nil {
				var state larkQuotaCycleState
				if json.Unmarshal([]byte(*rec.ResponseBody), &state) == nil && len(state.Attempts) > 0 {
					for threshold, attempt := range state.Attempts {
						attempt.RetryAfter = time.Time{}
						state.Attempts[threshold] = attempt
					}
					data, _ := json.Marshal(state)
					body := string(data)
					rec.ResponseBody = &body
				}
			}
		}
		store.mu.Unlock()
	}
	require.Equal(t, 4, count) // initial success plus three failed attempts
}

func TestLarkConcurrentWindowsShareLease(t *testing.T) {
	s := newLarkTestService(t)
	saveLarkTestSettings(t, s)
	var sent atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	s.client.Transport = larkTestTransport(func(*http.Request) (*http.Response, error) {
		sent.Add(1)
		close(entered)
		<-release
		return larkTestResponse(200, `{"code":0}`), nil
	})
	now, reset := time.Now(), time.Now().Add(time.Hour)
	a := &Account{ID: 9, Platform: PlatformOpenAI}
	usage := &UsageInfo{UpdatedAt: &now}
	progress := &UsageProgress{Utilization: 100, ResetsAt: &reset}
	done := make(chan error, 1)
	go func() { done <- s.evaluateWindow(context.Background(), a, "5h", usage, progress) }()
	<-entered
	err := s.evaluateWindow(context.Background(), a, "5h", usage, progress)
	require.ErrorIs(t, err, ErrIdempotencyInProgress)
	close(release)
	require.NoError(t, <-done)
	require.NoError(t, s.evaluateWindow(context.Background(), a, "5h", usage, progress))
	require.EqualValues(t, 1, sent.Load())
}

func TestLarkSnapshotUsageDoesNotProbeOrRecover(t *testing.T) {
	now, reset := time.Now().UTC(), time.Now().Add(time.Hour).UTC()
	s := &AccountUsageService{} // nil upstream/account repositories panic if touched
	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Extra: map[string]any{
		"codex_5h_sampled_at": now.Format(time.RFC3339), "codex_5h_used_percent": 96.0, "codex_5h_reset_at": reset.Format(time.RFC3339),
	}}
	usage, err := s.SubscriptionAlertUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	require.Equal(t, 96.0, usage["5h"].progress.Utilization)
}
