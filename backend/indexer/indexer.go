package indexer

import (
	"context"
	"log"
	"math/big"
	"sync"
	"time"

	"unigo-dex/metrics"
)

// indexer.go — 链上事件索引器（含 Reorg 检测与回滚）
//
// 索引器是链下系统的"眼睛"，负责：
//   1. 监听区块链上的新区块和合约事件
//   2. 解析事件日志为结构化数据
//   3. 更新链下池子状态
//   4. 检测链重组（Reorg）并执行回滚
//
// === Reorg 检测机制 ===
//
// 区块链的最终性不是即时的，可能发生 Reorg（链重组）：
//   - 节点收到更长的分叉链时，会切换到新链
//   - 已确认的区块可能被撤销
//   - 基于这些区块的状态更新也需要回滚
//
// 本实现采用 "延迟确认 + 父块哈希校验" 策略：
//   1. 维护最近 N 个区块的哈希链
//   2. 每收到新区块时，验证其 parentHash 是否与已知的前一个区块一致
//   3. 不一致则说明发生了 Reorg：向前找到分叉点，撤销分叉点之后的状态变更，
//      再从分叉点重新索引
//   4. 数据库状态不直接标记为 Success，而是先进入 Pending_Confirmation
//   5. 等待 K 个确认后才标记为最终确认

// BlockInfo 存储区块基本信息，用于 Reorg 检测
type BlockInfo struct {
	Number     uint64
	Hash       string
	ParentHash string
	Timestamp  time.Time
}

// ConfirmationStatus 交易确认状态（延迟确认状态机）
type ConfirmationStatus int

const (
	// StatusPending 等待确认（刚检测到事件）
	StatusPending ConfirmationStatus = iota
	// StatusPendingConfirmation 软确认（已有 1-K 个确认）
	StatusPendingConfirmation
	// StatusConfirmed 最终确认（已有 K+ 个确认）
	StatusConfirmed
	// StatusRolledBack 已回滚（因 Reorg 被撤销）
	StatusRolledBack
)

// ChainSource 链数据源
//
// 索引器只依赖这个接口，不关心区块从哪来：
//   - chain.Source：通过 go-ethereum 连接真实节点
//   - simSource：未配置节点时的模拟源（只出空区块）
type ChainSource interface {
	// LatestBlock 返回最新区块号
	LatestBlock(ctx context.Context) (uint64, error)
	// BlockByNumber 返回指定高度的区块信息（hash / parentHash / timestamp）
	BlockByNumber(ctx context.Context, number uint64) (*BlockInfo, error)
	// BlockEvents 返回该区块内已解析的事件，按 logIndex 升序
	BlockEvents(ctx context.Context, block *BlockInfo) ([]interface{}, error)
}

// SwapListener 订阅 Swap 事件流（K 线聚合、三明治检测等）
//
// 回调在索引协程中同步执行，顺序与链上一致，实现方不要长时间阻塞。
type SwapListener interface {
	// OnSwap 每处理完一笔已知池子的 Swap 事件调用一次
	OnSwap(event *SwapEvent)
	// OnReorg 区块号 >= forkBlock 的数据已被回滚，随后会重新推送
	OnReorg(forkBlock uint64)
}

// IndexerConfig 索引器配置
type IndexerConfig struct {
	// RPCURL 以太坊节点的 RPC 地址
	RPCURL string
	// FactoryAddress Factory 合约地址
	FactoryAddress string
	// StartBlock 开始索引的区块号
	StartBlock uint64
	// ConfirmationBlocks 确认所需的区块数（防 Reorg 深度）
	ConfirmationBlocks uint64
	// PollInterval 轮询间隔
	PollInterval time.Duration
	// MaxBlockRange 单次轮询最多处理的区块数
	MaxBlockRange uint64
}

// DefaultConfig 返回默认索引器配置
func DefaultConfig() IndexerConfig {
	return IndexerConfig{
		RPCURL:             "http://127.0.0.1:8545",
		ConfirmationBlocks: 12, // 以太坊约 12 个区块确认
		PollInterval:       2 * time.Second,
		MaxBlockRange:      1000,
	}
}

// undoEntry 一条可撤销的状态变更（Reorg 回滚用）
type undoEntry struct {
	block uint64
	undo  func()
}

// Indexer 链上事件索引器
type Indexer struct {
	config IndexerConfig
	state  *PoolStateManager
	source ChainSource

	// Reorg 检测：最近 N 个区块的哈希链
	blockHistory    map[uint64]*BlockInfo
	blockHistoryMu  sync.RWMutex
	maxHistoryDepth uint64

	// 最近 N 个区块内的状态变更，Reorg 时逆序撤销（只在索引协程中访问）
	undoLog []undoEntry

	// Swap 事件订阅者（Start 之前注册）
	listeners []SwapListener

	// 统计信息
	stats   IndexerStats
	statsMu sync.Mutex
}

// IndexerStats 索引器运行统计
type IndexerStats struct {
	BlocksProcessed uint64
	EventsProcessed uint64
	ReorgsDetected  uint64
	LastBlockNumber uint64
	LastBlockTime   time.Time
}

// NewIndexer 创建新的索引器实例
//
// source 为 nil 时使用模拟源（不连接节点）。
func NewIndexer(config IndexerConfig, stateManager *PoolStateManager, source ChainSource) *Indexer {
	if source == nil {
		source = &simSource{next: config.StartBlock}
	}
	return &Indexer{
		config:          config,
		state:           stateManager,
		source:          source,
		blockHistory:    make(map[uint64]*BlockInfo),
		maxHistoryDepth: config.ConfirmationBlocks * 2,
	}
}

// Subscribe 注册 Swap 事件订阅者，必须在 Start 之前调用
func (idx *Indexer) Subscribe(l SwapListener) {
	idx.listeners = append(idx.listeners, l)
}

// ConfirmationBlocks 返回延迟确认所需的区块数
func (idx *Indexer) ConfirmationBlocks() uint64 {
	return idx.config.ConfirmationBlocks
}

// Start 启动索引器
//
// 区块轮询协程持续拉取新区块、检测 Reorg，并同步处理区块内的事件。
// 事件与回滚在同一个协程里顺序执行，保证状态变更的顺序与链上一致。
func (idx *Indexer) Start(ctx context.Context) error {
	log.Printf("[Indexer] 启动索引器，起始区块: %d, 确认深度: %d",
		idx.config.StartBlock, idx.config.ConfirmationBlocks)

	go idx.pollBlocks(ctx)

	return nil
}

// pollBlocks 持续轮询新区块
func (idx *Indexer) pollBlocks(ctx context.Context) {
	ticker := time.NewTicker(idx.config.PollInterval)
	defer ticker.Stop()

	next := idx.config.StartBlock

	for {
		select {
		case <-ctx.Done():
			log.Println("[Indexer] 区块轮询停止")
			return
		case <-ticker.C:
			next = idx.syncOnce(ctx, next)
		}
	}
}

// SyncOnce 同步执行一轮追块：从 next 开始追到最新区块（最多 MaxBlockRange 个），返回下一个待处理的区块号
//
// 正常运行时由 Start 启动的轮询协程调用；压测和工具需要精确控制追块时机时可以直接调用，
// 但不要与 Start 同时使用。
func (idx *Indexer) SyncOnce(ctx context.Context, next uint64) uint64 {
	return idx.syncOnce(ctx, next)
}

// syncOnce 从 next 开始追到最新区块，返回下一个待处理的区块号
func (idx *Indexer) syncOnce(ctx context.Context, next uint64) uint64 {
	latest, err := idx.source.LatestBlock(ctx)
	if err != nil {
		log.Printf("[Indexer] 获取最新区块号失败: %v", err)
		return next
	}
	if limit := next + idx.config.MaxBlockRange; idx.config.MaxBlockRange > 0 && latest >= limit {
		latest = limit - 1
	}

	for next <= latest && ctx.Err() == nil {
		n, err := idx.processBlock(ctx, next)
		if err != nil {
			log.Printf("[Indexer] 处理区块 %d 失败: %v", next, err)
			break
		}
		next = n
	}
	return next
}

// processBlock 处理单个区块，返回下一个待处理的区块号
//
// 核心流程：
//  1. 获取区块信息（hash, parentHash）
//  2. 验证 parentHash 与已知的前一个区块是否一致（Reorg 检测）
//  3. 如果检测到 Reorg，回滚并返回分叉点，下一轮从分叉点重新索引
//  4. 获取并处理该区块中的相关事件
func (idx *Indexer) processBlock(ctx context.Context, blockNumber uint64) (uint64, error) {
	block, err := idx.source.BlockByNumber(ctx, blockNumber)
	if err != nil {
		return blockNumber, err
	}

	// === Reorg 检测 ===
	if blockNumber > 0 {
		idx.blockHistoryMu.RLock()
		prevBlock, hasPrev := idx.blockHistory[blockNumber-1]
		idx.blockHistoryMu.RUnlock()

		if hasPrev && block.ParentHash != prevBlock.Hash {
			log.Printf("[Indexer] ⚠️ 检测到 Reorg！区块 %d 的 parentHash 不匹配", blockNumber)
			log.Printf("[Indexer]   期望: %s", prevBlock.Hash)
			log.Printf("[Indexer]   实际: %s", block.ParentHash)

			forkBlock, err := idx.findForkBlock(ctx, blockNumber-1)
			if err != nil {
				return blockNumber, err
			}
			idx.handleReorg(forkBlock)
			return forkBlock, nil
		}
	}

	events, err := idx.source.BlockEvents(ctx, block)
	if err != nil {
		return blockNumber, err
	}
	for _, event := range events {
		idx.handleEvent(event)
	}

	// 记录区块信息
	idx.blockHistoryMu.Lock()
	idx.blockHistory[blockNumber] = block
	// 清理过旧的历史
	if blockNumber > idx.maxHistoryDepth {
		delete(idx.blockHistory, blockNumber-idx.maxHistoryDepth)
	}
	idx.blockHistoryMu.Unlock()

	// 超出回滚窗口的变更不再需要撤销
	drop := 0
	for drop < len(idx.undoLog) && idx.undoLog[drop].block+idx.maxHistoryDepth < blockNumber {
		drop++
	}
	idx.undoLog = idx.undoLog[drop:]

	// 更新统计
	idx.statsMu.Lock()
	idx.stats.BlocksProcessed++
	idx.stats.LastBlockNumber = blockNumber
	idx.stats.LastBlockTime = block.Timestamp
	idx.statsMu.Unlock()
	metrics.IndexerBlocksProcessed.Inc()

	return blockNumber + 1, nil
}

// findForkBlock 从 from 向前比对本地哈希链与链上区块，返回第一个需要重新索引的区块号
//
// 超出本地保留的历史深度时，只能回滚到最早保留的区块。
func (idx *Indexer) findForkBlock(ctx context.Context, from uint64) (uint64, error) {
	for n := from; ; n-- {
		idx.blockHistoryMu.RLock()
		known, ok := idx.blockHistory[n]
		idx.blockHistoryMu.RUnlock()
		if !ok {
			log.Printf("[Indexer] ⚠️ Reorg 深度超过保留的历史（%d 个区块），只能回滚到区块 %d",
				idx.maxHistoryDepth, n+1)
			return n + 1, nil
		}

		onChain, err := idx.source.BlockByNumber(ctx, n)
		if err != nil {
			return 0, err
		}
		if onChain.Hash == known.Hash {
			return n + 1, nil
		}
		if n == 0 {
			return 0, nil
		}
	}
}

// handleReorg 处理链重组
//
// 回滚策略：
//  1. 逆序撤销分叉点及之后区块的状态变更
//  2. 通知订阅者丢弃这些区块的数据（K 线、MEV 告警）
//  3. 清理分叉区块的哈希记录，由轮询协程从分叉点重新索引
func (idx *Indexer) handleReorg(forkBlock uint64) {
	log.Printf("[Indexer] 🔄 执行 Reorg 回滚，从区块 %d 重新索引", forkBlock)

	idx.statsMu.Lock()
	idx.stats.ReorgsDetected++
	idx.statsMu.Unlock()
	metrics.IndexerReorgsTotal.Inc()

	// 逆序撤销池子状态变更
	keep := len(idx.undoLog)
	for keep > 0 && idx.undoLog[keep-1].block >= forkBlock {
		keep--
		idx.undoLog[keep].undo()
	}
	idx.undoLog = idx.undoLog[:keep]

	if forkBlock > 0 {
		rolledBack := idx.state.RollbackToBlock(forkBlock - 1)
		log.Printf("[Indexer] 回滚了 %d 个池子的状态", rolledBack)
	}

	for _, l := range idx.listeners {
		l.OnReorg(forkBlock)
	}

	// 清理分叉区块的历史记录
	idx.blockHistoryMu.Lock()
	for num := range idx.blockHistory {
		if num >= forkBlock {
			delete(idx.blockHistory, num)
		}
	}
	idx.blockHistoryMu.Unlock()
}

// handleEvent 根据事件类型分发处理，并记录撤销动作
func (idx *Indexer) handleEvent(event interface{}) {
	switch e := event.(type) {
	case *PoolCreatedEvent:
		idx.state.AddPool(NewPoolState(e.Pool, e.Token0, e.Token1, e.Fee, e.TickSpacing))
		idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() { idx.state.RemovePool(e.Pool) }})
		log.Printf("[Indexer] 处理 PoolCreated 事件: pool=%s block=%d", e.Pool, e.BlockNumber)
	case *InitializeEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			pool.ApplyInitialize(e)
			idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() {
				pool.ApplyInitialize(&InitializeEvent{SqrtPriceX96: big.NewInt(0), BlockNumber: e.BlockNumber})
			}})
			log.Printf("[Indexer] 处理 Initialize 事件: pool=%s tick=%d block=%d", e.PoolAddress[:10], e.Tick, e.BlockNumber)
		}
	case *MintEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			pool.ApplyMint(e)
			idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() {
				pool.ApplyBurn(&BurnEvent{TickLower: e.TickLower, TickUpper: e.TickUpper, Amount: e.Amount, BlockNumber: e.BlockNumber})
			}})
			log.Printf("[Indexer] 处理 Mint 事件: pool=%s block=%d", e.PoolAddress[:10], e.BlockNumber)
		}
	case *BurnEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			pool.ApplyBurn(e)
			idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() {
				pool.ApplyMint(&MintEvent{TickLower: e.TickLower, TickUpper: e.TickUpper, Amount: e.Amount, BlockNumber: e.BlockNumber})
			}})
			log.Printf("[Indexer] 处理 Burn 事件: pool=%s block=%d", e.PoolAddress[:10], e.BlockNumber)
		}
	case *SwapEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			before := pool.Snapshot()
			pool.ApplySwap(e)
			idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() {
				pool.ApplySwap(&SwapEvent{SqrtPriceX96: before.SqrtPriceX96, Liquidity: before.Liquidity, Tick: before.Tick, BlockNumber: e.BlockNumber})
			}})
			for _, l := range idx.listeners {
				l.OnSwap(e)
			}
			log.Printf("[Indexer] 处理 Swap 事件: pool=%s tick=%d block=%d",
				e.PoolAddress[:10], e.Tick, e.BlockNumber)
		}
	}
	// Collect / Flash 不改变价格和流动性，只计数

	idx.statsMu.Lock()
	idx.stats.EventsProcessed++
	idx.statsMu.Unlock()
}

// GetStats 获取索引器运行统计
func (idx *Indexer) GetStats() IndexerStats {
	idx.statsMu.Lock()
	defer idx.statsMu.Unlock()
	return idx.stats
}
