# Go 后端：工程基础设施

> 小节编号沿用原文：1–13 见 [contracts.md](contracts.md)，14–22 见 [backend.md](backend.md)，23–26 见 [market-mev.md](market-mev.md)，27–32 见 [infrastructure.md](infrastructure.md)；测试见 [testing.md](testing.md)，压测见 [benchmark.md](benchmark.md)，部署与 API 见 [getting-started.md](getting-started.md)。

### 27. 断路器（Circuit Breaker）

**问题**：后端的索引、TWAP 读取、链上报价抽检、Gas 费用查询、交易广播都要调用以太坊 RPC 节点。当节点故障时：

```
正常情况：
  请求 → RPC 节点 → 响应 (10ms)

外部服务故障，无断路器：
  请求 → RPC 节点 → 等待超时... (30s) → 失败
  请求 → RPC 节点 → 等待超时... (30s) → 失败
  请求 → RPC 节点 → 等待超时... (30s) → 失败
  ↓
  所有 goroutine 阻塞等待 → 请求堆积 → 内存飙升 → OOM

外部服务故障，有断路器：
  前 5 次失败 → 断路器打开 → 后续请求直接返回错误 (0ms)
  30 秒后 → 断路器半开 → 放行 1 个探测请求
  探测成功 → 断路器关闭 → 恢复正常
  探测失败 → 断路器重新打开 → 继续保护
```

**三态状态机**：

```
         ┌────────────────┐
         │    Closed      │ ← 正常状态
         │ 放行所有请求     │
         │ 统计连续失败次数 │
         └───────┬────────┘
                 │ 连续失败 ≥ MaxFailures (默认 5)
                 ▼
         ┌────────────────┐
         │     Open       │ ← 熔断状态
         │ 拒绝所有请求    │
         │ (快速失败 0ms)  │
         └───────┬────────┘
                 │ 冷却时间到 (默认 30s)
                 ▼
         ┌────────────────┐    探测成功    ┌──────────┐
         │   HalfOpen     │ ─────────────→ │ Closed   │
         │ 放行 1 个探测   │               └──────────┘
         │ 成功则恢复      │    探测失败    ┌──────────┐
         │ 失败则重新熔断  │ ─────────────→ │  Open    │
         └────────────────┘               └──────────┘
```

**实现细节**：

```go
// circuitbreaker.go
func (cb *CircuitBreaker) Execute(fn func() (interface{}, error)) (interface{}, error) {
    cb.mu.Lock()

    switch cb.state {
    case StateOpen:
        if time.Since(cb.lastFailureTime) > cb.config.Timeout {
            // 冷却时间到，转入 HalfOpen 进行探测
            cb.setState(StateHalfOpen)
            cb.halfOpenCount = 0
        } else {
            cb.mu.Unlock()
            return nil, ErrCircuitOpen  // 快速失败，不执行 fn
        }

    case StateHalfOpen:
        if cb.halfOpenCount >= cb.config.HalfOpenMaxRequests {
            cb.mu.Unlock()
            return nil, ErrCircuitOpen  // 探测配额用完
        }
        cb.halfOpenCount++
    }

    cb.mu.Unlock()

    // 执行实际操作（只在 Closed 或 HalfOpen 时执行）
    result, err := fn()

    cb.mu.Lock()
    defer cb.mu.Unlock()
    if err != nil {
        cb.onFailure()  // 累加失败计数，可能触发 Closed→Open
    } else {
        cb.onSuccess()  // 重置计数，HalfOpen→Closed
    }
    return result, err
}
```

**与 RPC 客户端的集成**：

`chain.Client` 的每一次 RPC 调用都经过同一个断路器（第 15 节的 `call`）。断路器状态变化时写日志并更新 `circuitbreaker_open` 指标：

```go
// chain/client.go — NewClient
cfg := circuitbreaker.DefaultConfig()
cfg.OnStateChange = func(from, to circuitbreaker.State) {
    slog.Warn("RPC 断路器状态变化", "from", from.String(), "to", to.String())
    if to == circuitbreaker.StateOpen {
        metrics.CircuitBreakerOpen.Set(1)
    } else {
        metrics.CircuitBreakerOpen.Set(0)
    }
}
```

节点熔断期间各模块各自降级：索引器本轮轮询失败、下一轮重试；Gas 评估退回静态值；TWAP 校验返回 `unavailable` 并提示用户。

**代码位置**：`backend/circuitbreaker/circuitbreaker.go`, `backend/circuitbreaker/circuitbreaker_test.go`, `backend/chain/client.go`

---

### 28. Goroutine 池与性能优化

**问题**：Go 的 goroutine 虽然轻量（初始栈仅 2KB），但在高并发场景下无限制地 `go func()` 仍然有代价：

| 问题 | 原因 | 影响 |
|------|------|------|
| 调度开销 | runtime 调度器管理百万级 G-M-P | CPU 浪费在调度而非计算 |
| 内存压力 | 每个 goroutine 栈会动态增长 | 万级 goroutine × 几 KB = 可观内存 |
| GC 压力 | goroutine 引用的对象延迟回收 | GC STW 时间随 goroutine 数增加 |

**池化方案**：

```
朴素方案：go func()
  ┌─────────┐ ┌─────────┐ ┌─────────┐     ┌─────────┐
  │ Task 1  │ │ Task 2  │ │ Task 3  │ ... │ Task N  │
  │ goroutine│ │ goroutine│ │ goroutine│     │ goroutine│
  └─────────┘ └─────────┘ └─────────┘     └─────────┘
  → N 个任务 = N 个 goroutine（创建/销毁开销）

池化方案：Worker Pool
  ┌────────────────────────────────────────────┐
  │            taskQueue (有界 Channel)          │
  │  [ Task 1 | Task 2 | Task 3 | ... ]        │
  └──────┬──────────┬──────────┬───────────────┘
         │          │          │
    ┌────▼───┐ ┌───▼────┐ ┌──▼─────┐
    │Worker 1│ │Worker 2│ │Worker N│  ← 固定数量，长驻复用
    │(复用)   │ │(复用)   │ │(复用)   │
    └────────┘ └────────┘ └────────┘
  → N 个任务 = 固定 W 个 goroutine（零创建/销毁开销）
```

**核心实现**：

```go
// pool.go — Worker 循环
// 优先从 taskQueue 取任务执行，channel 关闭后自动退出
func (p *Pool) worker(id int) {
    defer p.wg.Done()
    for task := range p.taskQueue {  // channel 关闭时自动退出循环
        task()
        p.completed.Add(1)
    }
}

// 非阻塞提交（队列满时返回 false，调用方决定降级策略）
func (p *Pool) Submit(task Task) bool {
    p.submitted.Add(1)
    select {
    case p.taskQueue <- task:
        return true
    default:
        p.rejected.Add(1)
        return false  // 队列已满，调用方可以：go func() / 降级 / 返回错误
    }
}

// 优雅关闭：先清空队列再退出 Worker
func (p *Pool) Shutdown() {
    close(p.taskQueue)  // 1. 关闭 channel → 停止接受新任务
    p.wg.Wait()         // 2. 等待 Worker 消费完剩余任务
    p.cancel()          // 3. 清理 context
}
```

**泛型支持（Go 1.18+）**：

```go
// 提交任务并等待结果（泛型版本）
// 队列已满、任务被拒绝时在调用方协程里直接执行，否则等结果的一方会永远收不到值
func SubmitAndWaitResult[T any](p *Pool, fn func() T) <-chan T {
    ch := make(chan T, 1)
    if !p.Submit(func() { ch <- fn() }) {
        ch <- fn()
    }
    return ch
}

// 使用示例
result := <-SubmitAndWaitResult(pool, func() int {
    return computeExpensiveResult()
})
```

**在本项目中的使用**：路由引擎用一个 `WorkerCount = runtime.NumCPU()`、队列长度 256 的池并发评估候选路径（第 17 节）。

**Benchmark 实测数据**见 [Benchmark 性能对比](benchmark.md)：10000 个任务时池化方案的内存分配减少 86%、分配次数减半。

**代码位置**：`backend/workerpool/pool.go`, `backend/workerpool/pool_test.go`

---

### 29. 结构化日志与链路追踪

**为什么用 slog 而不是 `log.Printf`？**

| 维度 | log.Printf | slog (Go 1.21+) |
|------|-----------|-----------------|
| 输出格式 | 自由文本，难以机器解析 | JSON / Text，可被 ELK/Loki 直接采集 |
| 日志级别 | 无 | Debug / Info / Warn / Error |
| 结构化字段 | 手动拼接字符串 | 原生键值对 `slog.String("key", val)` |
| 上下文传递 | 不支持 | 通过 context 携带 Trace ID |
| 性能 | 每次调用都格式化 | Handler 接口支持零分配优化 |

**Trace ID 链路追踪**：

一个用户请求可能经过多个模块（API → Router → Indexer → Cache）。通过 context 传递 Trace ID，可以在日志中串联整个链路：

```go
// logging.go — TraceID 注入
func WithTraceID(ctx context.Context) context.Context {
    id := generateTraceID()  // 12 字节随机 hex
    return context.WithValue(ctx, traceIDKey, id)
}

// L(ctx) 获取携带 trace_id 的 Logger
func L(ctx context.Context) *slog.Logger {
    logger := slog.Default()
    if traceID := GetTraceID(ctx); traceID != "" {
        logger = logger.With("trace_id", traceID)
    }
    return logger
}
```

**HTTP 中间件注入 Trace ID**

每个请求带一个 Trace ID：客户端通过 `X-Trace-Id` 请求头传入就沿用（网关、上游服务可以把同一个 ID 串下来），否则生成一个。ID 放进 context，同时写回响应头，客户端报障时可以拿它查日志：

```go
// api/middleware.go
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
```

客户端传入的 ID 只接受 8 ~ 64 位的字母、数字、连字符（`validTraceID`），避免把任意内容写进日志。

请求的 context 一路传到路由引擎，后面所有用 `logging.L(ctx)` 打的日志都带同一个 `trace_id`：访问日志、限流和并发上限的拒绝、路由降级、高 gas 警告。索引器的轮询协程用自己的根 context 打日志；chain / gas / mev / arb / ws 等不在请求路径上的模块直接用 slog 的键值对风格输出关键事件（断路器状态变化、三明治告警、发现套利机会）。

**访问日志**（一次真实运行的输出，`LOG_FORMAT=text`）：

```
time=2026-11-24T23:43:09.890-08:00 level=INFO msg=请求完成 trace_id=my-trace-12345 method=GET path=/health status=200 latency_ms=0.019
time=2026-11-24T23:43:09.865-08:00 level=INFO msg=请求完成 trace_id=cfe731137c41ce4104d6e85c method=GET path=/api/v1/quote status=404 latency_ms=0.061
```

第一行是客户端带着 `X-Trace-Id: my-trace-12345` 发来的请求，第二行的 ID 由服务端生成。在 Grafana / Kibana 中搜索 `trace_id` 即可看到这个请求经过的所有模块的日志。

**配置**：

| 环境变量 | 取值 | 说明 |
|---------|------|------|
| `LOG_LEVEL` | `debug` / `info`（默认）/ `warn` / `error` | 索引器逐事件的处理日志、限流和并发上限的逐条拒绝日志是 debug 级别 |
| `LOG_FORMAT` | `text`（默认）/ `json` | JSON 格式可被 ELK / Loki 直接采集 |
| `ACCESS_LOG` | `off` 关闭 | 关闭逐请求的访问日志 |

逐条日志放在 debug 级别是有原因的：过载时每秒可能拒绝上万个请求，逐条打 warn 日志只会让服务更慢，拒绝数由指标统计。压测里打开访问日志（每个请求一行结构化日志）对吞吐的影响在各轮之间的波动范围之内（见[压测](benchmark.md#压测)）。

**代码位置**：`backend/logging/logging.go`, `backend/api/middleware.go`

---

### 30. Prometheus 风格可观测性指标

**日志告诉你"发生了什么"，指标告诉你"系统状态如何"。**

本实现使用纯标准库实现 Prometheus 文本格式的指标暴露，无需引入 `prometheus/client_golang`。

**三种指标类型**：

```go
// Counter — 只增不减的累计值（适合：请求总数、错误总数）
type Counter struct {
    value atomic.Int64  // 使用 atomic 保证并发安全，零锁开销
}
counter.Inc()     // +1
counter.Add(10)   // +10

// Gauge — 可增可减的瞬时值（适合：当前连接数、队列深度）
type Gauge struct {
    value atomic.Int64
}
gauge.Set(42)     // 设置为 42
gauge.Inc()       // +1
gauge.Dec()       // -1

// Histogram — 值的分布统计（适合：请求延迟）
type Histogram struct {
    buckets []float64      // 桶边界：[1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000]
    counts  []atomic.Int64 // 每个桶的计数
    values  []float64      // 保留最近 10000 个值，用于精确百分位计算
}
histogram.Observe(42.5)                          // 记录一个值
histogram.ObserveDuration(startTime)             // 记录持续时间
histogram.Percentile(0.99)                       // 获取 P99
```

**Histogram 百分位计算原理**：

```
P99 = 99% 的请求在 X ms 内完成

计算方法：
1. 保留最近 N 个观测值（默认 10000）
2. 排序
3. 取第 ceil(0.99 × N) 个值

例如 1000 个观测值：
  排序后 → [0.5, 0.8, 1.2, ..., 42.5, 50.1, 120.3]
  P99 = 第 990 个值 = 50.1ms
  含义："99% 的请求在 50.1ms 内完成"
```

**业务指标**（预注册到全局 Registry，各模块直接使用）：

| 指标 | 类型 | 含义 |
|------|------|------|
| `http_requests_total` | Counter | HTTP 请求总数（含被 429 / 503 拒绝的） |
| `http_request_duration_ms` | Histogram | 请求延迟分布 |
| `http_active_connections` | Gauge | 在途请求数 |
| `http_rate_limited_total` | Counter | 被每 IP 限流拒绝的请求数（429） |
| `http_concurrency_rejected_total` | Counter | 被全局并发上限拒绝的请求数（503） |
| `cache_hits_total` / `cache_misses_total` | Counter | 缓存命中 / 未命中次数 |
| `router_requests_total` | Counter | 路由请求总数 |
| `router_degraded_total` | Counter | 降级到直连路径的次数 |
| `router_latency_ms` | Histogram | 路由搜索延迟 |
| `workerpool_queue_used` | Gauge | 路径评估池的队列占用 |
| `indexer_blocks_processed` | Counter | 已处理的区块数 |
| `indexer_reorgs_total` | Counter | 检测到的 Reorg 次数 |
| `circuitbreaker_open` | Gauge | RPC 断路器是否处于熔断状态 |
| `quote_onchain_checks_total` | Counter | 链上报价抽检次数 |
| `quote_onchain_mismatch_total` | Counter | 抽检中链下与链上不一致的次数 |
| `quote_onchain_deviation_bps` | Histogram | 抽检偏差（基点） |
| `oracle_twap_checks_total` | Counter | TWAP 校验次数 |
| `oracle_twap_deviation_total` | Counter | 现价偏离 TWAP 超阈值的次数 |
| `oracle_twap_unavailable_total` | Counter | TWAP 不可用的次数 |
| `oracle_chainlink_deviation_total` | Counter | 现价偏离 Chainlink 喂价超阈值的次数 |
| `oracle_chainlink_unavailable_total` | Counter | Chainlink 喂价不可用的次数 |
| `mev_sandwich_detected_total` | Counter | 检测到的三明治攻击数 |
| `mev_swaps_rejected_total` | Counter | 被交易前保护拒绝的请求数 |
| `swap_signature_rejected_total` | Counter | EIP-712 验签失败次数 |
| `arb_opportunities_found_total` | Counter | 累计发现的套利机会 |
| `arb_opportunities_active` | Gauge | 当前有效的套利机会数 |
| `stream_clients` | Gauge | SSE 在线客户端数 |
| `stream_dropped_clients_total` | Counter | 因消费过慢被断开的客户端数 |

**`/metrics` 端点输出**（Prometheus 文本格式）：

```
# TYPE router_requests_total counter
router_requests_total 1200

# TYPE router_latency_ms histogram
router_latency_ms_bucket{le="5"} 100
router_latency_ms_bucket{le="10"} 250
router_latency_ms_bucket{le="25"} 800
router_latency_ms_bucket{le="50"} 1100
router_latency_ms_bucket{le="100"} 1180
router_latency_ms_bucket{le="+Inf"} 1200
router_latency_ms_count 1200
router_latency_ms_avg 18.50
router_latency_ms_p50 12.00
router_latency_ms_p95 45.00
router_latency_ms_p99 95.00

# TYPE quote_onchain_mismatch_total counter
quote_onchain_mismatch_total 0
```

Prometheus 定期 scrape 此端点，Grafana 展示面板。集成测试的最后一步也读这个端点：断言 `quote_onchain_mismatch_total` 为 0、`mev_sandwich_detected_total` 为 1，套利和 TWAP 偏离计数大于 0。压测脚本用 `http_requests_total` 在每轮前后的增量核对 k6 发出的请求数，8 个场景全部一致（见[压测](benchmark.md#压测)）。

**代码位置**：`backend/metrics/metrics.go`

---

### 31. API 限流与背压控制

**双层保护**：

```
请求进入
    │
    ▼
┌───────────────────────────────────┐
│ Layer 1: 每 IP 限流 (Token Bucket)│
│                                   │
│ 每个 IP 有一个令牌桶               │
│ 持续速率 = 100 请求/秒             │
│ 突发容量 = 200（桶的大小）         │
│ 每次请求消耗 1 个令牌              │
│                                   │
│ 桶空？→ 429 Too Many Requests     │
│        + Retry-After: 1 头        │
└───────────┬───────────────────────┘
            │ 通过
            ▼
┌───────────────────────────────────┐
│ Layer 2: 全局并发限制 (Semaphore)  │
│                                   │
│ 有界 Channel (capacity=500)       │
│ 非阻塞获取                        │
│                                   │
│ 满了？→ 503 Service Unavailable   │
│        + Retry-After: 1 头        │
└───────────┬───────────────────────┘
            │ 通过
            ▼
      正常处理请求
```

完整的中间件链是 `CORS → Trace → Metrics → Logging → RateLimit → ConcurrencyLimit → Handler`。Metrics 放在两个限制的外层，被 429 / 503 拒绝的请求也计入 `http_requests_total`。

| 环境变量 | 默认值 | 说明 |
|---------|-------|------|
| `RATE_LIMIT_PER_IP` | `100` | 每 IP 的持续速率（请求/秒），`0` = 不限流 |
| `RATE_LIMIT_BURST` | `200` | 每 IP 的突发容量（令牌桶大小），`0` = 与速率相同 |
| `MAX_CONCURRENT` | `500` | 全局并发上限，`0` = 不限 |

两个限制都可以设为 0 关闭：压测吞吐上限、或由前置网关负责限流时使用。

**Token Bucket 算法**：

速率 `rate` 请求/秒对应每 `1/rate` 秒补 1 个令牌，桶里最多攒 `burst` 个：

```go
// api/server.go — Handler
handler = NewRateLimiter(burst, time.Second/time.Duration(rate)).Middleware(handler)

// api/middleware.go — Allow
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
    return false  // 桶空，拒绝
}
bucket.tokens--
return true
```

“零头留到下次”这一行决定了持续速率准不准：如果每次补充都把 `lastTime` 直接设成当前时间，不足一个令牌的时间就被丢掉了，请求越密，实际速率比配置值低得越多。

**按 IP 而不是按连接限流**：`r.RemoteAddr` 是 `IP:端口`，每个连接的端口都不同，直接拿它当 key 等于没限流。中间件先用 `net.SplitHostPort` 去掉端口。不活跃的桶每 5 分钟清理一次（10 分钟未活跃），防止内存泄漏。

**为什么 503 而不是排队？**

排队意味着请求在内存中等待，极端情况下等待队列可以无限增长导致 OOM。直接 503 + `Retry-After` 让客户端自行重试，是更安全的策略。

**压测验证**（100 并发、20 秒、所有请求来自同一个 IP，见[压测](benchmark.md#压测)）：

| 配置 | 结果 |
|------|------|
| 每 IP 100 请求/秒、突发 200 | 825,061 个请求中放行 2,200 个，正好是 200（突发）+ 100 × 20（持续）；其余 822,861 个返回 429，与 `http_rate_limited_total` 的增量一致 |
| 限流关闭、全局并发上限 20 | 698,277 个请求中 592,976 个立即返回 503，与 `http_concurrency_rejected_total` 的增量一致；全部响应的 p99 为 17.66 ms |

**测试**：`middleware_test.go` 覆盖按 IP 而非按连接限流、持续速率、突发后回到持续速率、两个限制都可以关闭；`server_test.go` 覆盖 Trace ID 的沿用与生成（`TestTraceIDPropagation`）以及 HTTP 和缓存指标（`TestHTTPAndCacheMetrics`）。

**代码位置**：`backend/api/middleware.go`, `backend/api/server.go`（`Handler`）

---

### 32. 优雅关闭与生命周期管理

**启动顺序（依赖图）**：

```
0. logging.Init（LOG_LEVEL / LOG_FORMAT）
        ↓
1. PoolStateManager（无依赖）
        ↓
2. 连接链节点 chain.Dial（连不上或未配置则用模拟源）
        ↓
3. Oracle / Gas Estimator / K 线 / MEV Guard / 套利扫描 / SSE Hub
   （订阅 Swap 事件流的模块，回调必须在索引器启动前接好）
        ↓
4. Indexer（注册订阅者后启动轮询）
        ↓
5. Cache
        ↓
6. Router（依赖 PoolStateManager）
        ↓
7. EIP-712 Verifier + Chainlink 喂价读取器 + API Server（依赖所有上游）
```

`chain.Dial` 在连接后做一次自检：按最新区块头本地复算区块哈希，再用这个哈希向节点查回区块。个别节点返回的区块头字段不全时，本地复算的哈希会和节点自己的对不上，父哈希校验会一直误报 Reorg，所以启动时先确认一次。

**关闭顺序**：

```go
// main.go — 优雅关闭
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
<-sigCh  // 阻塞等待退出信号

// 1. 取消根 context：停止索引器，并让 Hub 断开所有 SSE 连接
//    否则长连接会拖住 HTTP 服务器的关闭
cancel()

// 2. 关闭 API Server：停止接受新请求，等待处理中的请求完成，最多 10 秒
shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
defer shutdownCancel()
server.Shutdown(shutdownCtx)
```

**关键原则**：

1. **先断开长连接**：SSE 连接永远不会自己结束，`server.Shutdown` 会一直等它们，所以先让 Hub 主动断开
2. **等待处理中的请求完成**：`server.Shutdown(ctx)` 会等待活跃连接处理完毕
3. **超时保护**：10 秒后强制退出，防止某些连接永不关闭
4. **所有后台协程都监听同一个根 context**：索引器轮询、Hub 通过 `ctx.Done()` 退出

**代码位置**：`backend/main.go`

