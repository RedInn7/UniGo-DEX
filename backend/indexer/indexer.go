package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"unigo-dex/logging"
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
//   4. 交易状态查询（GET /api/v1/tx/{hash}）按同一个确认深度 K 判定：
//      已打包但不足 K 个确认是 pending_confirmation，达到 K 个才是 confirmed

// BlockInfo 存储区块基本信息，用于 Reorg 检测
type BlockInfo struct {
	Number     uint64
	Hash       string
	ParentHash string
	Timestamp  time.Time
}

// ChainSource 链数据源
//
// 索引器只依赖这个接口，不关心区块从哪来：
//   - chain.Client：通过 go-ethereum 连接真实节点
//   - simSource：未配置节点时的模拟源（只出空区块）
type ChainSource interface {
	// LatestBlock 返回最新区块号
	LatestBlock(ctx context.Context) (uint64, error)
	// BlockByNumber 返回指定高度的区块信息（hash / parentHash / timestamp）
	BlockByNumber(ctx context.Context, number uint64) (*BlockInfo, error)
	// BlockEvents 返回一段连续区块内已解析的事件：结果与 blocks 一一对应，每个区块内按 logIndex 升序。
	// 实现方必须保证事件确实属于 blocks 里的那个区块（核对区块哈希），对不上时返回错误。
	BlockEvents(ctx context.Context, blocks []*BlockInfo) ([][]interface{}, error)
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
	// MaxBlockRange 单次日志查询（eth_getLogs）覆盖的最大区块数
	MaxBlockRange uint64
}

// DefaultConfig 返回默认索引器配置
func DefaultConfig() IndexerConfig {
	return IndexerConfig{
		RPCURL:             "http://127.0.0.1:8545",
		ConfirmationBlocks: 12, // 以太坊约 12 个区块确认
		PollInterval:       2 * time.Second,
		MaxBlockRange:      500, // 多数节点服务商对单次 eth_getLogs 的区块范围有限制
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
	logging.L(ctx).Info("启动索引器", "start_block", idx.config.StartBlock, "confirmation_blocks", idx.config.ConfirmationBlocks)

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
			logging.L(ctx).Info("区块轮询停止")
			return
		case <-ticker.C:
			next = idx.syncOnce(ctx, next)
		}
	}
}

// SyncOnce 同步执行一轮追块：从 next 开始追到最新区块，返回下一个待处理的区块号
//
// 正常运行时由 Start 启动的轮询协程调用；压测和工具需要精确控制追块时机时可以直接调用，
// 但不要与 Start 同时使用。
func (idx *Indexer) SyncOnce(ctx context.Context, next uint64) uint64 {
	return idx.syncOnce(ctx, next)
}

// syncOnce 从 next 开始分批追到最新区块，返回下一个待处理的区块号
func (idx *Indexer) syncOnce(ctx context.Context, next uint64) uint64 {
	latest, err := idx.source.LatestBlock(ctx)
	if err != nil {
		logging.L(ctx).Warn("获取最新区块号失败", "err", err)
		return next
	}

	batch := idx.config.MaxBlockRange
	if batch == 0 {
		batch = 1
	}
	for next <= latest && ctx.Err() == nil {
		to := min(latest, next+batch-1)
		n, err := idx.processRange(ctx, next, to)
		if err != nil {
			logging.L(ctx).Warn("处理区块失败", "from", next, "to", to, "err", err)
			break
		}
		next = n
	}
	return next
}

// processRange 处理一段连续的区块 [from, to]，返回下一个待处理的区块号
//
// 核心流程：
//  1. 逐个获取区块头（hash, parentHash, timestamp）
//  2. 验证第一个区块的 parentHash 与本地记录的上一个区块是否一致（Reorg 检测）；
//     检测到 Reorg 就回滚并返回分叉点，下一轮从分叉点重新索引
//  3. 一次性拉取整段区块的事件（一次 eth_getLogs），按区块分好
//  4. 按区块顺序应用事件、记录哈希链
//
// 区块头和事件是分两步读的，中间链可能重组。数据源会核对每条日志所属区块的哈希与
// 第 1 步拿到的区块头一致，不一致就报错，这一段整体作废、下一轮重读，不会把两条分叉上的数据拼在一起。
func (idx *Indexer) processRange(ctx context.Context, from, to uint64) (uint64, error) {
	blocks := make([]*BlockInfo, 0, to-from+1)
	for n := from; n <= to; n++ {
		block, err := idx.source.BlockByNumber(ctx, n)
		if err != nil {
			return from, err
		}
		// 读区块头的过程中链重组了：这一段前后接不上，作废重读
		if len(blocks) > 0 && block.ParentHash != blocks[len(blocks)-1].Hash {
			return from, fmt.Errorf("区块 %d 与前一个区块接不上，链在读取期间发生了重组", n)
		}
		blocks = append(blocks, block)
	}

	// === Reorg 检测 ===
	if from > 0 {
		idx.blockHistoryMu.RLock()
		prevBlock, hasPrev := idx.blockHistory[from-1]
		idx.blockHistoryMu.RUnlock()

		if hasPrev && blocks[0].ParentHash != prevBlock.Hash {
			logging.L(ctx).Warn("检测到 Reorg：parentHash 与本地记录的上一个区块不一致",
				"block", from, "expected_parent", prevBlock.Hash, "actual_parent", blocks[0].ParentHash)

			forkBlock, err := idx.findForkBlock(ctx, from-1)
			if err != nil {
				return from, err
			}
			idx.handleReorg(forkBlock)
			return forkBlock, nil
		}
	}

	events, err := idx.source.BlockEvents(ctx, blocks)
	if err != nil {
		return from, err
	}

	for i, block := range blocks {
		for _, event := range events[i] {
			idx.handleEvent(event)
		}

		// 记录区块信息，清理过旧的历史
		idx.blockHistoryMu.Lock()
		idx.blockHistory[block.Number] = block
		if block.Number > idx.maxHistoryDepth {
			delete(idx.blockHistory, block.Number-idx.maxHistoryDepth)
		}
		idx.blockHistoryMu.Unlock()

		// 超出回滚窗口的变更不再需要撤销
		drop := 0
		for drop < len(idx.undoLog) && idx.undoLog[drop].block+idx.maxHistoryDepth < block.Number {
			drop++
		}
		idx.undoLog = idx.undoLog[drop:]

		// 更新统计
		idx.statsMu.Lock()
		idx.stats.BlocksProcessed++
		idx.stats.LastBlockNumber = block.Number
		idx.stats.LastBlockTime = block.Timestamp
		idx.statsMu.Unlock()
		metrics.IndexerBlocksProcessed.Inc()
	}

	return to + 1, nil
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
			logging.L(ctx).Warn("Reorg 深度超过保留的历史，只能回滚到最早保留的区块",
				"history_depth", idx.maxHistoryDepth, "rollback_to", n+1)
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
	slog.Warn("执行 Reorg 回滚并重新索引", "fork_block", forkBlock)

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
		slog.Info("池子状态已回滚", "pools", rolledBack)
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
		slog.Info("发现新池子", "pool", e.Pool, "token0", e.Token0, "token1", e.Token1, "fee", e.Fee, "block", e.BlockNumber)
	case *InitializeEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			pool.ApplyInitialize(e)
			idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() {
				pool.ApplyInitialize(&InitializeEvent{SqrtPriceX96: big.NewInt(0), BlockNumber: e.BlockNumber})
			}})
			slog.Debug("处理 Initialize 事件", "pool", e.PoolAddress, "tick", e.Tick, "block", e.BlockNumber)
		}
	case *MintEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			pool.ApplyMint(e)
			idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() {
				pool.ApplyBurn(&BurnEvent{TickLower: e.TickLower, TickUpper: e.TickUpper, Amount: e.Amount, BlockNumber: e.BlockNumber})
			}})
			slog.Debug("处理 Mint 事件", "pool", e.PoolAddress, "block", e.BlockNumber)
		}
	case *BurnEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			pool.ApplyBurn(e)
			idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() {
				pool.ApplyMint(&MintEvent{TickLower: e.TickLower, TickUpper: e.TickUpper, Amount: e.Amount, BlockNumber: e.BlockNumber})
			}})
			slog.Debug("处理 Burn 事件", "pool", e.PoolAddress, "block", e.BlockNumber)
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
			slog.Debug("处理 Swap 事件", "pool", e.PoolAddress, "tick", e.Tick, "block", e.BlockNumber)
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
