package oracle

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"unigo-dex/indexer"
	umath "unigo-dex/math"
	"unigo-dex/metrics"
)

// oracle.go — 价格预言机验证层
//
// === 设计理念：不盲信任何单一价格源 ===
//
// 在 DeFi 系统中，价格信息来自多个源头：
//   1. 链上池内现价（AMM sqrtPriceX96）—— 实时，但能在单个区块内被闪电贷拉偏
//   2. 池内 TWAP —— 同一个池子的时间加权均价，要整个窗口都顶住价格才会动
//   3. Chainlink 预言机价格 —— 独立于池子，可信但有滞后（心跳间隔 1-24 小时）
//
// 本模块实现 "有效性校验 + 保护模式"，确保价格安全：
//
//   1. Chainlink 有效性检查（见 chainlink.go）
//      - 不只读 answer，还校验 updatedAt、轮次是否完成、是否超过心跳阈值
//      - 不可用的喂价不参与校验，三方降级为两方
//
//   2. 三方交叉验证：池内现价 / 池内 TWAP / Chainlink
//      - 现价 vs Chainlink：偏离超过 MaxDeviation 触发保护模式
//      - TWAP vs Chainlink：TWAP 也偏了，说明不是瞬时波动，同样触发保护模式
//      - 现价 vs TWAP：现价能在一个区块内被闪电贷拉偏，TWAP 要整个窗口都顶住才会动，
//        两者偏离超过 MaxTWAPDeviation 视为疑似单区块操纵，交易前检查直接拒绝
//
//   3. 保护模式（Fail-safe）
//      - 任意一组价格偏离超过阈值就进入，至少持续 ProtectionCooldown
//      - 期间要求请求带足够的滑点保护，否则拒绝

// PriceSource 价格来源标识
type PriceSource int

const (
	SourceChainlink PriceSource = iota
	SourcePool
	SourceTWAP
)

// PriceData 价格数据
type PriceData struct {
	Price     float64     // 价格
	Source    PriceSource // 来源
	UpdatedAt time.Time   // 更新时间
}

// OracleConfig 预言机配置
type OracleConfig struct {
	// MaxDeviation 最大允许偏离度（百分比）
	// 池内价格与 Chainlink 偏离超过此值触发保护模式
	MaxDeviation float64

	// MaxTWAPDeviation 现价相对 TWAP 的最大偏离，超过视为疑似单区块操纵
	MaxTWAPDeviation float64

	// ProtectionCooldown 保护模式冷却时间
	// 触发保护模式后，至少持续此时间
	ProtectionCooldown time.Duration
}

// DefaultOracleConfig 返回默认配置
func DefaultOracleConfig() OracleConfig {
	return OracleConfig{
		MaxDeviation:       0.02,            // 2% 偏离触发保护
		MaxTWAPDeviation:   0.05,            // 现价偏离 TWAP 5% 视为疑似操纵
		ProtectionCooldown: 5 * time.Minute, // 保护模式至少 5 分钟
	}
}

// ProtectionStatus 保护模式状态
type ProtectionStatus struct {
	Active      bool      // 是否处于保护模式
	Reason      string    // 触发原因
	ActivatedAt time.Time // 激活时间
	Deviation   float64   // 当前偏离度
}

// Oracle 价格预言机验证器
type Oracle struct {
	config OracleConfig

	// 各来源的最新价格（token pair -> PriceData）
	chainlinkPrices map[string]*PriceData
	poolPrices      map[string]*PriceData
	twapPrices      map[string]*PriceData

	// 保护模式状态
	protectionStatus map[string]*ProtectionStatus

	mu sync.RWMutex
}

// NewOracle 创建预言机实例
func NewOracle(config OracleConfig) *Oracle {
	return &Oracle{
		config:           config,
		chainlinkPrices:  make(map[string]*PriceData),
		poolPrices:       make(map[string]*PriceData),
		twapPrices:       make(map[string]*PriceData),
		protectionStatus: make(map[string]*ProtectionStatus),
	}
}

// UpdatePoolPrice 更新池内价格
func (o *Oracle) UpdatePoolPrice(pair string, price float64) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.poolPrices[pair] = &PriceData{
		Price:     price,
		Source:    SourcePool,
		UpdatedAt: time.Now(),
	}

	// 交叉验证：与 Chainlink、TWAP 对比
	o.crossValidate(pair)
}

// crossValidate 三方交叉验证：池内现价 / TWAP / Chainlink
//
// 任意一组偏离超过阈值就进入保护模式，全部正常才尝试退出（受冷却时间约束）。
// 缺少的价格源不参与比较。
func (o *Oracle) crossValidate(pair string) {
	chainlink := o.chainlinkPrices[pair]
	pool := o.poolPrices[pair]
	twap := o.twapPrices[pair]

	deviation := func(a, b *PriceData) float64 { return math.Abs(a.Price-b.Price) / b.Price }

	var reason string
	switch {
	case pool != nil && chainlink != nil && deviation(pool, chainlink) > o.config.MaxDeviation:
		reason = fmt.Sprintf(
			"池内价格与 Chainlink 偏离 %.2f%%（阈值 %.2f%%）: 池内=%.6f, Chainlink=%.6f",
			deviation(pool, chainlink)*100, o.config.MaxDeviation*100, pool.Price, chainlink.Price)
	case twap != nil && chainlink != nil && deviation(twap, chainlink) > o.config.MaxDeviation:
		reason = fmt.Sprintf(
			"池内 TWAP 与 Chainlink 偏离 %.2f%%（阈值 %.2f%%）: TWAP=%.6f, Chainlink=%.6f",
			deviation(twap, chainlink)*100, o.config.MaxDeviation*100, twap.Price, chainlink.Price)
	case pool != nil && twap != nil && deviation(pool, twap) > o.config.MaxTWAPDeviation:
		reason = fmt.Sprintf(
			"池内现价与 TWAP 偏离 %.2f%%（阈值 %.2f%%），疑似单区块价格操纵: 现价=%.6f, TWAP=%.6f",
			deviation(pool, twap)*100, o.config.MaxTWAPDeviation*100, pool.Price, twap.Price)
	}

	if reason != "" {
		o.activateProtection(pair, reason)
	} else if pool != nil && (chainlink != nil || twap != nil) {
		o.tryDeactivateProtection(pair)
	}
}

// TWAP 校验结果
const (
	TWAPOK          = "ok"          // 现价与 TWAP 一致
	TWAPDeviated    = "deviated"    // 现价偏离 TWAP 过大，疑似被操纵
	TWAPUnavailable = "unavailable" // 取不到 TWAP（观测不足或节点不可用），无法判断
)

// TWAPCheck 用 TWAP 校验现价的结果
type TWAPCheck struct {
	Status    string  `json:"status"`
	Spot      float64 `json:"spot"`
	TWAP      float64 `json:"twap,omitempty"`
	Deviation float64 `json:"deviation"` // |现价 - TWAP| / TWAP
}

// CheckTWAP 用 TWAP 校验池内现价是否可能被单区块操纵，并把两个价格纳入三方交叉验证
//
// twap <= 0 表示取不到 TWAP：此时无法判断，返回 unavailable，由调用方提示用户而不是放行为"正常"。
func (o *Oracle) CheckTWAP(pair string, spot, twap float64) TWAPCheck {
	metrics.OracleTWAPChecks.Inc()
	if twap <= 0 {
		metrics.OracleTWAPUnavailable.Inc()
		return TWAPCheck{Status: TWAPUnavailable, Spot: spot}
	}

	o.mu.Lock()
	now := time.Now()
	o.poolPrices[pair] = &PriceData{Price: spot, Source: SourcePool, UpdatedAt: now}
	o.twapPrices[pair] = &PriceData{Price: twap, Source: SourceTWAP, UpdatedAt: now}
	o.crossValidate(pair)
	o.mu.Unlock()

	check := TWAPCheck{Status: TWAPOK, Spot: spot, TWAP: twap, Deviation: math.Abs(spot-twap) / twap}
	if check.Deviation > o.config.MaxTWAPDeviation {
		check.Status = TWAPDeviated
		metrics.OracleTWAPDeviations.Inc()
	}
	return check
}

// ChainlinkCheck 用 Chainlink 喂价校验池内现价的结果
type ChainlinkCheck struct {
	Status    string  // ok / deviated / unavailable / none
	Price     float64 // 喂价（池子口径）
	Deviation float64 // |现价 - 喂价| / 喂价
	Detail    string  // 不可用时的原因
}

// CheckChainlink 用一次喂价读数校验池内现价，并更新三方交叉验证里 Chainlink 这一方
//
//   - 喂价有效：记入交叉验证；现价偏离超过 MaxDeviation 返回 deviated
//   - 喂价不可用：把这个币对的 Chainlink 价格清掉，交叉验证降级为现价 vs TWAP 两方
//   - 没有配置喂价：什么都不动
func (o *Oracle) CheckChainlink(pair string, spot float64, reading FeedReading) ChainlinkCheck {
	check := ChainlinkCheck{Status: reading.Status, Detail: reading.Detail}
	switch reading.Status {
	case ChainlinkOK:
		o.mu.Lock()
		o.chainlinkPrices[pair] = &PriceData{
			Price: reading.Price, Source: SourceChainlink, UpdatedAt: time.Unix(int64(reading.UpdatedAt), 0),
		}
		o.poolPrices[pair] = &PriceData{Price: spot, Source: SourcePool, UpdatedAt: time.Now()}
		o.crossValidate(pair)
		o.mu.Unlock()

		check.Price = reading.Price
		check.Deviation = math.Abs(spot-reading.Price) / reading.Price
		if check.Deviation > o.config.MaxDeviation {
			check.Status = ChainlinkDeviated
			metrics.OracleChainlinkDeviations.Inc()
		}
	case ChainlinkUnavailable:
		o.mu.Lock()
		delete(o.chainlinkPrices, pair)
		o.mu.Unlock()
		metrics.OracleChainlinkUnavailable.Inc()
	}
	return check
}

// ProtectedPairs 当前处于保护模式的交易对（按字典序）
func (o *Oracle) ProtectedPairs() []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var pairs []string
	for pair, status := range o.protectionStatus {
		if status.Active {
			pairs = append(pairs, pair)
		}
	}
	sort.Strings(pairs)
	return pairs
}

// MaxDeviation 现价相对 Chainlink 的最大允许偏离
func (o *Oracle) MaxDeviation() float64 { return o.config.MaxDeviation }

// PairKey 交易对在预言机里的 key：按池子的 token0/token1 顺序，价格是 token1/token0
func PairKey(token0, token1 string) string {
	return token0 + "/" + token1
}

// poolPriceFeed 把索引器的 Swap 事件喂给预言机做交叉验证
type poolPriceFeed struct {
	oracle *Oracle
	state  *indexer.PoolStateManager
}

// PoolPriceFeed 返回一个订阅 Swap 事件流的监听器：每笔成交后更新对应交易对的池内价格
func (o *Oracle) PoolPriceFeed(state *indexer.PoolStateManager) indexer.SwapListener {
	return &poolPriceFeed{oracle: o, state: state}
}

func (f *poolPriceFeed) OnSwap(e *indexer.SwapEvent) {
	pool, ok := f.state.GetPool(e.PoolAddress)
	if !ok {
		return
	}
	if price := umath.SqrtPriceX96ToPrice(e.SqrtPriceX96); price > 0 {
		f.oracle.UpdatePoolPrice(PairKey(pool.Token0, pool.Token1), price)
	}
}

// OnReorg 价格是瞬时值，回滚后的成交会重新推送，不需要额外处理
func (f *poolPriceFeed) OnReorg(uint64) {}

// activateProtection 激活保护模式
func (o *Oracle) activateProtection(pair string, reason string) {
	status, exists := o.protectionStatus[pair]
	if exists && status.Active {
		// 更新原因
		status.Reason = reason
		return
	}

	o.protectionStatus[pair] = &ProtectionStatus{
		Active:      true,
		Reason:      reason,
		ActivatedAt: time.Now(),
	}
}

// tryDeactivateProtection 尝试解除保护模式
func (o *Oracle) tryDeactivateProtection(pair string) {
	status, exists := o.protectionStatus[pair]
	if !exists || !status.Active {
		return
	}

	// 冷却时间未到，不解除
	if time.Since(status.ActivatedAt) < o.config.ProtectionCooldown {
		return
	}

	status.Active = false
}

// ValidateSwap 在执行 swap 前验证价格安全性
//
// 返回值：
//   - allowed: 是否允许执行
//   - maxSlippage: 建议的最大滑点
//   - warning: 警告信息
func (o *Oracle) ValidateSwap(pair string, requestedSlippage float64) (allowed bool, maxSlippage float64, warning string) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	status, inProtection := o.protectionStatus[pair]

	if inProtection && status.Active {
		// 保护模式下：
		// 不直接拒绝，但强制用户使用严格滑点
		minSlippage := 0.005 // 保护模式下最小滑点 0.5%

		if requestedSlippage < minSlippage {
			return false, minSlippage, fmt.Sprintf(
				"保护模式已激活（%s）。当前价格可能不可靠，请设置至少 %.1f%% 的滑点保护。",
				status.Reason, minSlippage*100)
		}

		return true, requestedSlippage, fmt.Sprintf(
			"注意：保护模式已激活（%s）。请确认你了解风险。", status.Reason)
	}

	return true, requestedSlippage, ""
}

// GetProtectionStatus 获取指定交易对的保护模式状态
func (o *Oracle) GetProtectionStatus(pair string) *ProtectionStatus {
	o.mu.RLock()
	defer o.mu.RUnlock()

	status, exists := o.protectionStatus[pair]
	if !exists {
		return &ProtectionStatus{Active: false}
	}
	return status
}

// GetPriceInfo 获取综合价格信息
func (o *Oracle) GetPriceInfo(pair string) map[string]*PriceData {
	o.mu.RLock()
	defer o.mu.RUnlock()

	result := make(map[string]*PriceData)
	if p, ok := o.chainlinkPrices[pair]; ok {
		result["chainlink"] = p
	}
	if p, ok := o.poolPrices[pair]; ok {
		result["pool"] = p
	}
	if p, ok := o.twapPrices[pair]; ok {
		result["twap"] = p
	}
	return result
}
