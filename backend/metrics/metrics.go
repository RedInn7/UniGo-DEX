package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// metrics.go — Prometheus 风格可观测性指标
//
// === 为什么需要 Metrics？ ===
//
// 日志告诉你 "发生了什么"，Metrics 告诉你 "系统状态如何"。
//
// 核心指标：
//   - 请求延迟分布（Histogram）→ P50 / P95 / P99 延迟
//   - 请求吞吐量（Counter）→ QPS
//   - 在途请求数（Gauge）→ 并发压力
//   - 路由降级比例（Counter）→ 系统健康度
//   - 断路器状态（Gauge）→ 外部依赖健康度
//   - Goroutine 池队列深度（Gauge）→ 背压程度
//
// === 指标类型 ===
//
//   Counter:   只增不减的累计值（请求总数、错误总数）
//   Gauge:     可增可减的瞬时值（当前连接数、队列深度）
//   Histogram: 值的分布统计（请求延迟，按桶分布）
//
// === 暴露方式 ===
//
// 通过 /metrics HTTP 端点暴露，Prometheus 定期 scrape。
// 本实现使用纯标准库，不依赖 prometheus/client_golang。

// Counter 计数器（只增不减）
type Counter struct {
	name  string
	value atomic.Int64
}

// NewCounter 创建计数器
func NewCounter(name string) *Counter {
	return &Counter{name: name}
}

// Inc 递增 1
func (c *Counter) Inc() { c.value.Add(1) }

// Add 增加指定值
func (c *Counter) Add(n int64) { c.value.Add(n) }

// Value 当前值
func (c *Counter) Value() int64 { return c.value.Load() }

// Gauge 仪表盘（可增可减）
type Gauge struct {
	name  string
	value atomic.Int64
}

// NewGauge 创建仪表盘
func NewGauge(name string) *Gauge {
	return &Gauge{name: name}
}

// Set 设置值
func (g *Gauge) Set(n int64) { g.value.Store(n) }

// Inc 递增 1
func (g *Gauge) Inc() { g.value.Add(1) }

// Dec 递减 1
func (g *Gauge) Dec() { g.value.Add(-1) }

// Value 当前值
func (g *Gauge) Value() int64 { return g.value.Load() }

// Histogram 直方图（值的分布统计）
//
// 预定义桶（bucket）边界，每个值落入对应的桶。
// 查询 P99 时：找到累积比例 ≥ 99% 的最小桶边界。
//
// 典型延迟桶（毫秒）：[5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000]
type Histogram struct {
	name    string
	buckets []float64
	counts  []atomic.Int64
	sum     atomic.Int64
	count   atomic.Int64
	mu      sync.Mutex
	values  []float64 // 保留最近的值用于精确百分位计算
	maxKeep int
}

// NewHistogram 创建直方图
func NewHistogram(name string, buckets []float64) *Histogram {
	sort.Float64s(buckets)
	h := &Histogram{
		name:    name,
		buckets: buckets,
		counts:  make([]atomic.Int64, len(buckets)+1), // +1 for +Inf
		maxKeep: 10000,
	}
	return h
}

// DefaultLatencyBuckets 默认延迟桶（毫秒）
func DefaultLatencyBuckets() []float64 {
	return []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}
}

// Observe 记录一个值
func (h *Histogram) Observe(value float64) {
	h.count.Add(1)
	h.sum.Add(int64(value * 1000)) // 保留 3 位小数精度

	// 放入对应的桶
	for i, boundary := range h.buckets {
		if value <= boundary {
			h.counts[i].Add(1)
			break
		}
		if i == len(h.buckets)-1 {
			h.counts[len(h.buckets)].Add(1) // +Inf 桶
		}
	}

	// 保留最近的值用于精确百分位
	h.mu.Lock()
	if len(h.values) >= h.maxKeep {
		h.values = h.values[1:]
	}
	h.values = append(h.values, value)
	h.mu.Unlock()
}

// ObserveDuration 记录从 start 到现在的持续时间（毫秒）
func (h *Histogram) ObserveDuration(start time.Time) {
	h.Observe(float64(time.Since(start).Milliseconds()))
}

// Percentile 计算百分位数
//
// p = 0.50 → P50（中位数）
// p = 0.95 → P95
// p = 0.99 → P99
func (h *Histogram) Percentile(p float64) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.values) == 0 {
		return 0
	}

	sorted := make([]float64, len(h.values))
	copy(sorted, h.values)
	sort.Float64s(sorted)

	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Count 总观测次数
func (h *Histogram) Count() int64 { return h.count.Load() }

// Avg 平均值
func (h *Histogram) Avg() float64 {
	c := h.count.Load()
	if c == 0 {
		return 0
	}
	return float64(h.sum.Load()) / 1000.0 / float64(c)
}

// Registry 指标注册中心
type Registry struct {
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	histograms map[string]*Histogram
	mu         sync.RWMutex
}

// NewRegistry 创建注册中心
func NewRegistry() *Registry {
	return &Registry{
		counters:   make(map[string]*Counter),
		gauges:     make(map[string]*Gauge),
		histograms: make(map[string]*Histogram),
	}
}

// Counter 获取或创建 Counter
func (r *Registry) Counter(name string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := NewCounter(name)
	r.counters[name] = c
	return c
}

// Gauge 获取或创建 Gauge
func (r *Registry) Gauge(name string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := NewGauge(name)
	r.gauges[name] = g
	return g
}

// Histogram 获取或创建 Histogram
func (r *Registry) Histogram(name string, buckets []float64) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.histograms[name]; ok {
		return h
	}
	h := NewHistogram(name, buckets)
	r.histograms[name] = h
	return h
}

// Handler 返回 /metrics HTTP 处理器
//
// 输出 Prometheus 文本格式：
//
//	# TYPE http_requests_total counter
//	http_requests_total 12345
//	# TYPE http_request_duration_ms histogram
//	http_request_duration_ms_bucket{le="5"} 100
//	http_request_duration_ms_bucket{le="10"} 200
//	...
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		r.mu.RLock()
		defer r.mu.RUnlock()

		// Counters
		for name, c := range r.counters {
			fmt.Fprintf(w, "# TYPE %s counter\n", name)
			fmt.Fprintf(w, "%s %d\n\n", name, c.Value())
		}

		// Gauges
		for name, g := range r.gauges {
			fmt.Fprintf(w, "# TYPE %s gauge\n", name)
			fmt.Fprintf(w, "%s %d\n\n", name, g.Value())
		}

		// Histograms
		for name, h := range r.histograms {
			fmt.Fprintf(w, "# TYPE %s histogram\n", name)
			var cumulative int64
			for i, boundary := range h.buckets {
				cumulative += h.counts[i].Load()
				fmt.Fprintf(w, "%s_bucket{le=\"%.0f\"} %d\n", name, boundary, cumulative)
			}
			cumulative += h.counts[len(h.buckets)].Load()
			fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, cumulative)
			fmt.Fprintf(w, "%s_count %d\n", name, h.Count())
			fmt.Fprintf(w, "%s_avg %.2f\n", name, h.Avg())
			fmt.Fprintf(w, "%s_p50 %.2f\n", name, h.Percentile(0.50))
			fmt.Fprintf(w, "%s_p95 %.2f\n", name, h.Percentile(0.95))
			fmt.Fprintf(w, "%s_p99 %.2f\n", name, h.Percentile(0.99))
			fmt.Fprintln(w)
		}
	})
}

// DefaultRegistry 全局默认注册中心
var DefaultRegistry = NewRegistry()

// 预定义常用指标
var (
	HTTPRequestsTotal      = DefaultRegistry.Counter("http_requests_total")
	HTTPRequestDuration    = DefaultRegistry.Histogram("http_request_duration_ms", DefaultLatencyBuckets())
	HTTPActiveConnections  = DefaultRegistry.Gauge("http_active_connections")
	RouterRequestsTotal    = DefaultRegistry.Counter("router_requests_total")
	RouterDegradedTotal    = DefaultRegistry.Counter("router_degraded_total")
	RouterLatency          = DefaultRegistry.Histogram("router_latency_ms", DefaultLatencyBuckets())
	IndexerBlocksProcessed = DefaultRegistry.Counter("indexer_blocks_processed")
	IndexerReorgsTotal     = DefaultRegistry.Counter("indexer_reorgs_total")
	CacheHitsTotal         = DefaultRegistry.Counter("cache_hits_total")
	CacheMissesTotal       = DefaultRegistry.Counter("cache_misses_total")
	CircuitBreakerOpen     = DefaultRegistry.Gauge("circuitbreaker_open")
	WorkerPoolQueueUsed    = DefaultRegistry.Gauge("workerpool_queue_used")
)
