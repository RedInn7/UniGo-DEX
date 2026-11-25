package math

import (
	"fmt"
	"math/big"
	"os"

	"github.com/daoleno/uniswapv3-sdk/constants"
	"github.com/daoleno/uniswapv3-sdk/utils"
)

// impl.go — 数学库的可切换实现
//
// 核心运算有两套实现，对外是同一组函数：
//   - native：本包自己照着链上 Solidity 库写的（tickmath.go / sqrtpricemath.go）
//   - oss：社区的 Go 版 Uniswap V3 SDK（github.com/daoleno/uniswapv3-sdk）
//
// 用环境变量 MATH_IMPL=native|oss 选择，默认 native。两套实现跑的是同一套单元测试
// 和同一套与链上 Quoter 逐位比对的集成测试，所以切换不改变任何报价结果。

// Impl 一套核心运算
type Impl struct {
	Name                       string
	GetSqrtRatioAtTick         func(tick int32) (*big.Int, error)
	GetTickAtSqrtRatio         func(sqrtPriceX96 *big.Int) (int32, error)
	GetAmount0Delta            func(sqrtPriceA, sqrtPriceB, liquidity *big.Int, roundUp bool) *big.Int
	GetAmount1Delta            func(sqrtPriceA, sqrtPriceB, liquidity *big.Int, roundUp bool) *big.Int
	GetNextSqrtPriceFromInput  func(sqrtPrice, liquidity, amountIn *big.Int, zeroForOne bool) (*big.Int, error)
	GetNextSqrtPriceFromOutput func(sqrtPrice, liquidity, amountOut *big.Int, zeroForOne bool) (*big.Int, error)
	ComputeSwapStep            func(sqrtPriceCurrent, sqrtPriceTarget, liquidity, amountRemaining *big.Int, feePips uint32) (sqrtPriceNext, amountIn, amountOut, feeAmount *big.Int, err error)
}

var impls = map[string]*Impl{
	"native": {
		Name:                       "native",
		GetSqrtRatioAtTick:         nativeGetSqrtRatioAtTick,
		GetTickAtSqrtRatio:         nativeGetTickAtSqrtRatio,
		GetAmount0Delta:            nativeGetAmount0Delta,
		GetAmount1Delta:            nativeGetAmount1Delta,
		GetNextSqrtPriceFromInput:  nativeGetNextSqrtPriceFromInput,
		GetNextSqrtPriceFromOutput: nativeGetNextSqrtPriceFromOutput,
		ComputeSwapStep:            nativeComputeSwapStep,
	},
	"oss": {
		Name: "oss",
		GetSqrtRatioAtTick: func(tick int32) (*big.Int, error) {
			return utils.GetSqrtRatioAtTick(int(tick))
		},
		GetTickAtSqrtRatio: func(sqrtPriceX96 *big.Int) (int32, error) {
			tick, err := utils.GetTickAtSqrtRatio(sqrtPriceX96)
			return int32(tick), err
		},
		GetAmount0Delta:            utils.GetAmount0Delta,
		GetAmount1Delta:            utils.GetAmount1Delta,
		GetNextSqrtPriceFromInput:  utils.GetNextSqrtPriceFromInput,
		GetNextSqrtPriceFromOutput: utils.GetNextSqrtPriceFromOutput,
		ComputeSwapStep: func(sqrtPriceCurrent, sqrtPriceTarget, liquidity, amountRemaining *big.Int, feePips uint32) (*big.Int, *big.Int, *big.Int, *big.Int, error) {
			return utils.ComputeSwapStep(sqrtPriceCurrent, sqrtPriceTarget, liquidity, amountRemaining, constants.FeeAmount(feePips))
		},
	},
}

// active 当前生效的实现；只在启动时（或测试里）切换，运行期间只读
var active = impls["native"]

func init() {
	if name := os.Getenv("MATH_IMPL"); name != "" {
		if err := Use(name); err != nil {
			panic(err)
		}
	}
}

// Use 切换数学库实现，name 取 native 或 oss
func Use(name string) error {
	impl, ok := impls[name]
	if !ok {
		return fmt.Errorf("未知的数学库实现 %q，可选 native / oss", name)
	}
	active = impl
	return nil
}

// Active 当前生效的实现名
func Active() string { return active.Name }

// GetSqrtRatioAtTick 由 tick 计算 sqrtPriceX96
func GetSqrtRatioAtTick(tick int32) (*big.Int, error) { return active.GetSqrtRatioAtTick(tick) }

// GetTickAtSqrtRatio 由 sqrtPriceX96 反推 tick
func GetTickAtSqrtRatio(sqrtPriceX96 *big.Int) (int32, error) {
	return active.GetTickAtSqrtRatio(sqrtPriceX96)
}

// GetAmount0Delta 两个价格之间的 token0 数量
func GetAmount0Delta(sqrtPriceA, sqrtPriceB, liquidity *big.Int, roundUp bool) *big.Int {
	return active.GetAmount0Delta(sqrtPriceA, sqrtPriceB, liquidity, roundUp)
}

// GetAmount1Delta 两个价格之间的 token1 数量
func GetAmount1Delta(sqrtPriceA, sqrtPriceB, liquidity *big.Int, roundUp bool) *big.Int {
	return active.GetAmount1Delta(sqrtPriceA, sqrtPriceB, liquidity, roundUp)
}

// GetNextSqrtPriceFromInput 投入 amountIn 之后的价格
func GetNextSqrtPriceFromInput(sqrtPrice, liquidity, amountIn *big.Int, zeroForOne bool) (*big.Int, error) {
	return active.GetNextSqrtPriceFromInput(sqrtPrice, liquidity, amountIn, zeroForOne)
}

// GetNextSqrtPriceFromOutput 取出 amountOut 之后的价格
func GetNextSqrtPriceFromOutput(sqrtPrice, liquidity, amountOut *big.Int, zeroForOne bool) (*big.Int, error) {
	return active.GetNextSqrtPriceFromOutput(sqrtPrice, liquidity, amountOut, zeroForOne)
}

// ComputeSwapStep 单步 swap 计算
func ComputeSwapStep(sqrtPriceCurrent, sqrtPriceTarget, liquidity, amountRemaining *big.Int, feePips uint32) (sqrtPriceNext, amountIn, amountOut, feeAmount *big.Int, err error) {
	return active.ComputeSwapStep(sqrtPriceCurrent, sqrtPriceTarget, liquidity, amountRemaining, feePips)
}
