package mev

import (
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"testing"

	"unigo-dex/indexer"
)

// BenchmarkOnSwap 三明治检测：每个区块、每个池子内有 perBlock 笔 swap，
// 每笔新 swap 都要和同区块同池子里之前的 swap 配对检查
//
//	go test -p 2 -run '^$' -bench . -benchmem ./mev
func BenchmarkOnSwap(b *testing.B) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // 告警日志不计入
	for _, perBlock := range []int{5, 20, 100} {
		b.Run(fmt.Sprintf("swaps_per_block=%d", perBlock), func(b *testing.B) {
			// 预先造好事件：8 个交易者随机方向，偶尔会构成真实的三明治
			const actors = 8
			events := make([]*indexer.SwapEvent, 8192)
			seed := uint32(12345)
			next := func() uint32 { seed = seed*1664525 + 1013904223; return seed >> 8 }
			for i := range events {
				amount0, amount1 := big.NewInt(100), big.NewInt(-100)
				if next()%2 == 0 {
					amount0, amount1 = amount1, amount0
				}
				events[i] = &indexer.SwapEvent{
					PoolAddress: "0xpool", Sender: "0xrouter", Recipient: fmt.Sprintf("0xuser%d", next()%actors),
					Amount0: amount0, Amount1: amount1, TxHash: fmt.Sprintf("0xtx%d", i),
				}
			}
			g := NewGuard(DefaultConfig())
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e := events[i%len(events)]
				e.BlockNumber, e.LogIndex = uint64(i/perBlock), uint(i%perBlock)
				g.OnSwap(e)
			}
			b.StopTimer()
			b.ReportMetric(float64(len(g.Alerts(0))), "alerts_kept")
		})
	}
}
