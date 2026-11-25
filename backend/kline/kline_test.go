package kline

import (
	"fmt"
	"math/big"
	"reflect"
	"sync"
	"testing"

	"unigo-dex/indexer"
)

const (
	pool = "0xpool"
	// base 对齐到整天，方便推算各周期的起始时间
	base = int64(19676 * 86400)
)

// swap 构造一笔 Swap：sqrtPriceX96 = sqrtPrice * 2^96，成交后价格为 sqrtPrice^2
func swap(block uint64, ts int64, sqrtPrice int64, amount0, amount1 int64) *indexer.SwapEvent {
	return &indexer.SwapEvent{
		PoolAddress:  pool,
		Amount0:      big.NewInt(amount0),
		Amount1:      big.NewInt(amount1),
		SqrtPriceX96: new(big.Int).Lsh(big.NewInt(sqrtPrice), 96),
		BlockNumber:  block,
		Timestamp:    uint64(ts),
		TxHash:       fmt.Sprintf("0xtx%d-%d", block, ts),
	}
}

func klines(t *testing.T, a *Aggregator, interval string) []Candle {
	t.Helper()
	candles, err := a.Klines(pool, interval, 0)
	if err != nil {
		t.Fatal(err)
	}
	return candles
}

func TestAggregateAcrossIntervals(t *testing.T) {
	a := NewAggregator(DefaultConfig())
	a.OnSwap(swap(1, base+10, 1, 100, -90)) // 价格 1
	a.OnSwap(swap(2, base+50, 2, -30, 40))  // 价格 4，同一分钟
	a.OnSwap(swap(3, base+70, 3, 10, -5))   // 价格 9，下一分钟
	a.OnSwap(swap(4, base+400, 2, 20, -10)) // 价格 4，下一个 5 分钟
	a.OnSwap(swap(5, base+3700, 1, 1, -1))  // 价格 1，下一个小时

	want := map[string][]Candle{
		"1m": {
			{OpenTime: base, Open: 1, High: 4, Low: 1, Close: 4, Volume0: 130, Volume1: 130, Trades: 2},
			{OpenTime: base + 60, Open: 9, High: 9, Low: 9, Close: 9, Volume0: 10, Volume1: 5, Trades: 1},
			{OpenTime: base + 360, Open: 4, High: 4, Low: 4, Close: 4, Volume0: 20, Volume1: 10, Trades: 1},
			{OpenTime: base + 3660, Open: 1, High: 1, Low: 1, Close: 1, Volume0: 1, Volume1: 1, Trades: 1},
		},
		"5m": {
			{OpenTime: base, Open: 1, High: 9, Low: 1, Close: 9, Volume0: 140, Volume1: 135, Trades: 3},
			{OpenTime: base + 300, Open: 4, High: 4, Low: 4, Close: 4, Volume0: 20, Volume1: 10, Trades: 1},
			{OpenTime: base + 3600, Open: 1, High: 1, Low: 1, Close: 1, Volume0: 1, Volume1: 1, Trades: 1},
		},
		"15m": {
			{OpenTime: base, Open: 1, High: 9, Low: 1, Close: 4, Volume0: 160, Volume1: 145, Trades: 4},
			{OpenTime: base + 3600, Open: 1, High: 1, Low: 1, Close: 1, Volume0: 1, Volume1: 1, Trades: 1},
		},
		"1h": {
			{OpenTime: base, Open: 1, High: 9, Low: 1, Close: 4, Volume0: 160, Volume1: 145, Trades: 4},
			{OpenTime: base + 3600, Open: 1, High: 1, Low: 1, Close: 1, Volume0: 1, Volume1: 1, Trades: 1},
		},
		"4h": {{OpenTime: base, Open: 1, High: 9, Low: 1, Close: 1, Volume0: 161, Volume1: 146, Trades: 5}},
		"1d": {{OpenTime: base, Open: 1, High: 9, Low: 1, Close: 1, Volume0: 161, Volume1: 146, Trades: 5}},
	}
	for interval, w := range want {
		if got := klines(t, a, interval); !reflect.DeepEqual(got, w) {
			t.Errorf("%s:\n got  %+v\n want %+v", interval, got, w)
		}
	}

	// limit 取最近的 N 根
	last, _ := a.Klines(pool, "1m", 2)
	if len(last) != 2 || last[1].OpenTime != base+3660 {
		t.Errorf("limit=2: %+v", last)
	}
	if _, err := a.Klines(pool, "2m", 10); err != ErrInvalidInterval {
		t.Errorf("err = %v, want ErrInvalidInterval", err)
	}
	if got, _ := a.Klines("0xunknown", "1m", 10); len(got) != 0 {
		t.Errorf("unknown pool: %+v", got)
	}
}

func TestTrades(t *testing.T) {
	a := NewAggregator(DefaultConfig())
	a.OnSwap(swap(1, base+10, 1, 100, -90))
	a.OnSwap(swap(2, base+50, 2, -30, 40))

	trades := a.Trades(pool, 10)
	if len(trades) != 2 {
		t.Fatalf("got %d trades", len(trades))
	}
	// 最新的在前
	if trades[0].BlockNumber != 2 || trades[0].Price != 4 || trades[0].Amount0 != "-30" || trades[0].ZeroForOne {
		t.Errorf("trades[0] = %+v", trades[0])
	}
	if trades[1].Amount1 != "-90" || !trades[1].ZeroForOne || trades[1].Timestamp != base+10 {
		t.Errorf("trades[1] = %+v", trades[1])
	}
	if got := a.Trades(pool, 1); len(got) != 1 || got[0].BlockNumber != 2 {
		t.Errorf("limit=1: %+v", got)
	}
}

// Reorg 之后的 K 线必须和"从一开始就只见过幸存成交"的聚合结果完全一致
func TestReorgRecomputesCandles(t *testing.T) {
	survivors := []*indexer.SwapEvent{
		swap(8, base-200, 2, 7, -7),   // 上一个 5 分钟周期（也是上一天），不受影响
		swap(9, base+5, 1, 100, -90),  // 与被回滚成交同一分钟
		swap(10, base+10, 2, 50, -40), // 同一分钟
	}
	orphaned := []*indexer.SwapEvent{
		swap(11, base+20, 5, 1000, -900), // 同一分钟内的最高价，回滚后不能残留
		swap(12, base+70, 3, 10, -5),     // 下一分钟
		swap(13, base+4000, 4, 10, -5),   // 下一个小时
	}
	replacement := swap(11, base+25, 3, 60, -50) // 新链上区块 11 的成交

	a := NewAggregator(DefaultConfig())
	for _, e := range append(append([]*indexer.SwapEvent{}, survivors...), orphaned...) {
		a.OnSwap(e)
	}
	a.OnReorg(11)

	want := NewAggregator(DefaultConfig())
	for _, e := range survivors {
		want.OnSwap(e)
	}
	compare := func(stage string) {
		t.Helper()
		for _, iv := range intervals {
			got, w := klines(t, a, iv.name), klines(t, want, iv.name)
			if !reflect.DeepEqual(got, w) {
				t.Errorf("%s %s:\n got  %+v\n want %+v", stage, iv.name, got, w)
			}
		}
		if got, w := a.Trades(pool, 0), want.Trades(pool, 0); !reflect.DeepEqual(got, w) {
			t.Errorf("%s trades:\n got  %+v\n want %+v", stage, got, w)
		}
	}
	compare("回滚后")

	// 回滚后那根 1m 的最高价应回到 4，而不是被回滚成交的 25
	oneMin := klines(t, a, "1m")
	if last := oneMin[len(oneMin)-1]; last.High != 4 || last.Trades != 2 || last.Volume0 != 150 {
		t.Errorf("recomputed 1m candle = %+v", last)
	}

	// 新链的成交重新进来
	a.OnSwap(replacement)
	want.OnSwap(replacement)
	compare("重新索引后")

	// 回滚点之后没有成交时什么都不变
	a.OnReorg(100)
	compare("空回滚后")
}

func TestMemoryBounds(t *testing.T) {
	a := NewAggregator(Config{MaxCandles: 1440, MaxTrades: 5, MaxPools: 2})
	for i := int64(0); i < 2000; i++ {
		a.OnSwap(swap(uint64(i), base+i*60, 1, 1, -1)) // 每分钟一笔
	}
	if n := len(klines(t, a, "1m")); n != 1440 {
		t.Errorf("1m candles = %d, want 1440", n)
	}
	if n := len(a.Trades(pool, 0)); n != 5 {
		t.Errorf("trades = %d, want 5", n)
	}

	// 池子数超过上限后，新池子不再占用内存
	for i := 0; i < 5; i++ {
		e := swap(1, base, 1, 1, -1)
		e.PoolAddress = fmt.Sprintf("0xother%d", i)
		a.OnSwap(e)
	}
	if len(a.pools) != 2 {
		t.Errorf("pools = %d, want 2", len(a.pools))
	}
}

func TestConcurrentAccess(t *testing.T) {
	a := NewAggregator(DefaultConfig())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(0); i < 500; i++ {
			a.OnSwap(swap(uint64(i), base+i*7, 1+i%3, 1, -1))
			if i%100 == 99 {
				a.OnReorg(uint64(i - 3))
			}
		}
	}()
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				a.Klines(pool, "5m", 50)
				a.Trades(pool, 50)
			}
		}()
	}
	wg.Wait()
}
