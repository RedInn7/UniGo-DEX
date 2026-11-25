package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 同一个 IP 换端口（新连接）不能绕过限流
func TestRateLimitIsPerIPNotPerConnection(t *testing.T) {
	rl := NewRateLimiter(2, time.Hour)
	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	codes := map[string][]int{}
	for _, remote := range []string{"10.0.0.1:1001", "10.0.0.1:1002", "10.0.0.1:1003", "10.0.0.2:1001"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		ip := remote[:8]
		codes[ip] = append(codes[ip], rec.Code)
	}
	if got := codes["10.0.0.1"]; got[0] != 200 || got[1] != 200 || got[2] != http.StatusTooManyRequests {
		t.Fatalf("10.0.0.1: %v, want [200 200 429]", got)
	}
	if got := codes["10.0.0.2"]; got[0] != 200 {
		t.Fatalf("10.0.0.2: %v, want [200]", got)
	}
}

func TestLoggingWriterSupportsStreaming(t *testing.T) {
	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapped ResponseWriter lost http.Flusher")
		}
		if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("ResponseController cannot reach the underlying writer: %v", err)
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

// 配置为每秒 n 个请求时，持续速率就是每秒 n 个，而不是用完突发量后每秒 1 个
func TestRateLimitSustainedRate(t *testing.T) {
	const perSecond = 50
	rl := NewRateLimiter(perSecond, time.Second/perSecond)

	allowed := 0
	for i := 0; i < perSecond*3; i++ { // 先把突发量耗尽
		if rl.Allow("10.0.0.9") {
			allowed++
		}
	}
	if allowed != perSecond {
		t.Fatalf("burst allowed %d, want %d", allowed, perSecond)
	}

	time.Sleep(400 * time.Millisecond) // 应补回约 20 个
	allowed = 0
	for i := 0; i < perSecond; i++ {
		if rl.Allow("10.0.0.9") {
			allowed++
		}
	}
	if allowed < 18 || allowed > 24 {
		t.Fatalf("after 400ms allowed %d more, want about 20", allowed)
	}
}

func TestLimitsCanBeDisabled(t *testing.T) {
	s := newTestServerWith(func(c *ServerConfig) { c.RateLimitPerIP, c.MaxConcurrent, c.DisableAccessLog = 0, 0, true })
	for i := 0; i < 500; i++ {
		if code := s.do(t, "GET", "/health", "", nil); code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	limited := newTestServerWith(func(c *ServerConfig) { c.RateLimitPerIP, c.RateLimitBurst = 5, 5 })
	rejected := 0
	for i := 0; i < 50; i++ {
		if limited.do(t, "GET", "/health", "", nil) == http.StatusTooManyRequests {
			rejected++
		}
	}
	if rejected < 40 {
		t.Fatalf("rate limit of 5/s rejected only %d of 50 back-to-back requests", rejected)
	}
}

// 经过完整的中间件链：突发容量用完之后，放行速率等于配置的每秒速率
func TestRateLimitBurstThenSustainedRate(t *testing.T) {
	s := newTestServerWith(func(c *ServerConfig) { c.RateLimitPerIP, c.RateLimitBurst, c.DisableAccessLog = 50, 80, true })
	count := func(n int) (ok int) {
		for i := 0; i < n; i++ {
			if s.do(t, "GET", "/health", "", nil) == http.StatusOK {
				ok++
			}
		}
		return ok
	}
	if got := count(200); got < 80 || got > 84 {
		t.Fatalf("burst let %d requests through, want about 80", got)
	}
	// 按实际经过的时间算上限：机器繁忙时 Sleep 会超出 500ms，补进来的 token 也相应变多
	start := time.Now()
	time.Sleep(500 * time.Millisecond)
	got := count(200)
	if max := int(50*time.Since(start).Seconds()) + 5; got < 23 || got > max {
		t.Fatalf("after %v %d more requests passed, want between 23 and %d", time.Since(start), got, max)
	}
}
