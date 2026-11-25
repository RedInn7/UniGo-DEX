package indexer_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"unigo-dex/indexer"
	"unigo-dex/kline"
	"unigo-dex/mev"
	"unigo-dex/oracle"
)

// bench_test.go — 索引器回放事件的吞吐
//
//	go test -p 2 -run '^$' -bench . -benchmem ./indexer
//
// 数据源在内存里（不含 RPC 耗时），量的是"拿到区块之后"的处理速度：
// 父哈希校验、事件分发、池子状态更新、撤销日志，以及订阅者（K 线 / 三明治检测 / 预言机）的处理。

const (
	benchBlocks         = 2000
	benchEventsPerBlock = 10
	benchPools          = 8
)

// memoryChain 预先造好的内存链
type memoryChain struct {
	blocks []*indexer.BlockInfo
	events [][]interface{}
}

func (c *memoryChain) LatestBlock(context.Context) (uint64, error) {
	return uint64(len(c.blocks) - 1), nil
}

func (c *memoryChain) BlockByNumber(_ context.Context, number uint64) (*indexer.BlockInfo, error) {
	return c.blocks[number], nil
}

func (c *memoryChain) BlockEvents(_ context.Context, blocks []*indexer.BlockInfo) ([][]interface{}, error) {
	events := make([][]interface{}, len(blocks))
	for i, block := range blocks {
		events[i] = c.events[block.Number]
	}
	return events, nil
}

// buildChain 区块 0 创建并初始化 benchPools 个池子，之后每个区块 benchEventsPerBlock 个事件：
// 8 笔 Swap、1 笔 Mint、1 笔 Burn
func buildChain() *memoryChain {
	c := &memoryChain{}
	liquidity, _ := new(big.Int).SetString("1000000000000000000000000", 10)
	pool := func(i int) string { return fmt.Sprintf("0x%040x", 0x1000+i) }

	var genesis []interface{}
	for i := 0; i < benchPools; i++ {
		genesis = append(genesis,
			&indexer.PoolCreatedEvent{Pool: pool(i), Token0: fmt.Sprintf("0xtoken%d", i), Token1: fmt.Sprintf("0xtoken%d", i+1), Fee: 3000, TickSpacing: 60},
			&indexer.InitializeEvent{PoolAddress: pool(i), SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96)},
			&indexer.MintEvent{PoolAddress: pool(i), TickLower: -6000, TickUpper: 6000, Amount: liquidity},
		)
	}

	seed := uint32(7)
	next := func() uint32 { seed = seed*1664525 + 1013904223; return seed >> 8 }
	for n := 0; n <= benchBlocks; n++ {
		block := &indexer.BlockInfo{Number: uint64(n), Hash: fmt.Sprintf("0xblock%d", n), Timestamp: time.Unix(int64(1_700_000_000+12*n), 0)}
		if n > 0 {
			block.ParentHash = c.blocks[n-1].Hash
		}
		c.blocks = append(c.blocks, block)
		if n == 0 {
			c.events = append(c.events, genesis)
			continue
		}
		events := make([]interface{}, 0, benchEventsPerBlock)
		for i := 0; i < benchEventsPerBlock-2; i++ {
			amount0, amount1 := big.NewInt(1e15), big.NewInt(-1e15)
			if next()%2 == 0 {
				amount0, amount1 = amount1, amount0
			}
			tick := int32(next()%200) - 100
			events = append(events, &indexer.SwapEvent{
				PoolAddress: pool(int(next() % benchPools)), Sender: "0xrouter", Recipient: fmt.Sprintf("0xuser%d", next()%16),
				Amount0: amount0, Amount1: amount1,
				SqrtPriceX96: new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 96), big.NewInt(int64(tick)*1e12)),
				Liquidity:    liquidity, Tick: tick,
				BlockNumber: uint64(n), Timestamp: uint64(block.Timestamp.Unix()), LogIndex: uint(i),
				TxHash: fmt.Sprintf("0xtx%d-%d", n, i),
			})
		}
		lower := int32(next()%50)*60 - 6000
		events = append(events,
			&indexer.MintEvent{PoolAddress: pool(int(next() % benchPools)), TickLower: lower, TickUpper: lower + 600, Amount: big.NewInt(1e18), BlockNumber: uint64(n)},
			&indexer.BurnEvent{PoolAddress: pool(0), TickLower: -6000, TickUpper: 6000, Amount: big.NewInt(1e12), BlockNumber: uint64(n)},
		)
		c.events = append(c.events, events)
	}
	return c
}

func replay(b *testing.B, chain *memoryChain, withListeners bool) {
	b.ReportAllocs()
	events := benchBlocks * benchEventsPerBlock
	for i := 0; i < b.N; i++ {
		state := indexer.NewPoolStateManager()
		idx := indexer.NewIndexer(indexer.DefaultConfig(), state, chain)
		if withListeners {
			idx.Subscribe(oracle.NewOracle(oracle.DefaultOracleConfig()).PoolPriceFeed(state))
			idx.Subscribe(kline.NewAggregator(kline.DefaultConfig()))
			idx.Subscribe(mev.NewGuard(mev.DefaultConfig()))
		}
		next := uint64(0)
		for next <= benchBlocks {
			next = idx.SyncOnce(context.Background(), next)
		}
		if got := idx.GetStats().EventsProcessed; got < uint64(events) {
			b.Fatalf("processed %d events, want at least %d", got, events)
		}
	}
	perOp := b.Elapsed().Seconds() / float64(b.N)
	b.ReportMetric(float64(events)/perOp, "events/s")
	b.ReportMetric(float64(benchBlocks)/perOp, "blocks/s")
}

// BenchmarkReplay 回放 2000 个区块、2 万个事件
func BenchmarkReplay(b *testing.B) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	chain := buildChain()

	b.Run("indexer_only", func(b *testing.B) { replay(b, chain, false) })
	b.Run("with_kline_mev_oracle", func(b *testing.B) { replay(b, chain, true) })
}
