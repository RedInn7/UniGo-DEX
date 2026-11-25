package router

import (
	"context"
	"math/big"
	"testing"

	"unigo-dex/indexer"
)

func q96(mul int64) *big.Int { return new(big.Int).Lsh(big.NewInt(mul), 96) }

func addPool(m *indexer.PoolStateManager, addr, token0, token1 string) {
	liquidity, _ := new(big.Int).SetString("1000000000000000000000000", 10) // 1e24
	p := indexer.NewPoolState(addr, token0, token1, 3000, 60)
	p.ApplySwap(&indexer.SwapEvent{SqrtPriceX96: q96(1), Liquidity: liquidity, Tick: 0})
	m.AddPool(p)
}

func TestRouteOutputFollowsPoolPrice(t *testing.T) {
	state := indexer.NewPoolStateManager()
	addPool(state, "0xp1", "0xa", "0xb")
	addPool(state, "0xp2", "0xb", "0xc")
	r := NewRouter(DefaultRouterConfig(), state)

	amountIn := big.NewInt(1e18)
	for _, tc := range []struct {
		in, out string
		hops    int
		lo, hi  int64 // 产出范围：每跳扣 0.3% 手续费，再有极小的价格冲击
	}{
		{"0xa", "0xb", 1, 996e15, 997e15},
		{"0xb", "0xa", 1, 996e15, 997e15},
		{"0xa", "0xc", 2, 993e15, 994009e12},
	} {
		route, err := r.FindBestRoute(context.Background(), tc.in, tc.out, amountIn)
		if err != nil {
			t.Fatalf("%s->%s: %v", tc.in, tc.out, err)
		}
		if len(route.Pools) != tc.hops {
			t.Fatalf("%s->%s: %d hops, want %d", tc.in, tc.out, len(route.Pools), tc.hops)
		}
		if route.AmountOut.Cmp(big.NewInt(tc.lo)) <= 0 || route.AmountOut.Cmp(big.NewInt(tc.hi)) >= 0 {
			t.Fatalf("%s->%s: amountOut = %s, want in (%d, %d)", tc.in, tc.out, route.AmountOut, tc.lo, tc.hi)
		}
	}
}

func TestRouteZeroOutputWithoutPrice(t *testing.T) {
	state := indexer.NewPoolStateManager()
	state.AddPool(indexer.NewPoolState("0xp1", "0xa", "0xb", 3000, 60)) // 没有价格和流动性
	r := NewRouter(DefaultRouterConfig(), state)

	route, err := r.FindBestRoute(context.Background(), "0xa", "0xb", big.NewInt(1e18))
	if err != nil {
		t.Fatal(err)
	}
	if route.AmountOut.Sign() != 0 {
		t.Fatalf("amountOut = %s, want 0", route.AmountOut)
	}
}
