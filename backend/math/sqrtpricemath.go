package math

import (
	"errors"
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

// nativeGetAmount0Delta 计算在价格区间 [sqrtPriceA, sqrtPriceB] 内所需的 token0 数量
//
// 公式：amount0 = L × (sqrtPriceB - sqrtPriceA) / (sqrtPriceA × sqrtPriceB)
//
// 参数均为 Q64.96 格式的 big.Int
func nativeGetAmount0Delta(sqrtPriceA, sqrtPriceB, liquidity *big.Int, roundUp bool) *big.Int {
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

// nativeGetAmount1Delta 计算在价格区间内所需的 token1 数量
//
// 公式：amount1 = L × (sqrtPriceB - sqrtPriceA) / Q96
func nativeGetAmount1Delta(sqrtPriceA, sqrtPriceB, liquidity *big.Int, roundUp bool) *big.Int {
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

// ErrSwapMath 对应 Solidity 数学库里 require 失败（链上会 revert）的情况
var ErrSwapMath = errors.New("swap math: on-chain computation would revert")

var (
	two256  = new(big.Int).Lsh(big.NewInt(1), 256)
	mask160 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
)

// nextSqrtPriceFromAmount0RoundingUp 对应 SqrtPriceMath.getNextSqrtPriceFromAmount0RoundingUp
//
// token0 数量变化 amount 后的新价格，向上取整：
//
//	add:    sqrtP' = L × sqrtP / (L + amount × sqrtP)
//	remove: sqrtP' = L × sqrtP / (L - amount × sqrtP)
//
// 链上在 amount × sqrtP 溢出 uint256 时会换一个精度较低的等价公式，这里按同样的条件分支，
// 保证取整结果逐位一致。
func nextSqrtPriceFromAmount0RoundingUp(sqrtPrice, liquidity, amount *big.Int, add bool) (*big.Int, error) {
	if amount.Sign() == 0 {
		return new(big.Int).Set(sqrtPrice), nil
	}
	numerator1 := new(big.Int).Lsh(liquidity, 96)
	product := new(big.Int).Mul(amount, sqrtPrice)

	if add {
		if product.Cmp(two256) < 0 {
			denominator := new(big.Int).Add(numerator1, product)
			if denominator.Cmp(two256) < 0 {
				return MulDivRoundingUp(numerator1, sqrtPrice, denominator), nil
			}
		}
		quotient := new(big.Int).Div(numerator1, sqrtPrice)
		return DivRoundingUp(numerator1, quotient.Add(quotient, amount)), nil
	}

	if product.Cmp(two256) >= 0 || numerator1.Cmp(product) <= 0 {
		return nil, ErrSwapMath
	}
	denominator := new(big.Int).Sub(numerator1, product)
	next := MulDivRoundingUp(numerator1, sqrtPrice, denominator)
	return next.And(next, mask160), nil // 链上是 uint160 截断
}

// nextSqrtPriceFromAmount1RoundingDown 对应 SqrtPriceMath.getNextSqrtPriceFromAmount1RoundingDown
//
// token1 数量变化 amount 后的新价格，向下取整：sqrtP' = sqrtP ± amount / L
func nextSqrtPriceFromAmount1RoundingDown(sqrtPrice, liquidity, amount *big.Int, add bool) (*big.Int, error) {
	if add {
		next := MulDiv(amount, Q96, liquidity)
		next.Add(next, sqrtPrice)
		return next.And(next, mask160), nil
	}
	quotient := MulDivRoundingUp(amount, Q96, liquidity)
	if sqrtPrice.Cmp(quotient) <= 0 {
		return nil, ErrSwapMath
	}
	return quotient.Sub(sqrtPrice, quotient), nil
}

// nativeGetNextSqrtPriceFromInput 投入 amountIn 后的新价格（对应 SqrtPriceMath.getNextSqrtPriceFromInput）
func nativeGetNextSqrtPriceFromInput(sqrtPrice, liquidity, amountIn *big.Int, zeroForOne bool) (*big.Int, error) {
	if sqrtPrice.Sign() <= 0 || liquidity.Sign() <= 0 {
		return nil, ErrSwapMath
	}
	if zeroForOne {
		return nextSqrtPriceFromAmount0RoundingUp(sqrtPrice, liquidity, amountIn, true)
	}
	return nextSqrtPriceFromAmount1RoundingDown(sqrtPrice, liquidity, amountIn, true)
}

// nativeGetNextSqrtPriceFromOutput 取出 amountOut 后的新价格（对应 SqrtPriceMath.getNextSqrtPriceFromOutput）
func nativeGetNextSqrtPriceFromOutput(sqrtPrice, liquidity, amountOut *big.Int, zeroForOne bool) (*big.Int, error) {
	if sqrtPrice.Sign() <= 0 || liquidity.Sign() <= 0 {
		return nil, ErrSwapMath
	}
	if zeroForOne {
		return nextSqrtPriceFromAmount1RoundingDown(sqrtPrice, liquidity, amountOut, false)
	}
	return nextSqrtPriceFromAmount0RoundingUp(sqrtPrice, liquidity, amountOut, false)
}

// nativeComputeSwapStep 单步 swap 计算，逐行对应 Solidity 的 SwapMath.computeSwapStep
//
// 在一个流动性不变的价格区间内，从 sqrtPriceCurrent 朝 sqrtPriceTarget 交换：
//   - amountRemaining > 0：精确输入，剩余待投入的数量（含手续费）
//   - amountRemaining < 0：精确输出，剩余待取出的数量
//
// 返回值：(sqrtPriceNext, amountIn, amountOut, feeAmount)。
// 取整方向与链上完全相同：输入向上取整、输出向下取整，池子永远不吃亏。
func nativeComputeSwapStep(
	sqrtPriceCurrent, sqrtPriceTarget *big.Int,
	liquidity *big.Int,
	amountRemaining *big.Int,
	feePips uint32,
) (sqrtPriceNext, amountIn, amountOut, feeAmount *big.Int, err error) {
	zeroForOne := sqrtPriceCurrent.Cmp(sqrtPriceTarget) >= 0
	exactIn := amountRemaining.Sign() >= 0
	absRemaining := new(big.Int).Abs(amountRemaining)

	fee := big.NewInt(int64(feePips))
	million := big.NewInt(1e6)
	feeComplement := new(big.Int).Sub(million, fee)

	if exactIn {
		// 扣除手续费后真正进池的输入
		amountRemainingLessFee := MulDiv(absRemaining, feeComplement, million)
		// 走到目标价格需要的输入
		if zeroForOne {
			amountIn = nativeGetAmount0Delta(sqrtPriceTarget, sqrtPriceCurrent, liquidity, true)
		} else {
			amountIn = nativeGetAmount1Delta(sqrtPriceCurrent, sqrtPriceTarget, liquidity, true)
		}
		if amountRemainingLessFee.Cmp(amountIn) >= 0 {
			sqrtPriceNext = new(big.Int).Set(sqrtPriceTarget)
		} else {
			// 输入不够走到目标价格：算出实际能推到的价格
			sqrtPriceNext, err = nativeGetNextSqrtPriceFromInput(sqrtPriceCurrent, liquidity, amountRemainingLessFee, zeroForOne)
		}
	} else {
		// 走到目标价格能取出的输出
		if zeroForOne {
			amountOut = nativeGetAmount1Delta(sqrtPriceTarget, sqrtPriceCurrent, liquidity, false)
		} else {
			amountOut = nativeGetAmount0Delta(sqrtPriceCurrent, sqrtPriceTarget, liquidity, false)
		}
		if absRemaining.Cmp(amountOut) >= 0 {
			sqrtPriceNext = new(big.Int).Set(sqrtPriceTarget)
		} else {
			sqrtPriceNext, err = nativeGetNextSqrtPriceFromOutput(sqrtPriceCurrent, liquidity, absRemaining, zeroForOne)
		}
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// 是否走到了目标价格；没走到时按实际价格重算两侧数量
	reachedTarget := sqrtPriceTarget.Cmp(sqrtPriceNext) == 0
	if zeroForOne {
		if !(reachedTarget && exactIn) {
			amountIn = nativeGetAmount0Delta(sqrtPriceNext, sqrtPriceCurrent, liquidity, true)
		}
		if !(reachedTarget && !exactIn) {
			amountOut = nativeGetAmount1Delta(sqrtPriceNext, sqrtPriceCurrent, liquidity, false)
		}
	} else {
		if !(reachedTarget && exactIn) {
			amountIn = nativeGetAmount1Delta(sqrtPriceCurrent, sqrtPriceNext, liquidity, true)
		}
		if !(reachedTarget && !exactIn) {
			amountOut = nativeGetAmount0Delta(sqrtPriceCurrent, sqrtPriceNext, liquidity, false)
		}
	}

	// 精确输出时，输出不能超过剩余要取的数量
	if !exactIn && amountOut.Cmp(absRemaining) > 0 {
		amountOut = absRemaining
	}

	if exactIn && !reachedTarget {
		// 没走到目标价格：剩下的输入全部算作手续费
		feeAmount = new(big.Int).Sub(absRemaining, amountIn)
	} else {
		feeAmount = MulDivRoundingUp(amountIn, fee, feeComplement)
	}
	return sqrtPriceNext, amountIn, amountOut, feeAmount, nil
}
