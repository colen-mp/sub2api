package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Fixed host/path validation plus disabled redirects keep the write-only token
// at the intended robot endpoint. Never return transport errors containing URLs.
func (s *LarkQuotaAlertService) send(ctx context.Context, webhook, secret, text string) error {
	if !validLarkQuotaWebhook(webhook) {
		return errors.New("Lark webhook unavailable")
	}
	payload := map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
	if secret != "" {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
		payload["timestamp"], payload["sign"] = timestamp, base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.New("Lark message encoding failed")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(data))
	if err != nil {
		return errors.New("Lark request invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.New("Lark delivery failed or timed out")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Lark HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil || len(body) > 8192 {
		return errors.New("Lark response invalid")
	}
	var result struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(body, &result) != nil || result.Code == nil {
		return errors.New("Lark response invalid")
	}
	if *result.Code != 0 {
		return fmt.Errorf("Lark rejected message (code %d)", *result.Code)
	}
	return nil
}

func (s *LarkQuotaAlertService) TestNotification(ctx context.Context) error {
	_, webhook, secret, err := s.runtimeSettings(ctx)
	if err != nil {
		return err
	}
	if webhook == "" {
		return errors.New("save a Lark webhook before testing")
	}
	return s.withLease(ctx, "test", func(ctx context.Context, _ *IdempotencyRecord) (any, error) {
		// Keep the lease through the HTTP operation and rate limit manual tests.
		return struct{}{}, s.send(ctx, webhook, secret, "Sub2API 订阅额度预警 · 测试通知\n机器人配置可用。本消息为手动测试，不代表账号额度不足。")
	})
}

func larkQuotaMessage(account *Account, window string, progress *UsageProgress, threshold float64) string {
	title := "订阅额度不足 · 预警"
	if threshold >= 100 {
		title = "订阅额度达到 100% · 告警"
	}
	email, _ := account.Credentials["email"].(string)
	if email == "" {
		email, _ = account.Extra["email"].(string)
	}
	if email == "" {
		email = "未提供"
	}
	groups := make([]string, 0, len(account.Groups))
	for _, g := range account.Groups {
		if g != nil {
			groups = append(groups, quotaMessageValue(g.Name))
		}
	}
	if len(groups) == 0 {
		groups = append(groups, "未分组")
	}
	label := "5 小时"
	if window == "7d" {
		label = "7 天"
	}
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	return fmt.Sprintf("Sub2API %s\n账号名称：%s\n账号邮箱：%s\n平台：%s\n所属分组：%s\n额度窗口：%s\n已使用：%.2f%%\n剩余额度：%.2f%%\n触发阈值：%g%%\n预计重置：%s（北京时间）",
		title, quotaMessageValue(account.Name), quotaMessageValue(email), account.Platform, strings.Join(groups, "、"), label,
		progress.Utilization, math.Max(0, 100-progress.Utilization), threshold, progress.ResetsAt.In(zone).Format("2006-01-02 15:04:05"))
}

func quotaMessageValue(value string) string {
	runes := []rune(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, value))
	if len(runes) > 160 {
		runes = runes[:160]
	}
	return string(runes)
}
