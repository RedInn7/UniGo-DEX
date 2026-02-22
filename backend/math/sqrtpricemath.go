package math

import (
	"math/big"
)

// SqrtPriceMath 链下复现 Solidity SqrtPriceMath.sol 的计算
//
// 核心公式（来自 x * y = L^2 恒等式的变形）：
//   token0 数量 = L × (1/sqrtP_lower - 1/sqrtP_upper)
//   token1 数量 = L × (sqrtP_upper - sqrtP_lower)
//
// 这些函数用于：
//   - 链下模拟 swap 时计算每一步的 token 数量
//   - 路由引擎评估不同路径的预期产出
//   - 报价服务快速返回 quote 而无需调用链上合约

// GetAmount0Delta 计算在价格区间 [sqrtPriceA, sqrtPriceB] 内所需的 token0 数量
//
// 公式：amount0 = L × (sqrtPriceB - sqrtPriceA) / (sqrtPriceA × sqrtPriceB)
//
// 参数均为 Q64.96 格式的 big.Int
func GetAmount0Delta(sqrtPriceA, sqrtPriceB, liquidity *big.Int, roundUp bool) *big.Int {
	// 确保 a < b
	if sqrtPriceA.Cmp(sqrtPriceB) > 0 {
		sqrtPriceA, sqrtPriceB = sqrtPriceB, sqrtPriceA
	}

	// numerator1 = liquidity << 96
	numerator1 := new(big.Int).Lsh(liquidity, 96)

	// numerator2 = sqrtPriceB - sqrtPriceA
	numerator2 := new(big.Int).Sub(sqrtPriceB, sqrtPriceA)

	// amount0 = (numerator1 × numerator2 / sqrtPriceB) / sqrtPriceA
	if roundUp {
		// 向上取整：用于计算用户需要存入的数量
		inner := MulDivRoundingUp(numerator1, numerator2, sqrtPriceB)
		return DivRoundingUp(inner, sqrtPriceA)
	}
	// 向下取整：用于计算用户可以提取的数量
	inner := MulDiv(numerator1, numerator2, sqrtPriceB)
	return new(big.Int).Div(inner, sqrtPriceA)
}

// GetAmount1Delta 计算在价格区间内所需的 token1 数量
//
// 公式：amount1 = L × (sqrtPriceB - sqrtPriceA) / Q96
func GetAmount1Delta(sqrtPriceA, sqrtPriceB, liquidity *big.Int, roundUp bool) *big.Int {
	if sqrtPriceA.Cmp(sqrtPriceB) > 0 {
		sqrtPriceA, sqrtPriceB = sqrtPriceB, sqrtPriceA
	}

	diff := new(big.Int).Sub(sqrtPriceB, sqrtPriceA)

	if roundUp {
		return MulDivRoundingUp(liquidity, diff, Q96)
	}
	return MulDiv(liquidity, diff, Q96)
}

// MulDiv 全精度乘除：(a × b) / denominator，向下取整
// Go 的 math/big 天然支持任意精度，不需要像 Solidity 那样手动处理 512 位
func MulDiv(a, b, denominator *big.Int) *big.Int {
	product := new(big.Int).Mul(a, b)
	return new(big.Int).Div(product, denominator)
}

// MulDivRoundingUp 全精度乘除，向上取整
func MulDivRoundingUp(a, b, denominator *big.Int) *big.Int {
	product := new(big.Int).Mul(a, b)
	result := new(big.Int).Div(product, denominator)
	remainder := new(big.Int).Mod(product, denominator)
	if remainder.Sign() > 0 {
		result.Add(result, big.NewInt(1))
	}
	return result
}

// DivRoundingUp 向上取整除法
func DivRoundingUp(a, b *big.Int) *big.Int {
	result := new(big.Int).Div(a, b)
	remainder := new(big.Int).Mod(a, b)
	if remainder.Sign() > 0 {
		result.Add(result, big.NewInt(1))
	}
	return result
}

// ComputeSwapStep 链下复现单步 swap 计算
//
// 返回值：(sqrtPriceNext, amountIn, amountOut, feeAmount)
func ComputeSwapStep(
	sqrtPriceCurrent, sqrtPriceTarget *big.Int,
	liquidity *big.Int,
	amountRemaining *big.Int,
	feePips uint32,
) (sqrtPriceNext, amountIn, amountOut, feeAmount *big.Int) {
	zeroForOne := sqrtPriceCurrent.Cmp(sqrtPriceTarget) >= 0
	exactIn := amountRemaining.Sign() >= 0

	fee := big.NewInt(int64(feePips))
	million := big.NewInt(1e6)

	if exactIn {
		// 扣除手续费后的可用输入
		amountRemainingLessFee := MulDiv(
			new(big.Int).Abs(amountRemaining),
			new(big.Int).Sub(million, fee),
			million,
		)

		// 计算到达目标价格需要的输入
		if zeroForOne {
			amountIn = GetAmount0Delta(sqrtPriceTarget, sqrtPriceCurrent, liquidity, true)
		} else {
			amountIn = GetAmount1Delta(sqrtPriceCurrent, sqrtPriceTarget, liquidity, true)
		}

		if amountRemainingLessFee.Cmp(amountIn) >= 0 {
			sqrtPriceNext = new(big.Int).Set(sqrtPriceTarget)
		} else {
			// 输入不足以到达目标价格，计算实际能到达的价格
			sqrtPriceNext = new(big.Int).Set(sqrtPriceTarget) // 简化：使用目标价格
			amountIn = new(big.Int).Set(amountRemainingLessFee)
		}

		// 计算输出
		if zeroForOne {
			amountOut = GetAmount1Delta(sqrtPriceNext, sqrtPriceCurrent, liquidity, false)
		} else {
			amountOut = GetAmount0Delta(sqrtPriceCurrent, sqrtPriceNext, liquidity, false)
		}

		// 手续费
		feeAmount = MulDivRoundingUp(amountIn, fee, new(big.Int).Sub(million, fee))
	} else {
		// exactOutput 模式（简化实现）
		if zeroForOne {
			amountOut = GetAmount1Delta(sqrtPriceTarget, sqrtPriceCurrent, liquidity, false)
		} else {
			amountOut = GetAmount0Delta(sqrtPriceCurrent, sqrtPriceTarget, liquidity, false)
		}

		absRemaining := new(big.Int).Abs(amountRemaining)
		if absRemaining.Cmp(amountOut) >= 0 {
			sqrtPriceNext = new(big.Int).Set(sqrtPriceTarget)
		} else {
			sqrtPriceNext = new(big.Int).Set(sqrtPriceTarget)
			amountOut = new(big.Int).Set(absRemaining)
		}

		if zeroForOne {
			amountIn = GetAmount0Delta(sqrtPriceNext, sqrtPriceCurrent, liquidity, true)
		} else {
			amountIn = GetAmount1Delta(sqrtPriceCurrent, sqrtPriceNext, liquidity, true)
		}
		feeAmount = MulDivRoundingUp(amountIn, fee, new(big.Int).Sub(million, fee))
	}

	return
}
