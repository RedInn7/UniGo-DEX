package router

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"runtime"
	"sync"
	"time"

	"unigo-dex/indexer"
	"unigo-dex/metrics"
	"unigo-dex/workerpool"
)

// router.go — Smart Order Router（智能路由引擎）
//
// === 核心设计：二级路由降级机制 ===
//
// 在极端行情下（如 ETH 瞬时下跌 30%），大量用户同时发起 Swap，
// 复杂的多跳路径搜索可能导致 CPU 飙升。
// 如果仍坚持最优路径搜索，反而会放大系统风险。
//
// 因此采用 Fail-soft 而非 Fail-fast 策略：
//
//   Level 1（正常模式）：BFS 搜索多跳最优路径
//     - 最大跳数：3
//     - 超时：200ms
//     - 先枚举候选路径，再用 Worker Pool 并发逐 tick 模拟每条路径的产出
//
//   Level 2（降级模式）：直连路径 fallback
//     - 当 Level 1 超时或失败
//     - 自动退回到硬编码的直连路径（如 ETH → USDC 单池）
//     - 牺牲部分价格最优性，优先保证系统可用性
//
// === 背压机制 ===
//
// 使用有界 Channel 限制请求队列长度，避免请求无限堆积：
//   - routeRequestChan 容量 = maxConcurrentRoutes
//   - 超出容量时，新请求直接走降级路径，而非阻塞等待

// RouterConfig 路由引擎配置
type RouterConfig struct {
	// MaxHops 最大跳数（经过的池子数量）
	MaxHops int
	// RouteTimeout 单次路由搜索超时时间
	RouteTimeout time.Duration
	// MaxConcurrentRoutes 最大并发路由搜索数
	MaxConcurrentRoutes int
	// MaxTicksCrossed 触发高 Gas 警告的 tick 穿越阈值
	MaxTicksCrossed int
}

// DefaultRouterConfig 返回默认路由配置
func DefaultRouterConfig() RouterConfig {
	return RouterConfig{
		MaxHops:             3,
		RouteTimeout:        200 * time.Millisecond,
		MaxConcurrentRoutes: 200,
		MaxTicksCrossed:     50,
	}
}

// Route 表示一条交易路径
type Route struct {
	Pools        []indexer.PoolSnapshot // 经过的池子
	Path         []string               // token 路径，如 [WETH, USDC, DAI]
	AmountIn     *big.Int               // 输入数量
	AmountOut    *big.Int               // 预期输出（逐 tick 模拟，与链上 Quoter 一致）
	GasEstimate  uint64                 // 预估 Gas
	TicksCrossed int                    // 穿越的已初始化 tick 数
	IsDegraded   bool                   // 是否为降级路径
}

// Fees 路径上每一跳的费率，用于编码 Manager 的紧凑路径
func (r *Route) Fees() []uint32 {
	fees := make([]uint32, len(r.Pools))
	for i, p := range r.Pools {
		fees[i] = p.Fee
	}
	return fees
}

// Gas 预估参数：每一跳的固定开销 + 每穿越一个已初始化 tick 的额外开销
const (
	gasPerHop       = 150000
	gasPerTickCross = 25000
)

// maxCandidates 单次搜索最多评估的候选路径数
const maxCandidates = 64

// Router 智能路由引擎
type Router struct {
	config       RouterConfig
	stateManager *indexer.PoolStateManager

	// 背压控制：有界 Channel 限制并发数
	workerSem chan struct{}

	// 候选路径的并发评估
	evalPool *workerpool.Pool

	// 统计
	stats   RouterStats
	statsMu sync.Mutex
}

// RouterStats 路由引擎统计
type RouterStats struct {
	TotalRequests    uint64
	DegradedRequests uint64 // 降级到直连的次数
	TimeoutCount     uint64
	AvgLatencyMs     float64
}

// candidate 一条候选路径
type candidate struct {
	pools []indexer.PoolSnapshot
	path  []string
}

// NewRouter 创建路由引擎
func NewRouter(config RouterConfig, stateManager *indexer.PoolStateManager) *Router {
	return &Router{
		config:       config,
		stateManager: stateManager,
		workerSem:    make(chan struct{}, config.MaxConcurrentRoutes),
		evalPool:     workerpool.New(workerpool.Config{WorkerCount: runtime.NumCPU(), QueueSize: 4 * maxCandidates}),
	}
}

// FindBestRoute 查找最优交易路径
//
// 二级降级流程：
//  1. 尝试获取 Worker 信号量（背压控制）
//     - 如果信号量已满，直接走降级路径（避免请求堆积）
//  2. 带超时的多跳搜索（Level 1）
//     - 超时 200ms 后自动取消
//  3. 如果 Level 1 失败/超时，执行 Level 2 降级
//  4. 检查 tick 穿越数量，必要时发出高 Gas 警告
func (r *Router) FindBestRoute(
	ctx context.Context,
	tokenIn, tokenOut string,
	amountIn *big.Int,
) (*Route, error) {
	r.statsMu.Lock()
	r.stats.TotalRequests++
	r.statsMu.Unlock()
	metrics.RouterRequestsTotal.Inc()

	start := time.Now()

	// === 背压控制 ===
	// 尝试非阻塞地获取信号量
	select {
	case r.workerSem <- struct{}{}:
		defer func() { <-r.workerSem }()
	default:
		// 信号量已满，所有 Worker 都在忙
		// 直接走降级路径，避免请求堆积导致系统雪崩
		log.Printf("[Router] ⚠️ Worker Pool 已满（%d/%d），启用降级路由",
			r.config.MaxConcurrentRoutes, r.config.MaxConcurrentRoutes)
		return r.degradedRoute(tokenIn, tokenOut, amountIn, "worker_pool_full")
	}

	// === Level 1：带超时的多跳搜索 ===
	routeCtx, cancel := context.WithTimeout(ctx, r.config.RouteTimeout)
	defer cancel()

	route, err := r.searchRoutes(routeCtx, tokenIn, tokenOut, amountIn)

	elapsed := time.Since(start)
	r.statsMu.Lock()
	r.stats.AvgLatencyMs = (r.stats.AvgLatencyMs*float64(r.stats.TotalRequests-1) + float64(elapsed.Milliseconds())) / float64(r.stats.TotalRequests)
	r.statsMu.Unlock()
	metrics.RouterLatency.ObserveDuration(start)

	if err != nil || route == nil {
		if routeCtx.Err() == context.DeadlineExceeded {
			log.Printf("[Router] ⏰ 路由搜索超时（%v），降级到直连路径", r.config.RouteTimeout)
			r.statsMu.Lock()
			r.stats.TimeoutCount++
			r.statsMu.Unlock()
		}
		// === Level 2：降级路径 ===
		return r.degradedRoute(tokenIn, tokenOut, amountIn, "search_failed")
	}

	// === Gas 预估与警告 ===
	if route.TicksCrossed > r.config.MaxTicksCrossed {
		log.Printf("[Router] ⚠️ 高 Gas 警告：预估穿越 %d 个 tick（阈值 %d），建议拆分订单",
			route.TicksCrossed, r.config.MaxTicksCrossed)
	}

	return route, nil
}

// searchRoutes 搜索最优路径
//
// 两个阶段：
//  1. 枚举：DFS 找出 tokenIn 到 tokenOut 不超过 MaxHops 跳、不重复经过同一 token 的所有路径。
//     同一对 token 的不同费率池算不同路径。只看图结构，很快。
//  2. 评估：每条候选路径逐池逐 tick 模拟产出。这一步是 CPU 密集的大数运算，
//     交给 Worker Pool 并发执行；队列满时在当前协程里直接算，不丢候选。
func (r *Router) searchRoutes(
	ctx context.Context,
	tokenIn, tokenOut string,
	amountIn *big.Int,
) (*Route, error) {
	pools := r.stateManager.GetAllPools()
	if len(pools) == 0 {
		return nil, fmt.Errorf("no pools available")
	}

	candidates := enumeratePaths(pools, tokenIn, tokenOut, r.config.MaxHops)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no path from %s to %s", tokenIn, tokenOut)
	}

	routes := make([]*Route, len(candidates))
	var wg sync.WaitGroup
	for i, c := range candidates {
		evaluate := func() {
			defer wg.Done()
			if ctx.Err() == nil {
				routes[i] = evaluatePath(c, tokenIn, amountIn)
			}
		}
		wg.Add(1)
		if !r.evalPool.Submit(evaluate) {
			evaluate()
		}
	}
	wg.Wait()
	metrics.WorkerPoolQueueUsed.Set(int64(r.evalPool.Stats().QueueUsed))

	var best *Route
	for _, route := range routes {
		if route != nil && (best == nil || route.AmountOut.Cmp(best.AmountOut) > 0) {
			best = route
		}
	}
	if best == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("no path from %s to %s has enough liquidity", tokenIn, tokenOut)
	}
	return best, nil
}

// enumeratePaths DFS 枚举候选路径，最多 maxCandidates 条
func enumeratePaths(pools []indexer.PoolSnapshot, tokenIn, tokenOut string, maxHops int) []candidate {
	var (
		candidates []candidate
		pathPools  []indexer.PoolSnapshot
		path       = []string{tokenIn}
		visited    = map[string]bool{tokenIn: true}
	)

	var dfs func(token string)
	dfs = func(token string) {
		for _, pool := range pools {
			if len(candidates) >= maxCandidates {
				return
			}
			var next string
			switch token {
			case pool.Token0:
				next = pool.Token1
			case pool.Token1:
				next = pool.Token0
			default:
				continue
			}
			if visited[next] {
				continue
			}

			pathPools = append(pathPools, pool)
			path = append(path, next)
			if next == tokenOut {
				candidates = append(candidates, candidate{
					pools: append([]indexer.PoolSnapshot{}, pathPools...),
					path:  append([]string{}, path...),
				})
			} else if len(pathPools) < maxHops {
				visited[next] = true
				dfs(next)
				visited[next] = false
			}
			pathPools = pathPools[:len(pathPools)-1]
			path = path[:len(path)-1]
		}
	}
	dfs(tokenIn)
	return candidates
}

// evaluatePath 逐池模拟一条路径的产出；任何一跳流动性不足或没有价格时返回 nil
func evaluatePath(c candidate, tokenIn string, amountIn *big.Int) *Route {
	amount := amountIn
	token := tokenIn
	crossed := 0
	for i, pool := range c.pools {
		out, result, err := QuoteExactInputSingle(pool, token, amount)
		if err != nil || out.Sign() <= 0 {
			return nil
		}
		amount = out
		token = c.path[i+1]
		crossed += result.TicksCrossed
	}
	return &Route{
		Pools:        c.pools,
		Path:         c.path,
		AmountIn:     amountIn,
		AmountOut:    amount,
		GasEstimate:  uint64(gasPerHop*len(c.pools) + gasPerTickCross*crossed),
		TicksCrossed: crossed,
	}
}

// degradedRoute 降级路径：只在直连池里选
//
// 当 Level 1 搜索失败时的 fallback：
//   - 只查找 tokenIn/tokenOut 的直连池（可能有多个费率档），取产出最高的
//   - 不做多跳搜索
//   - 牺牲价格最优性，保证系统可用性
func (r *Router) degradedRoute(
	tokenIn, tokenOut string,
	amountIn *big.Int,
	reason string,
) (*Route, error) {
	r.statsMu.Lock()
	r.stats.DegradedRequests++
	r.statsMu.Unlock()
	metrics.RouterDegradedTotal.Inc()

	log.Printf("[Router] 🔄 降级路由（原因: %s）：%s -> %s", reason, tokenIn, tokenOut)

	var best *Route
	for _, c := range enumeratePaths(r.stateManager.GetAllPools(), tokenIn, tokenOut, 1) {
		if route := evaluatePath(c, tokenIn, amountIn); route != nil && (best == nil || route.AmountOut.Cmp(best.AmountOut) > 0) {
			best = route
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no route found for %s -> %s (even degraded)", tokenIn, tokenOut)
	}
	best.IsDegraded = true
	return best, nil
}

// FindExactOutputRoute 精确输出的单跳路由：在直连池里找投入最少的那个
//
// 返回的 Route 中 AmountOut 是指定的输出数量，AmountIn 是需要投入的数量。
func (r *Router) FindExactOutputRoute(tokenIn, tokenOut string, amountOut *big.Int) (*Route, error) {
	var best *Route
	for _, c := range enumeratePaths(r.stateManager.GetAllPools(), tokenIn, tokenOut, 1) {
		amountIn, result, err := QuoteExactOutputSingle(c.pools[0], tokenIn, amountOut)
		if err != nil || amountIn.Sign() <= 0 {
			continue
		}
		if best == nil || amountIn.Cmp(best.AmountIn) < 0 {
			best = &Route{
				Pools:        c.pools,
				Path:         c.path,
				AmountIn:     amountIn,
				AmountOut:    amountOut,
				GasEstimate:  uint64(gasPerHop + gasPerTickCross*result.TicksCrossed),
				TicksCrossed: result.TicksCrossed,
			}
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no direct pool can deliver %s of %s", amountOut, tokenOut)
	}
	return best, nil
}

// GetStats 获取路由统计
func (r *Router) GetStats() RouterStats {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	return r.stats
}
