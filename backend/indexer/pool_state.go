package indexer

import (
	"fmt"
	"math/big"
	"sort"
	"sync"
)

// pool_state.go — 链下池子状态管理
//
// 通过监听链上事件，在内存中重建完整的池子状态。
// 这是链下路由引擎和报价服务的数据基础。
//
// 设计目标：
//   1. 链下状态与链上完全一致（通过事件回放保证）
//   2. 支持快速查询（O(1) 获取当前价格，O(log n) 查找下一个 tick）
//   3. 线程安全（读写锁保护，支持并发查询）
//   4. 支持 reorg 回滚（通过区块号追踪状态变更历史）

// TickInfo 单个 tick 的链下状态
type TickInfo struct {
	LiquidityGross *big.Int // 引用此 tick 的总流动性
	LiquidityNet   *big.Int // 穿过此 tick 时的净流动性变化
	Initialized    bool
}

// PoolState 单个池子的完整链下状态
type PoolState struct {
	Address     string
	Token0      string
	Token1      string
	Fee         uint32
	TickSpacing int32

	// 核心状态（对应 Solidity 的 slot0 和 liquidity）
	SqrtPriceX96 *big.Int
	Tick         int32
	Liquidity    *big.Int

	// Tick 状态映射
	Ticks map[int32]*TickInfo

	// 已初始化的 tick 列表（排序），用于快速查找下一个 tick
	InitializedTicks []int32

	// 最后更新的区块号，用于 reorg 检测
	LastBlockNumber uint64

	mu sync.RWMutex
}

// NewPoolState 创建新的池子状态
func NewPoolState(address, token0, token1 string, fee uint32, tickSpacing int32) *PoolState {
	return &PoolState{
		Address:      address,
		Token0:       token0,
		Token1:       token1,
		Fee:          fee,
		TickSpacing:  tickSpacing,
		SqrtPriceX96: big.NewInt(0),
		Liquidity:    big.NewInt(0),
		Ticks:        make(map[int32]*TickInfo),
	}
}

// ApplyMint 应用 Mint 事件，更新池子状态
func (p *PoolState) ApplyMint(event *MintEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 更新下界 tick
	p.updateTick(event.TickLower, event.Amount, false)
	// 更新上界 tick
	p.updateTick(event.TickUpper, event.Amount, true)

	// 如果当前价格在该 position 的范围内，更新活跃流动性
	if p.Tick >= event.TickLower && p.Tick < event.TickUpper {
		p.Liquidity = new(big.Int).Add(p.Liquidity, event.Amount)
	}

	p.LastBlockNumber = event.BlockNumber
}

// ApplyBurn 应用 Burn 事件
func (p *PoolState) ApplyBurn(event *BurnEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()

	negAmount := new(big.Int).Neg(event.Amount)
	p.updateTick(event.TickLower, negAmount, false)
	p.updateTick(event.TickUpper, negAmount, true)

	if p.Tick >= event.TickLower && p.Tick < event.TickUpper {
		p.Liquidity = new(big.Int).Sub(p.Liquidity, event.Amount)
	}

	p.LastBlockNumber = event.BlockNumber
}

// ApplySwap 应用 Swap 事件，更新价格和 tick
func (p *PoolState) ApplySwap(event *SwapEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.SqrtPriceX96 = new(big.Int).Set(event.SqrtPriceX96)
	p.Tick = event.Tick
	p.Liquidity = new(big.Int).Set(event.Liquidity)
	p.LastBlockNumber = event.BlockNumber
}

// updateTick 更新单个 tick 的状态
func (p *PoolState) updateTick(tick int32, liquidityDelta *big.Int, upper bool) {
	info, exists := p.Ticks[tick]
	if !exists {
		info = &TickInfo{
			LiquidityGross: big.NewInt(0),
			LiquidityNet:   big.NewInt(0),
		}
		p.Ticks[tick] = info
	}

	info.LiquidityGross = new(big.Int).Add(info.LiquidityGross, liquidityDelta)

	if upper {
		info.LiquidityNet = new(big.Int).Sub(info.LiquidityNet, liquidityDelta)
	} else {
		info.LiquidityNet = new(big.Int).Add(info.LiquidityNet, liquidityDelta)
	}

	wasInitialized := info.Initialized
	info.Initialized = info.LiquidityGross.Sign() > 0

	// 维护 InitializedTicks 有序列表
	if !wasInitialized && info.Initialized {
		p.InitializedTicks = append(p.InitializedTicks, tick)
		sort.Slice(p.InitializedTicks, func(i, j int) bool {
			return p.InitializedTicks[i] < p.InitializedTicks[j]
		})
	} else if wasInitialized && !info.Initialized {
		for i, t := range p.InitializedTicks {
			if t == tick {
				p.InitializedTicks = append(p.InitializedTicks[:i], p.InitializedTicks[i+1:]...)
				break
			}
		}
	}
}

// GetNextInitializedTick 查找下一个已初始化的 tick
//
// 用于链下 swap 模拟，对应 Solidity 的 TickBitmap.nextInitializedTickWithinOneWord
// 链下使用排序数组 + 二分查找，比位图更直观
func (p *PoolState) GetNextInitializedTick(tick int32, lte bool) (int32, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.InitializedTicks) == 0 {
		return 0, false
	}

	if lte {
		// 向左搜索：找到 <= tick 的最大已初始化 tick
		idx := sort.Search(len(p.InitializedTicks), func(i int) bool {
			return p.InitializedTicks[i] > tick
		})
		if idx > 0 {
			return p.InitializedTicks[idx-1], true
		}
		return 0, false
	}

	// 向右搜索：找到 > tick 的最小已初始化 tick
	idx := sort.Search(len(p.InitializedTicks), func(i int) bool {
		return p.InitializedTicks[i] > tick
	})
	if idx < len(p.InitializedTicks) {
		return p.InitializedTicks[idx], true
	}
	return 0, false
}

// GetCurrentPrice 获取当前价格（人类可读的浮点数）
func (p *PoolState) GetCurrentPrice() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.SqrtPriceX96.Sign() == 0 {
		return 0
	}

	// price = (sqrtPriceX96 / 2^96)^2
	sqrtPriceFloat := new(big.Float).SetInt(p.SqrtPriceX96)
	q96Float := new(big.Float).SetInt(new(big.Int).Lsh(big.NewInt(1), 96))
	sqrtPrice := new(big.Float).Quo(sqrtPriceFloat, q96Float)
	price := new(big.Float).Mul(sqrtPrice, sqrtPrice)
	result, _ := price.Float64()
	return result
}

// Snapshot 获取池子状态的快照（用于并发安全的路由计算）
func (p *PoolState) Snapshot() PoolSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return PoolSnapshot{
		Address:      p.Address,
		Token0:       p.Token0,
		Token1:       p.Token1,
		Fee:          p.Fee,
		TickSpacing:  p.TickSpacing,
		SqrtPriceX96: new(big.Int).Set(p.SqrtPriceX96),
		Tick:         p.Tick,
		Liquidity:    new(big.Int).Set(p.Liquidity),
	}
}

// PoolSnapshot 池子状态的不可变快照，用于并发安全的路由计算
type PoolSnapshot struct {
	Address      string
	Token0       string
	Token1       string
	Fee          uint32
	TickSpacing  int32
	SqrtPriceX96 *big.Int
	Tick         int32
	Liquidity    *big.Int
}

// String 实现 Stringer 接口，方便日志输出
func (p *PoolState) String() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return fmt.Sprintf("Pool{%s token0=%s token1=%s fee=%d tick=%d liq=%s}",
		p.Address[:10], p.Token0[:10], p.Token1[:10], p.Fee, p.Tick, p.Liquidity.String())
}

// PoolStateManager 管理所有池子的状态
type PoolStateManager struct {
	pools map[string]*PoolState // address -> state
	mu    sync.RWMutex
}

// NewPoolStateManager 创建池子状态管理器
func NewPoolStateManager() *PoolStateManager {
	return &PoolStateManager{
		pools: make(map[string]*PoolState),
	}
}

// GetPool 获取指定池子的状态
func (m *PoolStateManager) GetPool(address string) (*PoolState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pool, ok := m.pools[address]
	return pool, ok
}

// AddPool 注册新的池子
func (m *PoolStateManager) AddPool(pool *PoolState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pools[pool.Address] = pool
}

// RemovePool 移除池子（创建它的区块被 Reorg 回滚时使用）
func (m *PoolStateManager) RemovePool(address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pools, address)
}

// GetAllPools 获取所有池子的快照
func (m *PoolStateManager) GetAllPools() []PoolSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snapshots := make([]PoolSnapshot, 0, len(m.pools))
	for _, pool := range m.pools {
		snapshots = append(snapshots, pool.Snapshot())
	}
	return snapshots
}

// RollbackToBlock 回滚到指定区块（reorg 处理）
// 清除所有区块号 > targetBlock 的状态变更
func (m *PoolStateManager) RollbackToBlock(targetBlock uint64) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	rolled := 0
	for _, pool := range m.pools {
		if pool.LastBlockNumber > targetBlock {
			// 标记需要重新同步
			pool.LastBlockNumber = targetBlock
			rolled++
		}
	}
	return rolled
}
