package gas

import (
	"context"
	"log/slog"
	"math/big"
	"sync"
	"time"
)

// gas.go — Gas 成本评估（EIP-1559 口径）
//
// EIP-1559 之后一笔交易每单位 gas 的实际价格是：
//
//	gasPrice = baseFee + priorityFee
//
//   - baseFee：协议按上一个区块的拥堵程度算出，写在区块头里，会被销毁
//   - priorityFee：给出块者的小费，取节点建议值（eth_maxPriorityFeePerGas）
//
// 钱包提交交易时还要给一个上限 maxFeePerGas。baseFee 每个区块最多涨 12.5%，
// 取 2 × baseFee + priorityFee 可以保证连续 6 个满块之内交易仍能被打包。
//
// 成本 = 路由给出的 gas 用量 × gasPrice。
// 没有配置节点或节点不可用时退回静态估算，报价接口不会因此失败。

// FeeSource 当前链上费用的来源（chain.Client 实现了它）
type FeeSource interface {
	Fees(ctx context.Context) (baseFee, priorityFee *big.Int, err error)
}

// Config Gas 评估配置
type Config struct {
	StaticBaseFee     *big.Int      // 静态估算用的 baseFee（wei）
	StaticPriorityFee *big.Int      // 静态估算用的小费（wei）
	CacheTTL          time.Duration // 链上费用的缓存时间
}

// DefaultConfig 返回默认配置
func DefaultConfig() Config {
	return Config{
		StaticBaseFee:     big.NewInt(20e9), // 20 gwei
		StaticPriorityFee: big.NewInt(15e8), // 1.5 gwei
		CacheTTL:          5 * time.Second,  // 小于一个出块间隔
	}
}

// Estimate 一次 Gas 成本评估的结果，金额单位均为 wei
type Estimate struct {
	GasLimit     uint64 `json:"gas_limit"`       // 预估 gas 用量
	BaseFee      string `json:"base_fee"`        // 区块 baseFee
	PriorityFee  string `json:"priority_fee"`    // 建议小费
	GasPrice     string `json:"gas_price"`       // 预期成交价 = baseFee + priorityFee
	MaxFeePerGas string `json:"max_fee_per_gas"` // 建议的 maxFeePerGas = 2 × baseFee + priorityFee
	Cost         string `json:"cost"`            // 预期成本 = gasLimit × gasPrice
	MaxCost      string `json:"max_cost"`        // 最坏成本 = gasLimit × maxFeePerGas
	Source       string `json:"source"`          // chain = 取自节点，static = 静态估算
}

// Estimator Gas 成本评估器
type Estimator struct {
	cfg Config
	src FeeSource

	mu          sync.Mutex
	fetchedAt   time.Time
	baseFee     *big.Int
	priorityFee *big.Int
}

// NewEstimator 创建评估器，src 为 nil 时始终使用静态估算
func NewEstimator(cfg Config, src FeeSource) *Estimator {
	return &Estimator{cfg: cfg, src: src}
}

// Estimate 评估消耗 gasLimit 单位 gas 的交易成本
func (e *Estimator) Estimate(ctx context.Context, gasLimit uint64) Estimate {
	baseFee, priorityFee, source := e.fees(ctx)

	gasPrice := new(big.Int).Add(baseFee, priorityFee)
	maxFee := new(big.Int).Add(new(big.Int).Lsh(baseFee, 1), priorityFee)
	limit := new(big.Int).SetUint64(gasLimit)

	return Estimate{
		GasLimit:     gasLimit,
		BaseFee:      baseFee.String(),
		PriorityFee:  priorityFee.String(),
		GasPrice:     gasPrice.String(),
		MaxFeePerGas: maxFee.String(),
		Cost:         new(big.Int).Mul(limit, gasPrice).String(),
		MaxCost:      new(big.Int).Mul(limit, maxFee).String(),
		Source:       source,
	}
}

// CostWei 消耗 gasLimit 单位 gas 的预期成本（wei）= gasLimit × (baseFee + priorityFee)
func (e *Estimator) CostWei(ctx context.Context, gasLimit uint64) *big.Int {
	baseFee, priorityFee, _ := e.fees(ctx)
	cost := new(big.Int).Add(baseFee, priorityFee)
	return cost.Mul(cost, new(big.Int).SetUint64(gasLimit))
}

// fees 返回当前费用：优先用缓存内的链上数据，取不到时退回静态值
func (e *Estimator) fees(ctx context.Context) (baseFee, priorityFee *big.Int, source string) {
	if e.src == nil {
		return e.cfg.StaticBaseFee, e.cfg.StaticPriorityFee, "static"
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.baseFee != nil && time.Since(e.fetchedAt) < e.cfg.CacheTTL {
		return e.baseFee, e.priorityFee, "chain"
	}

	baseFee, priorityFee, err := e.src.Fees(ctx)
	if err != nil {
		slog.Warn("获取链上 Gas 费用失败，使用静态估算", "err", err)
		return e.cfg.StaticBaseFee, e.cfg.StaticPriorityFee, "static"
	}
	e.baseFee, e.priorityFee, e.fetchedAt = baseFee, priorityFee, time.Now()
	return baseFee, priorityFee, "chain"
}
