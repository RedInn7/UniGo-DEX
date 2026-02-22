package math

import "math/big"

// UniGo-DEX 链下数学库 — 常量定义
//
// 所有常量与 Solidity 合约中的定义严格一致，
// 确保链上链下计算结果完全匹配（链下复现）。

var (
	// Q96 = 2^96，Q64.96 定点数的缩放因子
	// Solidity 中: uint256 constant Q96 = 0x1000000000000000000000000
	Q96 = new(big.Int).Lsh(big.NewInt(1), 96)

	// Q128 = 2^128，用于 TickMath 中间计算
	Q128 = new(big.Int).Lsh(big.NewInt(1), 128)

	// MaxUint256 = 2^256 - 1
	MaxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

	// MinTick 最小 tick 值（对应最低价格）
	MinTick int32 = -887272

	// MaxTick 最大 tick 值（对应最高价格）
	MaxTick int32 = 887272

	// MinSqrtRatio 最小 sqrtPriceX96（对应 MinTick）
	MinSqrtRatio = big.NewInt(4295128739)

	// MaxSqrtRatio 最大 sqrtPriceX96（对应 MaxTick）
	MaxSqrtRatio, _ = new(big.Int).SetString("1461446703485210103287273052203988822378723970342", 10)
)

// 常用费率常量（百万分之一）
const (
	Fee500   uint32 = 500   // 0.05% — 适合稳定币对
	Fee3000  uint32 = 3000  // 0.3%  — 标准交易对
	Fee10000 uint32 = 10000 // 1%    — 高波动交易对
)

// FeeToTickSpacing 费率到 tickSpacing 的映射
var FeeToTickSpacing = map[uint32]int32{
	Fee500:   10,
	Fee3000:  60,
	Fee10000: 200,
}
