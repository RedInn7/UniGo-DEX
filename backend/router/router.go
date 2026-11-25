package router

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"unigo-dex/indexer"
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
//     - 并发 Worker Pool 限制
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
//   - 配合 sync.Pool 复用搜索过程中的临时对象，降低 GC 压力

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
	AmountOut    *big.Int               // 预期输出
	GasEstimate  uint64                 // 预估 Gas
	TicksCrossed int                    // 预估穿越的 tick 数量
	IsDegraded   bool                   // 是否为降级路径
}

// Router 智能路由引擎
type Router struct {
	config       RouterConfig
	stateManager *indexer.PoolStateManager

	// 背压控制：有界 Channel 限制并发数
	workerSem chan struct{}

	// sync.Pool 复用搜索中的临时对象，降低 GC 压力
	searchStatePool sync.Pool

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

// searchState 路由搜索的临时状态（通过 sync.Pool 复用）
type searchState struct {
	visited map[string]bool
	queue   []searchNode
}

type searchNode struct {
	token string
	pools []indexer.PoolSnapshot
	path  []string
	depth int
}

// NewRouter 创建路由引擎
func NewRouter(config RouterConfig, stateManager *indexer.PoolStateManager) *Router {
	return &Router{
		config:       config,
		stateManager: stateManager,
		workerSem:    make(chan struct{}, config.MaxConcurrentRoutes),
		searchStatePool: sync.Pool{
			New: func() interface{} {
				return &searchState{
					visited: make(map[string]bool),
					queue:   make([]searchNode, 0, 32),
				}
			},
		},
	}
}

// FindBestRoute 查找最优交易路径
//
// 二级降级流程：
//  1. 尝试获取 Worker 信号量（背压控制）
//     - 如果信号量已满，直接走降级路径（避免请求堆积）
//  2. 带超时的 BFS 搜索（Level 1）
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

	// === Level 1：带超时的 BFS 搜索 ===
	routeCtx, cancel := context.WithTimeout(ctx, r.config.RouteTimeout)
	defer cancel()

	route, err := r.searchRoutes(routeCtx, tokenIn, tokenOut, amountIn)

	elapsed := time.Since(start)
	r.statsMu.Lock()
	r.stats.AvgLatencyMs = (r.stats.AvgLatencyMs*float64(r.stats.TotalRequests-1) + float64(elapsed.Milliseconds())) / float64(r.stats.TotalRequests)
	r.statsMu.Unlock()

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

// searchRoutes BFS 搜索最优路径
//
// 搜索策略：
//  1. 从 tokenIn 出发，BFS 遍历所有可达的池子
//  2. 每一跳检查是否到达 tokenOut
//  3. 通过 context 超时控制搜索时间
//  4. 使用 sync.Pool 复用搜索状态，减少 GC 压力
func (r *Router) searchRoutes(
	ctx context.Context,
	tokenIn, tokenOut string,
	amountIn *big.Int,
) (*Route, error) {
	// 从 sync.Pool 获取搜索状态
	state := r.searchStatePool.Get().(*searchState)
	defer func() {
		// 重置并归还到 Pool
		for k := range state.visited {
			delete(state.visited, k)
		}
		state.queue = state.queue[:0]
		r.searchStatePool.Put(state)
	}()

	pools := r.stateManager.GetAllPools()
	if len(pools) == 0 {
		return nil, fmt.Errorf("no pools available")
	}

	// BFS 初始化
	state.queue = append(state.queue, searchNode{
		token: tokenIn,
		path:  []string{tokenIn},
		depth: 0,
	})
	state.visited[tokenIn] = true

	var bestRoute *Route

	for len(state.queue) > 0 {
		// 检查超时
		select {
		case <-ctx.Done():
			return bestRoute, ctx.Err()
		default:
		}

		node := state.queue[0]
		state.queue = state.queue[1:]

		if node.depth >= r.config.MaxHops {
			continue
		}

		// 遍历所有池子，寻找与当前 token 相关的
		for _, pool := range pools {
			var nextToken string
			if pool.Token0 == node.token {
				nextToken = pool.Token1
			} else if pool.Token1 == node.token {
				nextToken = pool.Token0
			} else {
				continue
			}

			if state.visited[nextToken] && nextToken != tokenOut {
				continue
			}

			newPath := make([]string, len(node.path)+1)
			copy(newPath, node.path)
			newPath[len(node.path)] = nextToken

			newPools := make([]indexer.PoolSnapshot, len(node.pools)+1)
			copy(newPools, node.pools)
			newPools[len(node.pools)] = pool

			if nextToken == tokenOut {
				// 找到一条路径
				route := &Route{
					Pools:    newPools,
					Path:     newPath,
					AmountIn: amountIn,
					AmountOut:    r.estimateOutput(newPools, tokenIn, amountIn),
					GasEstimate:  uint64(150000 * len(newPools)),
					TicksCrossed: len(newPools) * 10, // 简化估算
				}

				if bestRoute == nil || route.AmountOut.Cmp(bestRoute.AmountOut) > 0 {
					bestRoute = route
				}
			} else {
				state.visited[nextToken] = true
				state.queue = append(state.queue, searchNode{
					token: nextToken,
					pools: newPools,
					path:  newPath,
					depth: node.depth + 1,
				})
			}
		}
	}

	return bestRoute, nil
}

// degradedRoute 降级路径：直接查找直连池
//
// 当 Level 1 搜索失败时的 fallback：
//   - 只查找 tokenIn/tokenOut 的直连池
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

	log.Printf("[Router] 🔄 降级路由（原因: %s）：%s -> %s", reason, tokenIn, tokenOut)

	pools := r.stateManager.GetAllPools()
	for _, pool := range pools {
		if (pool.Token0 == tokenIn && pool.Token1 == tokenOut) ||
			(pool.Token1 == tokenIn && pool.Token0 == tokenOut) {
			return &Route{
				Pools:        []indexer.PoolSnapshot{pool},
				Path:         []string{tokenIn, tokenOut},
				AmountIn:     amountIn,
				AmountOut:    r.estimateOutput([]indexer.PoolSnapshot{pool}, tokenIn, amountIn),
				GasEstimate:  150000,
				TicksCrossed: 10,
				IsDegraded:   true,
			}, nil
		}
	}

	return nil, fmt.Errorf("no route found for %s -> %s (even degraded)", tokenIn, tokenOut)
}

// estimateOutput 估算路径的输出数量
//
// 沿路径逐池调用 SimulateSwap，用与链上相同的定点数公式计算每一跳的产出。
func (r *Router) estimateOutput(pools []indexer.PoolSnapshot, tokenIn string, amountIn *big.Int) *big.Int {
	if len(pools) == 0 || amountIn.Sign() <= 0 {
		return big.NewInt(0)
	}

	current := amountIn
	token := tokenIn
	for _, pool := range pools {
		// 没有流动性或还没有价格（尚未发生过 swap）的池子无法报价
		if pool.Liquidity.Sign() == 0 || pool.SqrtPriceX96.Sign() == 0 {
			return big.NewInt(0)
		}
		zeroForOne := token == pool.Token0
		current, _, _, _ = SimulateSwap(pool.SqrtPriceX96, pool.Liquidity, pool.Tick, pool.Fee, current, zeroForOne)
		if zeroForOne {
			token = pool.Token1
		} else {
			token = pool.Token0
		}
	}
	return current
}

// GetStats 获取路由统计
func (r *Router) GetStats() RouterStats {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	return r.stats
}
