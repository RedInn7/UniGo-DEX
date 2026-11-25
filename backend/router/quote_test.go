package router

import (
	"math/big"
	"math/rand"
	"sort"
	"testing"

	"unigo-dex/indexer"
	umath "unigo-dex/math"
)

// bitmapNext 按 TickBitmap.sol 的写法用位图实现"下一个已初始化 tick"，作为对照
func bitmapNext(bitmap map[int32]*big.Int, tick, tickSpacing int32, lte bool) (int32, bool) {
	compressed := tick / tickSpacing
	if tick < 0 && tick%tickSpacing != 0 {
		compressed--
	}
	word := func(pos int32) *big.Int {
		if w, ok := bitmap[pos]; ok {
			return w
		}
		return new(big.Int)
	}
	if lte {
		wordPos, bitPos := compressed>>8, compressed&255
		for b := bitPos; b >= 0; b-- {
			if word(wordPos).Bit(int(b)) == 1 {
				return (compressed - (bitPos - b)) * tickSpacing, true
			}
		}
		return (compressed - bitPos) * tickSpacing, false
	}
	start := compressed + 1
	wordPos, bitPos := start>>8, start&255
	for b := bitPos; b < 256; b++ {
		if word(wordPos).Bit(int(b)) == 1 {
			return (start + b - bitPos) * tickSpacing, true
		}
	}
	return (start + 255 - bitPos) * tickSpacing, false
}

func TestNextInitializedTickMatchesBitmap(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, spacing := range []int32{10, 60, 200} {
		bitmap := map[int32]*big.Int{}
		seen := map[int32]bool{}
		var ticks []indexer.TickLiquidity
		for i := 0; i < 60; i++ {
			compressed := int32(rng.Intn(4000) - 2000)
			if seen[compressed] {
				continue
			}
			seen[compressed] = true
			ticks = append(ticks, indexer.TickLiquidity{Tick: compressed * spacing, LiquidityNet: big.NewInt(1)})
			w, ok := bitmap[compressed>>8]
			if !ok {
				w = new(big.Int)
				bitmap[compressed>>8] = w
			}
			w.SetBit(w, int(compressed&255), 1)
		}
		sort.Slice(ticks, func(i, j int) bool { return ticks[i].Tick < ticks[j].Tick })

		for i := 0; i < 20000; i++ {
			tick := int32(rng.Intn(4400*int(spacing)) - 2200*int(spacing))
			for _, lte := range []bool{true, false} {
				wantTick, wantInit := bitmapNext(bitmap, tick, spacing, lte)
				gotTick, net := nextInitializedTickWithinOneWord(ticks, tick, spacing, lte)
				if gotTick != wantTick || (net != nil) != wantInit {
					t.Fatalf("spacing=%d tick=%d lte=%v: got (%d, %v), want (%d, %v)",
						spacing, tick, lte, gotTick, net != nil, wantTick, wantInit)
				}
			}
		}
	}
}

// testPool 价格 1:1 的池子，[-600, 600] 上有 1e21 流动性，[-60, 60] 上再叠 4e21
func testPool() indexer.PoolSnapshot {
	p := indexer.NewPoolState("0xpool", "0xa", "0xb", 3000, 60)
	p.ApplyInitialize(&indexer.InitializeEvent{SqrtPriceX96: new(big.Int).Set(umath.Q96), Tick: 0})
	wide, _ := new(big.Int).SetString("1000000000000000000000", 10)
	narrow, _ := new(big.Int).SetString("4000000000000000000000", 10)
	p.ApplyMint(&indexer.MintEvent{TickLower: -600, TickUpper: 600, Amount: wide})
	p.ApplyMint(&indexer.MintEvent{TickLower: -60, TickUpper: 60, Amount: narrow})
	return p.Snapshot()
}

func TestSimulateSwapCrossesTicks(t *testing.T) {
	pool := testPool()
	if pool.Liquidity.String() != "5000000000000000000000" {
		t.Fatalf("active liquidity = %s", pool.Liquidity)
	}

	// 小额：不出 [-60, 60]，不穿越 tick
	small := big.NewInt(1e18)
	out, res, err := QuoteExactInputSingle(pool, "0xa", small)
	if err != nil {
		t.Fatal(err)
	}
	if res.TicksCrossed != 0 || res.Liquidity.Cmp(pool.Liquidity) != 0 || res.Tick >= 0 || res.Tick < -60 {
		t.Fatalf("small swap: %+v", res)
	}
	if res.Amount0.Cmp(small) != 0 || new(big.Int).Neg(res.Amount1).Cmp(out) != 0 {
		t.Fatalf("amounts: %s %s", res.Amount0, res.Amount1)
	}

	// 大额：推过 -60，窄区间的流动性退出
	large, _ := new(big.Int).SetString("30000000000000000000", 10) // 30e18
	_, res, err = QuoteExactInputSingle(pool, "0xa", large)
	if err != nil {
		t.Fatal(err)
	}
	if res.TicksCrossed != 1 || res.Liquidity.String() != "1000000000000000000000" || res.Tick >= -60 {
		t.Fatalf("large swap: crossed=%d liquidity=%s tick=%d", res.TicksCrossed, res.Liquidity, res.Tick)
	}

	// 反方向对称：推过 +60
	_, res, err = QuoteExactInputSingle(pool, "0xb", large)
	if err != nil {
		t.Fatal(err)
	}
	if res.TicksCrossed != 1 || res.Liquidity.String() != "1000000000000000000000" || res.Tick < 60 {
		t.Fatalf("large reverse swap: crossed=%d liquidity=%s tick=%d", res.TicksCrossed, res.Liquidity, res.Tick)
	}

	// 超出全部流动性：报流动性不足，而不是给一个部分成交的报价
	huge, _ := new(big.Int).SetString("1000000000000000000000000", 10)
	if _, _, err := QuoteExactInputSingle(pool, "0xa", huge); err != ErrInsufficientLiquidity {
		t.Fatalf("err = %v, want ErrInsufficientLiquidity", err)
	}
}

func TestExactOutputInvertsExactInput(t *testing.T) {
	pool := testPool()
	amountIn, _ := new(big.Int).SetString("30000000000000000000", 10)
	out, _, err := QuoteExactInputSingle(pool, "0xa", amountIn)
	if err != nil {
		t.Fatal(err)
	}
	// 要拿到同样的输出，需要的输入不超过原输入，且只差取整误差
	in, res, err := QuoteExactOutputSingle(pool, "0xa", out)
	if err != nil {
		t.Fatal(err)
	}
	diff := new(big.Int).Sub(amountIn, in)
	if diff.Sign() < 0 || diff.Cmp(big.NewInt(1000)) > 0 {
		t.Fatalf("exact output needs %s, exact input used %s", in, amountIn)
	}
	if new(big.Int).Neg(res.Amount1).Cmp(out) != 0 {
		t.Fatalf("amount1 = %s, want -%s", res.Amount1, out)
	}
}

func TestSimulateSwapPriceLimit(t *testing.T) {
	pool := testPool()
	limit, _ := umath.GetSqrtRatioAtTick(-30)
	huge, _ := new(big.Int).SetString("1000000000000000000000000", 10)

	res, err := SimulateSwap(pool, true, huge, limit)
	if err != nil {
		t.Fatal(err)
	}
	if res.SqrtPriceX96.Cmp(limit) != 0 || res.AmountRemaining.Sign() <= 0 {
		t.Fatalf("should stop at the limit with input left: price=%s remaining=%s", res.SqrtPriceX96, res.AmountRemaining)
	}
	// 限价在错误的一侧
	if _, err := SimulateSwap(pool, false, huge, limit); err != ErrPriceLimit {
		t.Fatalf("err = %v, want ErrPriceLimit", err)
	}
}
