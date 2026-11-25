package router

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"testing"

	"unigo-dex/indexer"
	umath "unigo-dex/math"
)

// bench_test.go — 路由与逐 tick 模拟的基准
//
//	go test -p 2 -run '^$' -bench . -benchmem ./router

// ladderPool 价格 1:1 的池子：一个宽区间打底，再叠 steps 个下界依次为 -60、-120、... 的区间。
// token0 换 token1 时价格下行，每走过 60 个 tick 就穿越一个已初始化的 tick。
func ladderPool(addr, token0, token1 string, fee uint32, steps int) *indexer.PoolState {
	liquidity, _ := new(big.Int).SetString("1000000000000000000000000", 10) // 1e24
	p := indexer.NewPoolState(addr, token0, token1, fee, 60)
	p.ApplyInitialize(&indexer.InitializeEvent{SqrtPriceX96: new(big.Int).Set(umath.Q96), Tick: 0})
	p.ApplyMint(&indexer.MintEvent{TickLower: -60000, TickUpper: 60000, Amount: liquidity})
	for k := 1; k <= steps; k++ {
		p.ApplyMint(&indexer.MintEvent{TickLower: int32(-60 * k), TickUpper: 60, Amount: liquidity})
	}
	return p
}

// amountCrossing 二分找一个恰好穿越 n 个已初始化 tick 的输入数量
func amountCrossing(tb testing.TB, pool indexer.PoolSnapshot, n int) *big.Int {
	lo, hi := big.NewInt(1), new(big.Int).Lsh(big.NewInt(1), 90)
	for lo.Cmp(hi) < 0 {
		mid := new(big.Int).Add(lo, hi)
		mid.Rsh(mid, 1)
		_, res, err := QuoteExactInputSingle(pool, pool.Token0, mid)
		if err != nil || res.TicksCrossed >= n {
			hi = mid
		} else {
			lo = new(big.Int).Add(mid, big.NewInt(1))
		}
	}
	// 再往里走一点，离下一个 tick 还远
	amount := new(big.Int).Mul(lo, big.NewInt(101))
	amount.Div(amount, big.NewInt(100))
	if _, res, err := QuoteExactInputSingle(pool, pool.Token0, amount); err != nil || res.TicksCrossed != n {
		tb.Fatalf("找不到恰好穿越 %d 个 tick 的数量（实际 %d，err=%v）", n, res.TicksCrossed, err)
	}
	return amount
}

// BenchmarkSimulateSwap 单池逐 tick 模拟：不跨 tick / 跨 1、5、20 个已初始化的 tick
func BenchmarkSimulateSwap(b *testing.B) {
	pool := ladderPool("0xpool", "0xa", "0xb", 3000, 30).Snapshot()
	for _, crossings := range []int{0, 1, 5, 20} {
		amount := big.NewInt(1e18)
		if crossings > 0 {
			amount = amountCrossing(b, pool, crossings)
		}
		b.Run(fmt.Sprintf("cross=%d", crossings), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, res, err := QuoteExactInputSingle(pool, "0xa", amount); err != nil || res.TicksCrossed != crossings {
					b.Fatalf("crossed %d, err %v", res.TicksCrossed, err)
				}
			}
		})
	}
}

// layeredState 四层 token（t0 → t1 → t2 → t3），相邻两层之间各有 widths[i] 个池子，
// t0 到 t3 恰好有 widths[0] × widths[1] × widths[2] 条三跳路径
func layeredState(widths [3]int) *indexer.PoolStateManager {
	state := indexer.NewPoolStateManager()
	tokens := []string{"0xt0", "0xt1", "0xt2", "0xt3"}
	for layer, width := range widths {
		for i := 0; i < width; i++ {
			addr := fmt.Sprintf("0xpool-%d-%d", layer, i)
			// 费率各不相同，各条路径的产出也不同
			state.AddPool(ladderPool(addr, tokens[layer], tokens[layer+1], uint32(500+100*i), 8))
		}
	}
	return state
}

// BenchmarkRouteSearch 三跳路由搜索：候选路径串行评估 对比 Worker Pool 并发评估
//
// 每条候选路径要逐池逐 tick 模拟三次 swap（这里每次穿越 2 个 tick）。
func BenchmarkRouteSearch(b *testing.B) {
	for _, widths := range [][3]int{{2, 2, 2}, {4, 4, 2}, {4, 4, 4}} {
		state := layeredState(widths)
		r := NewRouter(DefaultRouterConfig(), state)
		pools := state.GetAllPools()
		candidates := enumeratePaths(pools, "0xt0", "0xt3", 3)
		amount := amountCrossing(b, candidates[0].pools[0], 2)
		name := fmt.Sprintf("candidates=%d", len(candidates))

		b.Run(name+"/serial", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var best *Route
				for _, c := range enumeratePaths(state.GetAllPools(), "0xt0", "0xt3", 3) {
					if route := evaluatePath(c, "0xt0", amount); route != nil && (best == nil || route.AmountOut.Cmp(best.AmountOut) > 0) {
						best = route
					}
				}
				if best == nil {
					b.Fatal("no route")
				}
			}
		})
		b.Run(name+"/workerpool", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if route, err := r.searchRoutes(context.Background(), "0xt0", "0xt3", amount); err != nil || route == nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRouteSearchConcurrent 多个请求同时搜索路由（模拟并发报价），32 条候选路径
func BenchmarkRouteSearchConcurrent(b *testing.B) {
	state := layeredState([3]int{4, 4, 2})
	r := NewRouter(DefaultRouterConfig(), state)
	candidates := enumeratePaths(state.GetAllPools(), "0xt0", "0xt3", 3)
	amount := amountCrossing(b, candidates[0].pools[0], 2)

	b.ReportAllocs()
	var once sync.Once
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if route, err := r.searchRoutes(context.Background(), "0xt0", "0xt3", amount); err != nil || route == nil {
				once.Do(func() { b.Error(err) })
			}
		}
	})
}
