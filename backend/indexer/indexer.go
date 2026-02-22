package indexer

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
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
//   3. 不一致则说明发生了 Reorg，触发回滚
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
	// MaxBlockRange 单次查询的最大区块范围
	MaxBlockRange uint64
}

// DefaultConfig 返回默认索引器配置
func DefaultConfig() IndexerConfig {
	return IndexerConfig{
		RPCURL:             "http://127.0.0.1:8545",
		ConfirmationBlocks: 12,  // 以太坊约 12 个区块确认
		PollInterval:       2 * time.Second,
		MaxBlockRange:      1000,
	}
}

// Indexer 链上事件索引器
type Indexer struct {
	config IndexerConfig
	state  *PoolStateManager

	// Reorg 检测：最近 N 个区块的哈希链
	blockHistory    map[uint64]*BlockInfo
	blockHistoryMu  sync.RWMutex
	maxHistoryDepth uint64

	// 事件处理通道
	eventChan chan interface{}

	// 统计信息
	stats IndexerStats
}

// IndexerStats 索引器运行统计
type IndexerStats struct {
	BlocksProcessed uint64
	EventsProcessed uint64
	ReorgsDetected  uint64
	LastBlockNumber uint64
	LastBlockTime   time.Time
	mu              sync.Mutex
}

// NewIndexer 创建新的索引器实例
func NewIndexer(config IndexerConfig, stateManager *PoolStateManager) *Indexer {
	return &Indexer{
		config:          config,
		state:           stateManager,
		blockHistory:    make(map[uint64]*BlockInfo),
		maxHistoryDepth: config.ConfirmationBlocks * 2,
		eventChan:       make(chan interface{}, 1000),
	}
}

// Start 启动索引器
//
// 索引器运行两个协程：
//   1. 区块轮询协程：持续拉取新区块，检测 Reorg
//   2. 事件处理协程：消费事件队列，更新池子状态
func (idx *Indexer) Start(ctx context.Context) error {
	log.Printf("[Indexer] 启动索引器，起始区块: %d, 确认深度: %d",
		idx.config.StartBlock, idx.config.ConfirmationBlocks)

	// 启动事件处理协程
	go idx.processEvents(ctx)

	// 启动区块轮询
	go idx.pollBlocks(ctx)

	return nil
}

// pollBlocks 持续轮询新区块
func (idx *Indexer) pollBlocks(ctx context.Context) {
	ticker := time.NewTicker(idx.config.PollInterval)
	defer ticker.Stop()

	currentBlock := idx.config.StartBlock

	for {
		select {
		case <-ctx.Done():
			log.Println("[Indexer] 区块轮询停止")
			return
		case <-ticker.C:
			// 模拟获取最新区块号
			latestBlock := currentBlock + 1

			for blockNum := currentBlock; blockNum <= latestBlock; blockNum++ {
				if err := idx.processBlock(ctx, blockNum); err != nil {
					log.Printf("[Indexer] 处理区块 %d 失败: %v", blockNum, err)
					break
				}
			}

			currentBlock = latestBlock + 1
		}
	}
}

// processBlock 处理单个区块
//
// 核心流程：
//   1. 获取区块信息（hash, parentHash）
//   2. 验证 parentHash 与已知的前一个区块是否一致（Reorg 检测）
//   3. 如果检测到 Reorg，执行回滚
//   4. 获取并处理该区块中的相关事件
func (idx *Indexer) processBlock(ctx context.Context, blockNumber uint64) error {
	// 模拟获取区块信息
	block := &BlockInfo{
		Number:    blockNumber,
		Hash:      fmt.Sprintf("0x%064x", blockNumber), // 模拟哈希
		Timestamp: time.Now(),
	}

	// === Reorg 检测 ===
	idx.blockHistoryMu.RLock()
	prevBlock, hasPrev := idx.blockHistory[blockNumber-1]
	idx.blockHistoryMu.RUnlock()

	if hasPrev && block.ParentHash != "" && block.ParentHash != prevBlock.Hash {
		// 检测到 Reorg！
		log.Printf("[Indexer] ⚠️ 检测到 Reorg！区块 %d 的 parentHash 不匹配", blockNumber)
		log.Printf("[Indexer]   期望: %s", prevBlock.Hash)
		log.Printf("[Indexer]   实际: %s", block.ParentHash)
		idx.handleReorg(blockNumber)
	}

	// 记录区块信息
	idx.blockHistoryMu.Lock()
	idx.blockHistory[blockNumber] = block
	// 清理过旧的历史
	if blockNumber > idx.maxHistoryDepth {
		delete(idx.blockHistory, blockNumber-idx.maxHistoryDepth)
	}
	idx.blockHistoryMu.Unlock()

	// 更新统计
	idx.stats.mu.Lock()
	idx.stats.BlocksProcessed++
	idx.stats.LastBlockNumber = blockNumber
	idx.stats.LastBlockTime = block.Timestamp
	idx.stats.mu.Unlock()

	return nil
}

// handleReorg 处理链重组
//
// 回滚策略：
//   1. 确定 Reorg 的深度（从哪个区块开始分叉）
//   2. 回滚 PoolStateManager 中受影响的状态
//   3. 从分叉点重新索引事件
//   4. 更新确认状态（Pending -> RolledBack）
func (idx *Indexer) handleReorg(forkBlock uint64) {
	log.Printf("[Indexer] 🔄 执行 Reorg 回滚到区块 %d", forkBlock-1)

	idx.stats.mu.Lock()
	idx.stats.ReorgsDetected++
	idx.stats.mu.Unlock()

	// 回滚池子状态
	rolledBack := idx.state.RollbackToBlock(forkBlock - 1)
	log.Printf("[Indexer] 回滚了 %d 个池子的状态", rolledBack)

	// 清理分叉区块的历史记录
	idx.blockHistoryMu.Lock()
	for num := range idx.blockHistory {
		if num >= forkBlock {
			delete(idx.blockHistory, num)
		}
	}
	idx.blockHistoryMu.Unlock()
}

// processEvents 事件处理协程
func (idx *Indexer) processEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-idx.eventChan:
			idx.handleEvent(event)
		}
	}
}

// handleEvent 根据事件类型分发处理
func (idx *Indexer) handleEvent(event interface{}) {
	switch e := event.(type) {
	case *MintEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			pool.ApplyMint(e)
			log.Printf("[Indexer] 处理 Mint 事件: pool=%s block=%d", e.PoolAddress[:10], e.BlockNumber)
		}
	case *BurnEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			pool.ApplyBurn(e)
			log.Printf("[Indexer] 处理 Burn 事件: pool=%s block=%d", e.PoolAddress[:10], e.BlockNumber)
		}
	case *SwapEvent:
		if pool, ok := idx.state.GetPool(e.PoolAddress); ok {
			pool.ApplySwap(e)
			log.Printf("[Indexer] 处理 Swap 事件: pool=%s tick=%d block=%d",
				e.PoolAddress[:10], e.Tick, e.BlockNumber)
		}
	}

	idx.stats.mu.Lock()
	idx.stats.EventsProcessed++
	idx.stats.mu.Unlock()
}

// GetStats 获取索引器运行统计
func (idx *Indexer) GetStats() IndexerStats {
	idx.stats.mu.Lock()
	defer idx.stats.mu.Unlock()
	return idx.stats
}
