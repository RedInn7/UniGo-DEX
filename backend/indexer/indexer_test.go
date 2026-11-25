package indexer

import (
	"context"
	"fmt"
	"math/big"
	"testing"
)

// fakeChain 可分叉的内存链，用于测试 Reorg 检测与回滚
type fakeChain struct {
	blocks []*BlockInfo
	events map[string][]interface{} // 区块哈希 -> 事件
}

func newFakeChain() *fakeChain {
	return &fakeChain{events: make(map[string][]interface{})}
}

// add 在链尾追加一个区块，salt 用来让分叉上的区块哈希不同
func (c *fakeChain) add(salt string, events ...interface{}) {
	n := uint64(len(c.blocks))
	b := &BlockInfo{Number: n, Hash: fmt.Sprintf("0x%s%d", salt, n)}
	if n > 0 {
		b.ParentHash = c.blocks[n-1].Hash
	}
	c.blocks = append(c.blocks, b)
	c.events[b.Hash] = events
}

func (c *fakeChain) LatestBlock(ctx context.Context) (uint64, error) {
	return uint64(len(c.blocks) - 1), nil
}

func (c *fakeChain) BlockByNumber(ctx context.Context, number uint64) (*BlockInfo, error) {
	if number >= uint64(len(c.blocks)) {
		return nil, fmt.Errorf("block %d not found", number)
	}
	return c.blocks[number], nil
}

func (c *fakeChain) BlockEvents(ctx context.Context, blocks []*BlockInfo) ([][]interface{}, error) {
	events := make([][]interface{}, len(blocks))
	for i, block := range blocks {
		events[i] = c.events[block.Hash]
	}
	return events, nil
}

type recorder struct {
	swaps  []*SwapEvent
	reorgs []uint64
}

func (r *recorder) OnSwap(e *SwapEvent) { r.swaps = append(r.swaps, e) }
func (r *recorder) OnReorg(fork uint64) { r.reorgs = append(r.reorgs, fork) }

const testPool = "0xpool000000000000000000000000000000000001"

func swapAt(block uint64, tick int32) *SwapEvent {
	return &SwapEvent{
		PoolAddress:  testPool,
		Amount0:      big.NewInt(100),
		Amount1:      big.NewInt(-100),
		SqrtPriceX96: big.NewInt(int64(1000 + tick)),
		Liquidity:    big.NewInt(500),
		Tick:         tick,
		BlockNumber:  block,
	}
}

func TestIndexerAppliesEvents(t *testing.T) {
	chain := newFakeChain()
	chain.add("a")
	chain.add("a", &PoolCreatedEvent{Pool: testPool, Token0: "0xt0", Token1: "0xt1", Fee: 3000, TickSpacing: 60, BlockNumber: 1})
	chain.add("a", &MintEvent{PoolAddress: testPool, TickLower: -60, TickUpper: 60, Amount: big.NewInt(500), BlockNumber: 2})
	chain.add("a", swapAt(3, 7))

	state := NewPoolStateManager()
	idx := NewIndexer(DefaultConfig(), state, chain)
	rec := &recorder{}
	idx.Subscribe(rec)

	if next := idx.syncOnce(context.Background(), 0); next != 4 {
		t.Fatalf("next = %d, want 4", next)
	}
	pool, ok := state.GetPool(testPool)
	if !ok {
		t.Fatal("pool not registered from PoolCreated")
	}
	snap := pool.Snapshot()
	if snap.Tick != 7 || snap.Liquidity.Int64() != 500 {
		t.Fatalf("tick=%d liquidity=%s", snap.Tick, snap.Liquidity)
	}
	if len(rec.swaps) != 1 {
		t.Fatalf("listener got %d swaps, want 1", len(rec.swaps))
	}
	stats := idx.GetStats()
	if stats.BlocksProcessed != 4 || stats.EventsProcessed != 3 || stats.LastBlockNumber != 3 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestIndexerReorgRollsBackAndReindexes(t *testing.T) {
	chain := newFakeChain()
	chain.add("a")
	chain.add("a", &PoolCreatedEvent{Pool: testPool, Token0: "0xt0", Token1: "0xt1", Fee: 3000, BlockNumber: 1})
	chain.add("a", &MintEvent{PoolAddress: testPool, TickLower: -60, TickUpper: 60, Amount: big.NewInt(500), BlockNumber: 2})
	chain.add("a", swapAt(3, 7))
	chain.add("a",
		&MintEvent{PoolAddress: testPool, TickLower: -60, TickUpper: 60, Amount: big.NewInt(300), BlockNumber: 4},
		swapAt(4, 20))

	state := NewPoolStateManager()
	idx := NewIndexer(DefaultConfig(), state, chain)
	rec := &recorder{}
	idx.Subscribe(rec)
	ctx := context.Background()
	next := idx.syncOnce(ctx, 0)

	// 区块 3、4 被分叉替换：新链上区块 3 没有交易，区块 4 有一笔不同的 swap
	chain.blocks = chain.blocks[:3]
	chain.add("b")
	chain.add("b", swapAt(4, -5))
	chain.add("b")

	next = idx.syncOnce(ctx, next) // 发现 parentHash 不匹配并回滚
	next = idx.syncOnce(ctx, next) // 从分叉点重新索引
	if next != 6 {
		t.Fatalf("next = %d, want 6", next)
	}
	if len(rec.reorgs) != 1 || rec.reorgs[0] != 3 {
		t.Fatalf("reorgs = %v, want [3]", rec.reorgs)
	}
	if idx.GetStats().ReorgsDetected != 1 {
		t.Fatalf("ReorgsDetected = %d", idx.GetStats().ReorgsDetected)
	}

	pool, _ := state.GetPool(testPool)
	snap := pool.Snapshot()
	if snap.Tick != -5 {
		t.Fatalf("tick = %d, want -5 (来自新链的 swap)", snap.Tick)
	}
	// 被回滚的 300 流动性 Mint 不能留在 tick 上
	if got := pool.Ticks[-60].LiquidityGross.Int64(); got != 500 {
		t.Fatalf("tick -60 liquidityGross = %d, want 500", got)
	}
	// 订阅者收到：旧链 2 笔 + 新链 1 笔
	if len(rec.swaps) != 3 || rec.swaps[2].Tick != -5 {
		t.Fatalf("listener swaps = %d", len(rec.swaps))
	}
}

func TestIndexerReorgRemovesPoolCreatedOnFork(t *testing.T) {
	chain := newFakeChain()
	chain.add("a")
	chain.add("a", &PoolCreatedEvent{Pool: testPool, BlockNumber: 1})

	state := NewPoolStateManager()
	idx := NewIndexer(DefaultConfig(), state, chain)
	ctx := context.Background()
	next := idx.syncOnce(ctx, 0)

	chain.blocks = chain.blocks[:1]
	chain.add("b")
	chain.add("b")
	next = idx.syncOnce(ctx, next)
	idx.syncOnce(ctx, next)

	if _, ok := state.GetPool(testPool); ok {
		t.Fatal("pool created on the orphaned fork should be removed")
	}
}

func TestSimSourceNeverReorgs(t *testing.T) {
	idx := NewIndexer(DefaultConfig(), NewPoolStateManager(), nil)
	ctx := context.Background()
	next := idx.syncOnce(ctx, 0)
	next = idx.syncOnce(ctx, next)
	stats := idx.GetStats()
	if next != 4 || stats.BlocksProcessed != 4 || stats.ReorgsDetected != 0 {
		t.Fatalf("next=%d stats=%+v", next, stats)
	}
}
