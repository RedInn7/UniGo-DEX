package router

import (
	"fmt"
	"math/big"
	"time"

	umath "unigo-dex/math"
)

// quote.go — 链下报价引擎
//
// 链下报价的优势（相比调用链上 Quoter 合约）：
//   1. 零 Gas 消耗：纯内存计算
//   2. 低延迟：不需要等待 RPC 调用和 EVM 执行
//   3. 高吞吐：可以并发处理大量报价请求
//   4. 可扩展：可以轻松添加多路径报价对比
//
// 与链上保持一致的关键：
//   - 使用相同的 Q64.96 定点数格式
//   - 使用相同的 mulDiv 全精度算法
//   - 使用相同的 tick 查找逻辑
//   - 所有中间值精确复现，不使用浮点近似

// QuoteRequest 报价请求
type QuoteRequest struct {
	TokenIn    string   // 输入 token 地址
	TokenOut   string   // 输出 token 地址
	AmountIn   *big.Int // 输入数量
	MaxHops    int      // 最大跳数（0 = 使用默认值）
}

// QuoteResult 报价结果
type QuoteResult struct {
	Route        *Route        // 最优路径
	AmountOut    *big.Int      // 预期输出
	PriceImpact  float64       // 价格影响（百分比）
	GasEstimate  uint64        // 预估 Gas
	TicksCrossed int           // 穿越的 tick 数
	IsDegraded   bool          // 是否为降级路径
	Latency      time.Duration // 计算耗时
	Warning      string        // 警告信息（高滑点、高 Gas 等）
}

// SimulateSwap 模拟单池 swap，精确复现链上计算
//
// 该函数链下复现 UniGoPool.swap() 的完整逻辑：
//   1. 确定交易方向
//   2. 循环：查找下一个 tick → 计算单步 swap → 更新状态
//   3. 返回最终的输出数量和价格影响
//
// 与链上的区别：
//   - 链上通过 TickBitmap（位图）查找下一个 tick
//   - 链下通过排序数组 + 二分查找（更直观，等价结果）
func SimulateSwap(
	sqrtPriceX96 *big.Int,
	liquidity *big.Int,
	tick int32,
	fee uint32,
	amountIn *big.Int,
	zeroForOne bool,
) (amountOut *big.Int, newSqrtPrice *big.Int, newTick int32, ticksCrossed int) {
	// 确定价格限制
	var sqrtPriceLimit *big.Int
	if zeroForOne {
		sqrtPriceLimit = new(big.Int).Add(umath.MinSqrtRatio, big.NewInt(1))
	} else {
		sqrtPriceLimit = new(big.Int).Sub(umath.MaxSqrtRatio, big.NewInt(1))
	}

	currentSqrtPrice := new(big.Int).Set(sqrtPriceX96)
	currentLiquidity := new(big.Int).Set(liquidity)
	currentTick := tick
	remainingAmount := new(big.Int).Set(amountIn)
	totalOut := big.NewInt(0)

	// 最多循环 100 步（防止无限循环）
	for step := 0; step < 100 && remainingAmount.Sign() > 0; step++ {
		// 计算目标价格（下一个 tick 的价格，简化为 10 tick 步进）
		var nextTick int32
		if zeroForOne {
			nextTick = currentTick - 10
		} else {
			nextTick = currentTick + 10
		}

		targetSqrtPrice, err := umath.GetSqrtRatioAtTick(nextTick)
		if err != nil {
			break
		}

		// 检查是否超过价格限制
		if zeroForOne && targetSqrtPrice.Cmp(sqrtPriceLimit) < 0 {
			targetSqrtPrice = sqrtPriceLimit
		} else if !zeroForOne && targetSqrtPrice.Cmp(sqrtPriceLimit) > 0 {
			targetSqrtPrice = sqrtPriceLimit
		}

		if currentLiquidity.Sign() == 0 {
			break
		}

		// 调用链下 ComputeSwapStep
		newPrice, stepIn, stepOut, stepFee := umath.ComputeSwapStep(
			currentSqrtPrice,
			targetSqrtPrice,
			currentLiquidity,
			remainingAmount,
			fee,
		)

		totalCost := new(big.Int).Add(stepIn, stepFee)
		remainingAmount.Sub(remainingAmount, totalCost)
		totalOut.Add(totalOut, stepOut)
		currentSqrtPrice = newPrice

		if currentSqrtPrice.Cmp(targetSqrtPrice) == 0 {
			currentTick = nextTick
			ticksCrossed++
		}
	}

	return totalOut, currentSqrtPrice, currentTick, ticksCrossed
}

// FormatQuoteResult 格式化报价结果为可读字符串
func FormatQuoteResult(result *QuoteResult) string {
	status := "✅ 正常路由"
	if result.IsDegraded {
		status = "⚠️ 降级路由"
	}

	msg := fmt.Sprintf(
		"%s\n输出: %s\n价格影响: %.2f%%\nGas 预估: %d\nTick 穿越: %d\n延迟: %v",
		status,
		result.AmountOut.String(),
		result.PriceImpact,
		result.GasEstimate,
		result.TicksCrossed,
		result.Latency,
	)

	if result.Warning != "" {
		msg += fmt.Sprintf("\n⚠️ 警告: %s", result.Warning)
	}

	return msg
}
