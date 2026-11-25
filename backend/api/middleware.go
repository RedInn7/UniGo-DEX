package api

import (
	"net"
	"net/http"
	"sync"
	"time"

	"unigo-dex/logging"
	"unigo-dex/metrics"
)

// middleware.go — HTTP 中间件（限流 + 背压 + 日志）
//
// === 背压机制（Backpressure）===
//
// 在极端行情下，大量请求可能同时涌入 API 层。
// 如果不加控制，请求会堆积在内存中，最终导致 OOM。
//
// 本模块实现了两级保护：
//
//   1. 全局并发限制（Semaphore）
//      - 使用有界 Channel 限制同时处理的请求数
//      - 超出限制时返回 503 Service Unavailable
//      - 而非让请求排队等待（避免延迟恶化）
//
//   2. 每 IP 限流（Rate Limiting）
//      - Token Bucket 算法
//      - 防止单个用户/IP 过度消耗资源

// ConcurrencyLimiter 并发限制中间件
//
// 使用有界 Channel 作为信号量，控制同时处理的请求数
// 超出限制时立即返回 503，而非排队等待
type ConcurrencyLimiter struct {
	sem chan struct{}
}

// NewConcurrencyLimiter 创建并发限制器
func NewConcurrencyLimiter(maxConcurrent int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		sem: make(chan struct{}, maxConcurrent),
	}
}

// Middleware 返回 HTTP 中间件
func (cl *ConcurrencyLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 尝试非阻塞地获取信号量
		select {
		case cl.sem <- struct{}{}:
			defer func() { <-cl.sem }()
			next.ServeHTTP(w, r)
		default:
			// 信号量已满：返回 503 + Retry-After 头
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"服务器繁忙，请稍后重试","code":503}`, http.StatusServiceUnavailable)
			metrics.HTTPConcurrencyRejected.Inc()
			// 过载时每秒可能拒绝上万个请求，逐条打日志只会雪上加霜；拒绝数看指标
			logging.L(r.Context()).Debug("并发上限触发，拒绝请求", "method", r.Method, "path", r.URL.Path)
		}
	})
}

// RateLimiter 每 IP 限流器（Token Bucket 算法）
//
// Token Bucket 原理：
//   - 每个 IP 有一个 bucket，容量 = maxTokens
//   - 每次请求消耗 1 个 token
//   - token 以 refillRate 的速率持续补充
//   - bucket 为空时拒绝请求
type RateLimiter struct {
	buckets    map[string]*tokenBucket
	mu         sync.Mutex
	maxTokens  int
	refillRate time.Duration
}

type tokenBucket struct {
	tokens   int
	lastTime time.Time
}

// NewRateLimiter 创建限流器
//
// maxTokens 是桶容量（允许的突发量），refillRate 是补充 1 个 token 的间隔。
func NewRateLimiter(maxTokens int, refillRate time.Duration) *RateLimiter {
	rl := &RateLimiter{
		buckets:    make(map[string]*tokenBucket),
		maxTokens:  maxTokens,
		refillRate: refillRate,
	}
	// 定期清理过期的 bucket
	go rl.cleanup()
	return rl
}

// Allow 检查是否允许请求
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	bucket, exists := rl.buckets[ip]
	if !exists {
		bucket = &tokenBucket{
			tokens:   rl.maxTokens,
			lastTime: time.Now(),
		}
		rl.buckets[ip] = bucket
	}

	// 补充 token
	elapsed := time.Since(bucket.lastTime)
	refilled := int(elapsed / rl.refillRate)
	if refilled > 0 {
		bucket.tokens += refilled
		if bucket.tokens > rl.maxTokens {
			bucket.tokens = rl.maxTokens
		}
		// 只推进已经兑换成 token 的那部分时间，不足一个 token 的零头留到下次
		bucket.lastTime = bucket.lastTime.Add(time.Duration(refilled) * rl.refillRate)
	}

	if bucket.tokens <= 0 {
		return false
	}

	bucket.tokens--
	return true
}

// Middleware 返回 HTTP 中间件
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RemoteAddr 是 "IP:端口"，每个连接的端口都不同，必须去掉端口才是按 IP 限流
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}

		if !rl.Allow(ip) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"请求过于频繁","code":429}`, http.StatusTooManyRequests)
			metrics.HTTPRateLimited.Inc()
			logging.L(r.Context()).Debug("限流触发", "ip", ip)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// cleanup 定期清理长时间不活跃的 bucket
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-10 * time.Minute)
		for ip, bucket := range rl.buckets {
			if bucket.lastTime.Before(cutoff) {
				delete(rl.buckets, ip)
			}
		}
		rl.mu.Unlock()
	}
}

// TraceMiddleware 链路追踪中间件
//
// 每个请求带一个 Trace ID：客户端通过 X-Trace-Id 传入就沿用（网关、上游服务可以把同一个 ID 串下来），
// 否则生成一个。ID 放进 context，后面所有用 logging.L(ctx) 打的日志都会带上 trace_id，
// 同时写回响应头，客户端报障时可以拿它查日志。
func TraceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if id := r.Header.Get("X-Trace-Id"); validTraceID(id) {
			ctx = logging.WithExistingTraceID(ctx, id)
		} else {
			ctx = logging.WithTraceID(ctx)
		}
		w.Header().Set("X-Trace-Id", logging.GetTraceID(ctx))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validTraceID 只接受 8~64 位的字母、数字、连字符，避免把任意内容写进日志
func validTraceID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// MetricsMiddleware 记录请求总数、在途请求数和延迟分布
//
// 放在限流和并发控制的外层：被 429 / 503 拒绝的请求也计入总数。
func MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		metrics.HTTPRequestsTotal.Inc()
		metrics.HTTPActiveConnections.Inc()
		defer func() {
			metrics.HTTPActiveConnections.Dec()
			metrics.HTTPRequestDuration.Observe(float64(time.Since(start).Microseconds()) / 1000)
		}()
		next.ServeHTTP(w, r)
	})
}

// LoggingMiddleware 请求日志中间件
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// 包装 ResponseWriter 以捕获状态码
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(lrw, r)

		logging.L(r.Context()).Info("请求完成",
			"method", r.Method, "path", r.URL.Path, "status", lrw.statusCode,
			"latency_ms", float64(time.Since(start).Microseconds())/1000)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// Flush 透传给底层 ResponseWriter，否则 SSE 这类流式响应发不出去
func (lrw *loggingResponseWriter) Flush() {
	if f, ok := lrw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能找到底层 ResponseWriter
func (lrw *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return lrw.ResponseWriter
}

// CORSMiddleware 跨域中间件
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
