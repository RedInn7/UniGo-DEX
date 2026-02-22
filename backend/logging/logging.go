package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"time"
)

// logging.go — 结构化日志（基于 Go 1.21 slog）
//
// === 为什么用 slog 而不是 fmt.Printf / log？ ===
//
//   1. 结构化输出：JSON 格式，可被 ELK / Loki / Datadog 直接解析
//   2. 日志级别：Debug / Info / Warn / Error，生产环境可动态切级
//   3. 上下文传递：通过 context 携带 Trace ID，串联一个请求的全链路日志
//   4. 性能：slog 的 Handler 接口支持零分配优化
//
// === Trace ID 链路追踪 ===
//
// 每个 HTTP 请求生成唯一的 Trace ID，注入 context：
//
//   Request → Middleware(生成 TraceID) → Handler → Router → Indexer
//                  ↓                        ↓          ↓
//            [trace_id=abc123]        [trace_id=abc123] ...
//
// 所有模块的日志都携带同一个 trace_id，方便在日志系统中筛选一个请求的完整链路。

type contextKey string

const traceIDKey contextKey = "trace_id"

// Init 初始化全局日志
//
// level:
//   - "debug": 开发环境，输出所有日志
//   - "info":  生产环境默认
//   - "warn":  只关注警告和错误
//   - "error": 只关注错误
//
// format:
//   - "json": 生产环境，JSON 格式（机器可读）
//   - "text": 开发环境，人类可读
func Init(level, format string) {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: logLevel,
		// 添加调用位置（文件:行号）
		AddSource: logLevel == slog.LevelDebug,
	}

	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}

// WithTraceID 向 context 注入 Trace ID
func WithTraceID(ctx context.Context) context.Context {
	id := generateTraceID()
	return context.WithValue(ctx, traceIDKey, id)
}

// WithExistingTraceID 使用已有的 Trace ID（如从 HTTP Header 传入）
func WithExistingTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

// GetTraceID 从 context 提取 Trace ID
func GetTraceID(ctx context.Context) string {
	if id, ok := ctx.Value(traceIDKey).(string); ok {
		return id
	}
	return ""
}

// L 获取携带 trace_id 的 Logger
//
// 使用方式：
//
//	logging.L(ctx).Info("处理 swap 请求",
//	    "token_in", "WETH",
//	    "token_out", "USDC",
//	    "amount", "1000000000000000000",
//	)
//
// 输出（JSON 格式）：
//
//	{
//	  "time": "2024-01-01T00:00:00Z",
//	  "level": "INFO",
//	  "msg": "处理 swap 请求",
//	  "trace_id": "a1b2c3d4e5f6",
//	  "token_in": "WETH",
//	  "token_out": "USDC",
//	  "amount": "1000000000000000000"
//	}
func L(ctx context.Context) *slog.Logger {
	logger := slog.Default()
	if traceID := GetTraceID(ctx); traceID != "" {
		logger = logger.With("trace_id", traceID)
	}
	return logger
}

// LogRequestStart 记录请求开始（返回开始时间供后续计算延迟）
func LogRequestStart(ctx context.Context, method, path string) time.Time {
	L(ctx).Info("请求开始",
		"method", method,
		"path", path,
	)
	return time.Now()
}

// LogRequestEnd 记录请求结束
func LogRequestEnd(ctx context.Context, method, path string, status int, start time.Time) {
	L(ctx).Info("请求结束",
		"method", method,
		"path", path,
		"status", status,
		"latency_ms", time.Since(start).Milliseconds(),
	)
}

// generateTraceID 生成 12 字节的随机 Trace ID
func generateTraceID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
