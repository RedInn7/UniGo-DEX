package kline

import (
	"fmt"
	"math/big"
	"testing"

	"unigo-dex/indexer"
)

// BenchmarkOnSwap K 线写入：每笔成交更新 6 个周期的 K 线并记入成交记录
//
//	go test -p 2 -run '^$' -bench . -benchmem ./kline
func BenchmarkOnSwap(b *testing.B) {
	for _, pools := range []int{1, 64} {
		b.Run(fmt.Sprintf("pools=%d", pools), func(b *testing.B) {
			events := make([]*indexer.SwapEvent, 4096)
			for i := range events {
				events[i] = &indexer.SwapEvent{
					PoolAddress:  fmt.Sprintf("0xpool%d", i%pools),
					Sender:       "0xrouter",
					Recipient:    "0xuser",
					Amount0:      big.NewInt(int64(1e15 + i)),
					Amount1:      big.NewInt(int64(-1e15 - i)),
					SqrtPriceX96: new(big.Int).Lsh(big.NewInt(int64(1000+i%50)), 86),
					TxHash:       "0x9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
				}
			}
			a := NewAggregator(DefaultConfig())
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e := events[i%len(events)]
				// 每笔成交间隔 3 秒：每 20 笔开一根新的 1m K 线
				e.BlockNumber, e.Timestamp = uint64(i/5), uint64(1_700_000_000+3*i)
				a.OnSwap(e)
			}
		})
	}
}

// BenchmarkKlines K 线查询：取最近 100 根 1m K 线
func BenchmarkKlines(b *testing.B) {
	a := NewAggregator(DefaultConfig())
	for i := 0; i < 5000; i++ {
		a.OnSwap(&indexer.SwapEvent{
			PoolAddress: "0xpool", Amount0: big.NewInt(1e15), Amount1: big.NewInt(-1e15),
			SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1000), 86),
			BlockNumber:  uint64(i), Timestamp: uint64(1_700_000_000 + 30*i),
		})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if candles, err := a.Klines("0xpool", "1m", 100); err != nil || len(candles) != 100 {
			b.Fatal(len(candles), err)
		}
	}
}
