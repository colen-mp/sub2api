package llmtelemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type simulationUserKey struct{}

func TestConcurrentUserContentIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	exp := tracetest.NewInMemoryExporter()
	cfg := testConfig()
	cfg.QueueSize = 64
	cfg.BatchSize = 1
	var r *Runtime
	var stop func(context.Context) error
	if os.Getenv("SUB2API_TEST_LIVE_LANGFUSE") == "1" {
		var err error
		stop, err = InitFromEnv(context.Background())
		require.NoError(t, err)
		r = current.Load()
	} else {
		r = newRuntime(cfg, exp)
		old := current.Swap(r)
		defer current.Store(old)
		stop = r.provider.Shutdown
	}
	defer stop(context.Background())
	inner := &mockUpstream{do: func(req *http.Request) (*http.Response, error) {
		b, e := io.ReadAll(req.Body)
		if e != nil {
			return nil, e
		}
		_ = req.Body.Close()
		var doc map[string]any
		if e = json.Unmarshal(b, &doc); e != nil {
			return nil, e
		}
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "reply:" + doc["input"].(string)}}}, "usage": map[string]int{"prompt_tokens": 12, "completion_tokens": 34}})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	}}
	u := Wrap(inner)
	engine := gin.New()
	engine.Use(RequestMiddleware(func(ctx context.Context) Identity {
		return Identity{UserID: ctx.Value(simulationUserKey{}).(int64), GroupID: 1, Platform: "openai"}
	}))
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Header("X-Simulation-Trace", trace.SpanFromContext(c.Request.Context()).SpanContext().TraceID().String())
		req, e := http.NewRequestWithContext(c.Request.Context(), "POST", "https://simulation.invalid/v1/responses", c.Request.Body)
		if e != nil {
			c.Status(500)
			return
		}
		resp, e := u.Do(req, "", 101, 1)
		if e != nil {
			c.Status(502)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(c.Writer, resp.Body)
	})
	type row struct {
		User   int64  `json:"user"`
		Marker string `json:"marker"`
		Trace  string `json:"trace"`
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	rows := []row{}
	start := make(chan struct{})
	users := 3
	if value := os.Getenv("SUB2API_TEST_USERS"); value != "" {
		n, err := strconv.Atoi(value)
		require.NoError(t, err)
		require.True(t, n > 0 && n <= 100)
		users = n
	}
	for user := int64(900000000001); user < 900000000001+int64(users); user++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			<-start
			for n := 1; n <= 2; n++ {
				marker := fmt.Sprintf("multiuser-smoke-%d-%d", id, n)
				body, _ := json.Marshal(map[string]any{"model": "simulation", "input": marker})
				req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body))).WithContext(context.WithValue(context.Background(), simulationUserKey{}, id))
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "reply:"+marker) {
					t.Errorf("simulation response mismatch for %d", id)
				}
				mu.Lock()
				rows = append(rows, row{id, marker, w.Header().Get("X-Simulation-Trace")})
				mu.Unlock()
			}
		}(user)
	}
	close(start)
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, r.provider.ForceFlush(ctx))
	require.Zero(t, r.ExportFailures.Load())
	if os.Getenv("SUB2API_TEST_LIVE_LANGFUSE") == "1" {
		b, _ := json.Marshal(rows)
		t.Log("SIMULATION_ROWS", string(b))
		return
	}
	spans := exp.GetSpans()
	require.Len(t, spans, users*4)
	for _, row := range rows {
		count := 0
		for _, span := range spans {
			if span.SpanContext.TraceID().String() != row.Trace {
				continue
			}
			count++
			require.Equal(t, row.User, attr(span.Attributes, "sub2api.user_id"))
			require.Contains(t, attr(span.Attributes, "langfuse.observation.input"), row.Marker)
			require.Equal(t, "reply:"+row.Marker, attr(span.Attributes, "langfuse.observation.output"))
		}
		require.Equal(t, 2, count)
	}
}
