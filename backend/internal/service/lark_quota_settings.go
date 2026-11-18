package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"sort"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const larkQuotaSettingsKey = "lark_subscription_quota_alerts"

// Secret inputs are write-only. Empty inputs preserve existing secrets; clearing
// requires an explicit flag so an ordinary form save cannot lose credentials.
type LarkQuotaSettings struct {
	Enabled            bool      `json:"enabled"`
	Thresholds         []float64 `json:"thresholds"`
	Windows            []string  `json:"windows"`
	Webhook            string    `json:"webhook,omitempty"`
	SigningSecret      string    `json:"signing_secret,omitempty"`
	ClearWebhook       bool      `json:"clear_webhook,omitempty"`
	ClearSigningSecret bool      `json:"clear_signing_secret,omitempty"`
	WebhookConfigured  bool      `json:"webhook_configured"`
	SigningConfigured  bool      `json:"signing_secret_configured"`
}

type larkQuotaStoredSettings struct {
	Enabled    bool      `json:"enabled"`
	Thresholds []float64 `json:"thresholds"`
	Windows    []string  `json:"windows"`
	Webhook    string    `json:"webhook_encrypted,omitempty"`
	Secret     string    `json:"signing_secret_encrypted,omitempty"`
}

func defaultLarkQuotaSettings() larkQuotaStoredSettings {
	return larkQuotaStoredSettings{Thresholds: []float64{95, 100}, Windows: []string{"5h", "7d"}}
}

func (s *LarkQuotaAlertService) loadSettings(ctx context.Context) (larkQuotaStoredSettings, error) {
	cfg := defaultLarkQuotaSettings()
	raw, err := s.settings.GetValue(ctx, larkQuotaSettingsKey)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && raw == "") {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal([]byte(raw), &cfg)
	return cfg, err
}

func publicLarkQuotaSettings(cfg larkQuotaStoredSettings) *LarkQuotaSettings {
	return &LarkQuotaSettings{Enabled: cfg.Enabled, Thresholds: cfg.Thresholds, Windows: cfg.Windows,
		WebhookConfigured: cfg.Webhook != "", SigningConfigured: cfg.Secret != ""}
}

func (s *LarkQuotaAlertService) GetSettings(ctx context.Context) (*LarkQuotaSettings, error) {
	cfg, err := s.loadSettings(ctx)
	if err != nil {
		return nil, err
	}
	return publicLarkQuotaSettings(cfg), nil
}

func normalizeLarkQuotaSettings(in *LarkQuotaSettings) error {
	if len(in.Thresholds) == 0 || len(in.Thresholds) > 10 {
		return infraerrors.BadRequest("LARK_QUOTA_THRESHOLDS_INVALID", "provide 1 to 10 thresholds between 0 and 100 percent")
	}
	sort.Float64s(in.Thresholds)
	thresholds := make([]float64, 0, len(in.Thresholds))
	for _, v := range in.Thresholds {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 100 {
			return infraerrors.BadRequest("LARK_QUOTA_THRESHOLDS_INVALID", "thresholds must be greater than 0 and at most 100 percent")
		}
		if len(thresholds) == 0 || thresholds[len(thresholds)-1] != v {
			thresholds = append(thresholds, v)
		}
	}
	in.Thresholds = thresholds
	if len(in.Windows) == 0 || len(in.Windows) > 2 {
		return infraerrors.BadRequest("LARK_QUOTA_WINDOWS_INVALID", "select at least one quota window")
	}
	seen := map[string]bool{}
	for _, w := range in.Windows {
		if (w != "5h" && w != "7d") || seen[w] {
			return infraerrors.BadRequest("LARK_QUOTA_WINDOWS_INVALID", "supported windows are 5h and 7d")
		}
		seen[w] = true
	}
	in.Webhook = strings.TrimSpace(in.Webhook)
	if in.Webhook != "" && !validLarkQuotaWebhook(in.Webhook) {
		return infraerrors.BadRequest("LARK_WEBHOOK_INVALID", "use an HTTPS open.larksuite.com robot webhook")
	}
	if len(in.SigningSecret) > 512 || len(in.Webhook) > 2048 || (in.ClearWebhook && in.Webhook != "") || (in.ClearSigningSecret && in.SigningSecret != "") {
		return infraerrors.BadRequest("LARK_SECRET_INVALID", "invalid secret input or conflicting clear operation")
	}
	return nil
}

func validLarkQuotaWebhook(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "open.larksuite.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return false
	}
	token := strings.TrimPrefix(u.Path, "/open-apis/bot/v2/hook/")
	return token != u.Path && len(token) >= 8 && !strings.ContainsAny(token, "/\\ \t\r\n")
}

func (s *LarkQuotaAlertService) UpdateSettings(ctx context.Context, in LarkQuotaSettings) (*LarkQuotaSettings, error) {
	if err := normalizeLarkQuotaSettings(&in); err != nil {
		return nil, err
	}
	// Serialize read/merge/write with the same DB lease used by all instances.
	err := s.withLease(ctx, "settings", func(ctx context.Context, _ *IdempotencyRecord) (any, error) {
		cfg, err := s.loadSettings(ctx)
		if err != nil {
			return nil, err
		}
		if in.Webhook != "" || in.SigningSecret != "" {
			if !s.encryptionConfigured || s.encryptor == nil {
				return nil, ErrSecretEncryptionKeyNotConfigured
			}
		}
		cfg.Enabled, cfg.Thresholds, cfg.Windows = in.Enabled, in.Thresholds, in.Windows
		if in.ClearWebhook {
			cfg.Webhook = ""
			cfg.Enabled = false
		}
		if in.ClearSigningSecret {
			cfg.Secret = ""
		}
		if in.Webhook != "" {
			cfg.Webhook, err = s.encryptor.Encrypt(in.Webhook)
			if err != nil {
				return nil, errors.New("failed to encrypt Lark webhook")
			}
		}
		if in.SigningSecret != "" {
			cfg.Secret, err = s.encryptor.Encrypt(in.SigningSecret)
			if err != nil {
				return nil, errors.New("failed to encrypt Lark signing secret")
			}
		}
		if cfg.Webhook == "" {
			cfg.Enabled = false
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			return nil, err
		}
		if err := s.settings.Set(ctx, larkQuotaSettingsKey, string(data)); err != nil {
			return nil, err
		}
		s.enabled.Store(cfg.Enabled && cfg.Webhook != "")
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetSettings(ctx)
}

func (s *LarkQuotaAlertService) runtimeSettings(ctx context.Context) (larkQuotaStoredSettings, string, string, error) {
	cfg, err := s.loadSettings(ctx)
	if err != nil || cfg.Webhook == "" {
		return cfg, "", "", err
	}
	if !s.encryptionConfigured || s.encryptor == nil {
		return cfg, "", "", ErrSecretEncryptionKeyNotConfigured
	}
	webhook, err := s.encryptor.Decrypt(cfg.Webhook)
	if err != nil || !validLarkQuotaWebhook(webhook) {
		return cfg, "", "", errors.New("Lark webhook unavailable")
	}
	secret := ""
	if cfg.Secret != "" {
		secret, err = s.encryptor.Decrypt(cfg.Secret)
		if err != nil {
			return cfg, "", "", errors.New("Lark signing secret unavailable")
		}
	}
	return cfg, webhook, secret, nil
}
