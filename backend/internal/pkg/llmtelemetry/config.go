// Package llmtelemetry adds opt-in, bounded OTLP tracing to Sub2API.
package llmtelemetry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Enabled, CaptureContent, CaptureReasoning     bool
	Backend, Endpoint, ServiceName                string
	Headers                                       map[string]string
	FieldMapping                                  map[string]string
	CaptureGroups                                 map[int64]bool
	MaxBytes, EventMaxBytes, QueueSize, BatchSize int
	Timeout, FlushInterval                        time.Duration
}

func LoadConfig() (Config, error) {
	c := Config{Backend: "langfuse", ServiceName: "sub2api", MaxBytes: 65536, EventMaxBytes: 262144, QueueSize: 256, BatchSize: 32, Timeout: 5 * time.Second, FlushInterval: time.Second, Headers: map[string]string{}, CaptureGroups: map[int64]bool{}}
	var err error
	c.Enabled, err = envBool("SUB2API_TELEMETRY_ENABLED", false)
	if err != nil || !c.Enabled {
		return c, err
	}
	c.FieldMapping, err = parseFieldMapping(os.Getenv("SUB2API_TELEMETRY_FIELD_MAPPING"))
	if err != nil {
		return c, err
	}
	c.CaptureContent, err = envBool("SUB2API_TELEMETRY_CAPTURE_CONTENT", false)
	if err != nil {
		return c, err
	}
	c.CaptureReasoning, err = envBool("SUB2API_TELEMETRY_CAPTURE_REASONING", false)
	if err != nil {
		return c, err
	}
	if s := strings.TrimSpace(os.Getenv("SUB2API_TELEMETRY_BACKEND")); s != "" {
		c.Backend = s
	}
	if s := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); s != "" {
		c.ServiceName = s
	}
	for _, field := range []struct {
		key      string
		dst      *int
		min, max int
	}{
		{"SUB2API_TELEMETRY_CAPTURE_MAX_BYTES", &c.MaxBytes, 1024, 16777216},
		{"SUB2API_TELEMETRY_EVENT_MAX_BYTES", &c.EventMaxBytes, 1024, 33554432},
		{"SUB2API_TELEMETRY_QUEUE_SIZE", &c.QueueSize, 1, 4096},
	} {
		if s := os.Getenv(field.key); s != "" {
			n, e := strconv.Atoi(s)
			if e != nil || n < field.min || n > field.max {
				return c, errors.New("invalid " + field.key)
			}
			*field.dst = n
		}
	}
	if c.BatchSize > c.QueueSize {
		c.BatchSize = c.QueueSize
	}
	for _, s := range strings.Split(os.Getenv("SUB2API_TELEMETRY_CAPTURE_GROUP_IDS"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		n, e := strconv.ParseInt(s, 10, 64)
		if e != nil || n <= 0 {
			return c, errors.New("invalid capture group ID")
		}
		c.CaptureGroups[n] = true
	}
	switch c.Backend {
	case "langfuse":
		host := strings.TrimRight(strings.TrimSpace(os.Getenv("LANGFUSE_HOST")), "/")
		pub, secret := os.Getenv("LANGFUSE_PUBLIC_KEY"), os.Getenv("LANGFUSE_SECRET_KEY")
		if host != "" {
			c.Endpoint = host + "/api/public/otel/v1/traces"
		}
		if pub != "" && secret != "" {
			c.Headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(pub+":"+secret))
		}
		c.Headers["x-langfuse-ingestion-version"] = "4"
	case "datadog":
		site := strings.TrimSpace(os.Getenv("DD_SITE"))
		switch site {
		case "datadoghq.com", "datadoghq.eu", "us3.datadoghq.com", "us5.datadoghq.com", "ap1.datadoghq.com", "ap2.datadoghq.com":
			c.Endpoint = "https://otlp." + site + "/v1/traces"
		}
		if key := os.Getenv("DD_API_KEY"); key != "" {
			c.Headers["dd-api-key"] = key
		}
		c.Headers["dd-otlp-source"] = "llmobs"
	case "otlp":
	default:
		return c, errors.New("SUB2API_TELEMETRY_BACKEND must be langfuse, datadog or otlp")
	}
	if s := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")); s != "" {
		c.Endpoint = s
	}
	for _, item := range strings.Split(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS"), ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			return c, errors.New("invalid OTLP header configuration")
		}
		key = strings.TrimSpace(key)
		decoded, e := url.PathUnescape(strings.TrimSpace(value))
		if e != nil || key == "" || strings.ContainsAny(key+decoded, "\r\n") {
			return c, errors.New("invalid OTLP header configuration")
		}
		c.Headers[key] = decoded
	}
	if c.Backend == "langfuse" && header(c.Headers, "Authorization") == "" {
		return c, errors.New("Langfuse project credentials are required")
	}
	if c.Backend == "datadog" && header(c.Headers, "dd-api-key") == "" {
		return c, errors.New("DD_API_KEY is required")
	}
	u, e := url.Parse(c.Endpoint)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("invalid OTLP trace endpoint; use a full http(s) URL without credentials or query")
	}
	return c, nil
}

func parseFieldMapping(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	invalid := errors.New("invalid SUB2API_TELEMETRY_FIELD_MAPPING")
	if len(raw) > 4096 {
		return nil, invalid
	}
	var mapping map[string]string
	if json.Unmarshal([]byte(raw), &mapping) != nil || mapping == nil {
		return nil, invalid
	}
	if len(mapping) > 64 {
		return nil, invalid
	}
	targets := map[string]bool{}
	for source, target := range mapping {
		if !validMappingField(source) || !validMappingField(target) || target == "langfuse.attributes" || target == "langfuse.resourceAttributes" {
			return nil, invalid
		}
		for _, key := range mappingTargetKeys(target) {
			if targets[key] {
				return nil, invalid
			}
			targets[key] = true
		}
	}
	return mapping, nil
}

func header(h map[string]string, key string) string {
	for k, v := range h {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
func envBool(key string, fallback bool) (bool, error) {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return fallback, nil
	}
	b, e := strconv.ParseBool(s)
	if e != nil {
		return false, errors.New("invalid " + key)
	}
	return b, nil
}
func (c Config) capture(groupID int64) bool {
	return c.CaptureContent && groupID > 0 && (len(c.CaptureGroups) == 0 || c.CaptureGroups[groupID])
}

func (c Config) contentOptions() []string {
	if c.CaptureReasoning {
		return []string{c.Backend, "reasoning"}
	}
	return []string{c.Backend}
}
