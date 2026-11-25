package router

import (
	"errors"
	"math/big"
	"sort"

	"unigo-dex/indexer"
	umath "unigo-dex/math"
)

// quote.go — 链下报价引擎
//
// 链下报价的优势（相比调用链上 Quoter 合约）：
//   1. 零 Gas 消耗：纯内存计算
//   2. 低延迟：不需要等待 RPC 调用和 EVM 执行
//   3. 高吞吐：可以并发处理大量报价请求
//
// 与链上逐位一致的关键：
//   - 同样的 Q64.96 定点数和取整方向（math 包逐行对应 SwapMath / SqrtPriceMath）
//   - 同样的"下一个 tick"查找：链上用位图在一个 word（256 个 tickSpacing）内查找，
//     找不到就停在 word 边界再继续。每一步的输入向上取整、输出向下取整，
//     所以步子怎么切会影响最终数量，链下必须在同样的位置切步
//   - 穿过已初始化的 tick 时按 liquidityNet 调整活跃流动性
//
// 数据来源是索引器由 Initialize / Mint / Burn / Swap 事件重建的池子状态。

var (
	// ErrPriceLimit 价格限制不在当前价格的正确一侧（链上 revert "SPL"）
	ErrPriceLimit = errors.New("invalid sqrt price limit")
	// ErrInsufficientLiquidity 流动性不足，指定数量无法全部成交（Manager / Quoter 会 revert "PF"）
	ErrInsufficientLiquidity = errors.New("insufficient liquidity")
	// ErrNotInitialized 池子还没有初始化价格
	ErrNotInitialized = errors.New("pool not initialized")
)

// SwapResult 一次 swap 模拟的结果
type SwapResult struct {
	// Amount0 / Amount1 与链上 swap 的返回值同号：正数 = 池子收入，负数 = 池子付出
	Amount0 *big.Int
	Amount1 *big.Int
	// swap 之后的池子状态
	SqrtPriceX96 *big.Int
	Tick         int32
	Liquidity    *big.Int
	// TicksCrossed 穿越的已初始化 tick 数
	TicksCrossed int
	// AmountRemaining 没有成交的指定数量；不为 0 说明撞到了价格限制
	AmountRemaining *big.Int
}

// SimulateSwap 链下复现 UniGoPool.swap 的计算
//
// 参数含义与链上一致：
//   - zeroForOne：true 为 token0 换 token1（价格下降）
//   - amountSpecified：正数为精确输入，负数为精确输出
//   - sqrtPriceLimitX96：价格走到这里就停
func SimulateSwap(pool indexer.PoolSnapshot, zeroForOne bool, amountSpecified, sqrtPriceLimitX96 *big.Int) (*SwapResult, error) {
	if pool.SqrtPriceX96.Sign() == 0 {
		return nil, ErrNotInitialized
	}
	if amountSpecified.Sign() == 0 {
		return nil, errors.New("amount specified is zero")
	}
	if zeroForOne {
		if sqrtPriceLimitX96.Cmp(pool.SqrtPriceX96) >= 0 || sqrtPriceLimitX96.Cmp(umath.MinSqrtRatio) <= 0 {
			return nil, ErrPriceLimit
		}
	} else {
		if sqrtPriceLimitX96.Cmp(pool.SqrtPriceX96) <= 0 || sqrtPriceLimitX96.Cmp(umath.MaxSqrtRatio) >= 0 {
			return nil, ErrPriceLimit
		}
	}

	exactInput := amountSpecified.Sign() > 0
	remaining := new(big.Int).Set(amountSpecified)
	calculated := new(big.Int)
	sqrtPrice := pool.SqrtPriceX96
	tick := pool.Tick
	liquidity := pool.Liquidity
	crossed := 0

	for remaining.Sign() != 0 && sqrtPrice.Cmp(sqrtPriceLimitX96) != 0 {
		sqrtPriceStart := sqrtPrice

		tickNext, liquidityNet := nextInitializedTickWithinOneWord(pool.Ticks, tick, pool.TickSpacing, zeroForOne)
		if tickNext < umath.MinTick {
			tickNext = umath.MinTick
		} else if tickNext > umath.MaxTick {
			tickNext = umath.MaxTick
		}
		sqrtPriceNext, err := umath.GetSqrtRatioAtTick(tickNext)
		if err != nil {
			return nil, err
		}

		// 本步的目标价格：下一个 tick，或者价格限制（哪个先到算哪个）
		target := sqrtPriceNext
		if zeroForOne && sqrtPriceNext.Cmp(sqrtPriceLimitX96) < 0 || !zeroForOne && sqrtPriceNext.Cmp(sqrtPriceLimitX96) > 0 {
			target = sqrtPriceLimitX96
		}

		var amountIn, amountOut, feeAmount *big.Int
		sqrtPrice, amountIn, amountOut, feeAmount, err = umath.ComputeSwapStep(sqrtPrice, target, liquidity, remaining, pool.Fee)
		if err != nil {
			return nil, err
		}

		if exactInput {
			remaining.Sub(remaining, amountIn).Sub(remaining, feeAmount)
			calculated.Sub(calculated, amountOut)
		} else {
			remaining.Add(remaining, amountOut)
			calculated.Add(calculated, amountIn).Add(calculated, feeAmount)
		}

		if sqrtPrice.Cmp(sqrtPriceNext) == 0 {
			// 走到了下一个 tick：已初始化的话穿越它，调整活跃流动性
			if liquidityNet != nil {
				if zeroForOne {
					liquidity = new(big.Int).Sub(liquidity, liquidityNet)
				} else {
					liquidity = new(big.Int).Add(liquidity, liquidityNet)
				}
				if liquidity.Sign() < 0 {
					return nil, umath.ErrSwapMath
				}
				crossed++
			}
			if zeroForOne {
				tick = tickNext - 1
			} else {
				tick = tickNext
			}
		} else if sqrtPrice.Cmp(sqrtPriceStart) != 0 {
			if tick, err = umath.GetTickAtSqrtRatio(sqrtPrice); err != nil {
				return nil, err
			}
		}
	}

	// amountSpecified - remaining 是指定那一侧实际成交的数量
	filled := new(big.Int).Sub(amountSpecified, remaining)
	result := &SwapResult{
		SqrtPriceX96:    sqrtPrice,
		Tick:            tick,
		Liquidity:       liquidity,
		TicksCrossed:    crossed,
		AmountRemaining: remaining,
	}
	if zeroForOne == exactInput {
		result.Amount0, result.Amount1 = filled, calculated
	} else {
		result.Amount0, result.Amount1 = calculated, filled
	}
	return result, nil
}

// nextInitializedTickWithinOneWord 对应 TickBitmap.nextInitializedTickWithinOneWord
//
// 链上把 tick 按 tickSpacing 压缩后存进位图，每个 word 管 256 个压缩 tick，
// 查找只在当前 word 内进行。这里用排序数组得到同样的结果：
// 在同一个 word 的范围内有已初始化的 tick 就返回它和它的 liquidityNet，
// 否则返回 word 边界，liquidityNet 为 nil。
func nextInitializedTickWithinOneWord(ticks []indexer.TickLiquidity, tick, tickSpacing int32, lte bool) (int32, *big.Int) {
	// 向下取整的压缩 tick
	compressed := tick / tickSpacing
	if tick < 0 && tick%tickSpacing != 0 {
		compressed--
	}

	if lte {
		// 向左找：<= 当前 tick，且不低于本 word 的起点
		bitPos := compressed & 255
		wordStart := (compressed - bitPos) * tickSpacing
		i := sort.Search(len(ticks), func(i int) bool { return ticks[i].Tick > compressed*tickSpacing }) - 1
		if i >= 0 && ticks[i].Tick >= wordStart {
			return ticks[i].Tick, ticks[i].LiquidityNet
		}
		return wordStart, nil
	}

	// 向右找：从下一个压缩 tick 开始，且不超过它所在 word 的终点
	start := compressed + 1
	bitPos := start & 255
	wordEnd := (start + 255 - bitPos) * tickSpacing
	i := sort.Search(len(ticks), func(i int) bool { return ticks[i].Tick >= start*tickSpacing })
	if i < len(ticks) && ticks[i].Tick <= wordEnd {
		return ticks[i].Tick, ticks[i].LiquidityNet
	}
	return wordEnd, nil
}

// unlimitedPrice 不限价时使用的价格边界（与 Manager / Quoter 相同）
func unlimitedPrice(zeroForOne bool) *big.Int {
	if zeroForOne {
		return new(big.Int).Add(umath.MinSqrtRatio, big.NewInt(1))
	}
	return new(big.Int).Sub(umath.MaxSqrtRatio, big.NewInt(1))
}

// QuoteExactInputSingle 单池精确输入报价：投入 amountIn 个 tokenIn 能换到多少
//
// 与 Manager.exactInput / Quoter.quoteExactInput 的一跳等价：不限价，输入必须全部成交。
func QuoteExactInputSingle(pool indexer.PoolSnapshot, tokenIn string, amountIn *big.Int) (amountOut *big.Int, result *SwapResult, err error) {
	zeroForOne := tokenIn == pool.Token0
	result, err = SimulateSwap(pool, zeroForOne, amountIn, unlimitedPrice(zeroForOne))
	if err != nil {
		return nil, nil, err
	}
	if result.AmountRemaining.Sign() != 0 {
		return nil, nil, ErrInsufficientLiquidity
	}
	if zeroForOne {
		return new(big.Int).Neg(result.Amount1), result, nil
	}
	return new(big.Int).Neg(result.Amount0), result, nil
}

// QuoteExactOutputSingle 单池精确输出报价：要拿到 amountOut 个 tokenOut 需要投入多少 tokenIn
//
// 与 Manager.exactOutputSingle 等价：不限价，输出必须全部成交。
func QuoteExactOutputSingle(pool indexer.PoolSnapshot, tokenIn string, amountOut *big.Int) (amountIn *big.Int, result *SwapResult, err error) {
	zeroForOne := tokenIn == pool.Token0
	result, err = SimulateSwap(pool, zeroForOne, new(big.Int).Neg(amountOut), unlimitedPrice(zeroForOne))
	if err != nil {
		return nil, nil, err
	}
	if result.AmountRemaining.Sign() != 0 {
		return nil, nil, ErrInsufficientLiquidity
	}
	if zeroForOne {
		return result.Amount0, result, nil
	}
	return result.Amount1, result, nil
}
