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
