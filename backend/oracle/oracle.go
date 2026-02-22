package oracle

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// oracle.go — 价格预言机验证层
//
// === 设计理念：不盲信任何单一价格源 ===
//
// 在 DeFi 系统中，价格信息来自多个源头：
//   1. 链上池内价格（AMM sqrtPriceX96）—— 实时但可被操纵（闪电贷攻击）
//   2. Chainlink 预言机价格 —— 可信但有滞后（心跳间隔 1-24 小时）
//   3. 链下 CEX 价格 —— 参考用
//
// 本模块实现 "有效性校验 + 保护模式"，确保价格安全：
//
//   1. Chainlink 有效性检查
//      - 不仅读取 price，还校验 updatedAt
//      - 若 CurrentTime - updatedAt > StaleThreshold，判定价格失效
//
//   2. 交叉验证
//      - 对比 Chainlink 价格与池内价格
//      - 偏离超过 MaxDeviation 触发保护模式
//
//   3. 保护模式（Fail-safe）
//      - 暂停自动路由
//      - 强制用户手动设置严格滑点
//      - 防止利用过期/被操纵价格进行套利

// PriceSource 价格来源标识
type PriceSource int

const (
	SourceChainlink PriceSource = iota
	SourcePool
	SourceCEX
)

// PriceData 价格数据
type PriceData struct {
	Price     float64     // 价格
	Source    PriceSource // 来源
	UpdatedAt time.Time   // 更新时间
	IsStale   bool        // 是否已过期
}

// OracleConfig 预言机配置
type OracleConfig struct {
	// StaleThreshold Chainlink 价格过期阈值
	// 若 time.Now() - updatedAt > StaleThreshold，判定价格失效
	StaleThreshold time.Duration

	// MaxDeviation 最大允许偏离度（百分比）
	// 池内价格与 Chainlink 偏离超过此值触发保护模式
	MaxDeviation float64

	// ProtectionCooldown 保护模式冷却时间
	// 触发保护模式后，至少持续此时间
	ProtectionCooldown time.Duration
}

// DefaultOracleConfig 返回默认配置
func DefaultOracleConfig() OracleConfig {
	return OracleConfig{
		StaleThreshold:     1 * time.Hour,   // Chainlink 1 小时未更新视为过期
		MaxDeviation:       0.02,             // 2% 偏离触发保护
		ProtectionCooldown: 5 * time.Minute,  // 保护模式至少 5 分钟
	}
}

// ProtectionStatus 保护模式状态
type ProtectionStatus struct {
	Active     bool      // 是否处于保护模式
	Reason     string    // 触发原因
	ActivatedAt time.Time // 激活时间
	Deviation  float64   // 当前偏离度
}

// Oracle 价格预言机验证器
type Oracle struct {
	config OracleConfig

	// 各来源的最新价格（token pair -> PriceData）
	chainlinkPrices map[string]*PriceData
	poolPrices      map[string]*PriceData

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
		protectionStatus: make(map[string]*ProtectionStatus),
	}
}

// UpdateChainlinkPrice 更新 Chainlink 预言机价格
//
// 关键：不只存储价格，还记录 updatedAt 用于有效性校验
func (o *Oracle) UpdateChainlinkPrice(pair string, price float64, updatedAt time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()

	isStale := time.Since(updatedAt) > o.config.StaleThreshold

	o.chainlinkPrices[pair] = &PriceData{
		Price:     price,
		Source:    SourceChainlink,
		UpdatedAt: updatedAt,
		IsStale:   isStale,
	}

	if isStale {
		o.activateProtection(pair, fmt.Sprintf(
			"Chainlink 价格过期：上次更新于 %v（阈值 %v）",
			updatedAt.Format(time.RFC3339), o.config.StaleThreshold))
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
		IsStale:   false,
	}

	// 交叉验证：与 Chainlink 价格对比
	o.crossValidate(pair)
}

// crossValidate 交叉验证池内价格与 Chainlink 价格
//
// 当偏离超过 MaxDeviation 时：
//   - 可能是闪电贷攻击导致池内价格被操纵
//   - 可能是 Chainlink 更新滞后
//   - 无论哪种情况，都应该触发保护模式
func (o *Oracle) crossValidate(pair string) {
	chainlink, hasChainlink := o.chainlinkPrices[pair]
	pool, hasPool := o.poolPrices[pair]

	if !hasChainlink || !hasPool {
		return
	}

	// 跳过过期的 Chainlink 价格
	if chainlink.IsStale {
		return
	}

	// 计算偏离度
	deviation := math.Abs(pool.Price-chainlink.Price) / chainlink.Price

	if deviation > o.config.MaxDeviation {
		o.activateProtection(pair, fmt.Sprintf(
			"池内价格与 Chainlink 偏离 %.2f%%（阈值 %.2f%%）: 池内=%.6f, Chainlink=%.6f",
			deviation*100, o.config.MaxDeviation*100,
			pool.Price, chainlink.Price))
	} else {
		// 偏离在正常范围内，检查是否可以解除保护
		o.tryDeactivateProtection(pair)
	}
}

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
				"⚠️ 保护模式已激活（%s）。当前价格可能不可靠，请设置至少 %.1f%% 的滑点保护。",
				status.Reason, minSlippage*100)
		}

		return true, requestedSlippage, fmt.Sprintf(
			"⚠️ 注意：保护模式已激活（%s）。请确认你了解风险。", status.Reason)
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
	return result
}
