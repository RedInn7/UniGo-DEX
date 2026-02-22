package math

import (
	"errors"
	"math"
	"math/big"
)

// TickMath 实现 tick 与 sqrtPriceX96 之间的转换
//
// 核心公式：
//   price = 1.0001^tick
//   sqrtPriceX96 = sqrt(1.0001^tick) × 2^96
//
// Go 实现与 Solidity TickMath.sol 完全对应，使用相同的查表法 + 位运算，
// 保证链上链下计算结果完全一致（bit-exact）。
//
// 为什么需要链下复现？
//   1. 链下路由引擎需要模拟 swap，必须精确复现价格计算
//   2. 报价服务不应频繁调用链上 Quoter（gas 和延迟开销）
//   3. 索引器需要根据事件数据重建池子状态

// magicNumbers 是 sqrt(1.0001^(2^i)) 的 Q128.128 预计算值
// 每个值对应 absTick 的一个二进制位
// 这些常量与 Solidity 合约中 getSqrtRatioAtTick 使用的完全相同
var magicNumbers [20]*big.Int

func init() {
	hexValues := []string{
		"fffcb933bd6fad37aa2d162d1a594001",
		"fff97272373d413259a46990580e213a",
		"fff2e50f5f656932ef12357cf3c7fdcc",
		"ffe5caca7e10e4e61c3624eaa0941cd0",
		"ffcb9843d60f6159c9db58835c926644",
		"ff973b41fa98c081472e6896dfb254c0",
		"ff2ea16466c96a3843ec78b326b52861",
		"fe5dee046a99a2a811c461f1969c3053",
		"fcbe86c7900a88aedcffc83b479aa3a4",
		"f987a7253ac413176f2b074cf7815e54",
		"f3392b0822b70005940c7a398e4b70f3",
		"e7159475a2c29b7443b29c7fa6e889d9",
		"d097f3bdfd2022b8845ad8f792aa5825",
		"a9f746462d870fdf8a65dc1f90e061e5",
		"70d869a156d2a1b890bb3df62baf32f7",
		"31be135f97d08fd981231505542fcfa6",
		"9aa508b5b7a84e1c677de54f3e99bc9",
		"5d6af8dedb81196699c329225ee604",
		"2216e584f5fa1ea926041bedfe98",
		"48a170391f7dc42444e8fa2",
	}
	for i, hex := range hexValues {
		magicNumbers[i], _ = new(big.Int).SetString(hex, 16)
	}
}

// baseRatio 是 absTick 第 0 位为 0 时的基础值（Q128.128 格式的 1.0）
var baseRatio, _ = new(big.Int).SetString("100000000000000000000000000000000", 16)

// GetSqrtRatioAtTick 根据 tick 计算 sqrtPriceX96
//
// 算法原理（查表法）：
//
//	将 |tick| 分解为二进制：|tick| = b0 + 2*b1 + 4*b2 + ... + 2^19*b19
//	则 sqrt(1.0001^|tick|) = ∏(sqrt(1.0001^(2^i))) 对所有 bi=1 的 i
//	每个 sqrt(1.0001^(2^i)) 都是预计算的常量（magicNumbers）
//
//	如果 tick > 0，取倒数（因为预计算值是 1/sqrt(1.0001^(2^i))）
//	最后从 Q128.128 格式转换为 Q64.96
func GetSqrtRatioAtTick(tick int32) (*big.Int, error) {
	absTick := int64(tick)
	if absTick < 0 {
		absTick = -absTick
	}
	if absTick > int64(MaxTick) {
		return nil, errors.New("tick out of range")
	}

	absTickBig := big.NewInt(absTick)

	// 初始化 ratio（Q128.128 格式）
	ratio := new(big.Int)
	if absTickBig.Bit(0) == 1 {
		ratio.Set(magicNumbers[0])
	} else {
		ratio.Set(baseRatio)
	}

	// 依次检查每一位，如果为 1 则乘以对应的 magic number
	for i := 1; i < 20; i++ {
		if absTickBig.Bit(i) == 1 {
			ratio.Mul(ratio, magicNumbers[i])
			ratio.Rsh(ratio, 128)
		}
	}

	// 如果 tick 为正，取倒数：ratio = MaxUint256 / ratio
	if tick > 0 {
		ratio.Div(MaxUint256, ratio)
	}

	// 从 Q128.128 转换为 Q64.96：右移 32 位，并向上取整
	mask32 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 32), big.NewInt(1))
	mod := new(big.Int).And(ratio, mask32)
	result := new(big.Int).Rsh(ratio, 32)
	if mod.Sign() > 0 {
		result.Add(result, big.NewInt(1))
	}

	return result, nil
}

// GetTickAtSqrtRatio 根据 sqrtPriceX96 计算 tick
//
// 算法原理（基于 log2）：
//
//	tick = floor(log_{1.0001}(sqrtPrice^2))
//	使用浮点近似 + 边界验证确保精确
func GetTickAtSqrtRatio(sqrtPriceX96 *big.Int) (int32, error) {
	if sqrtPriceX96.Cmp(MinSqrtRatio) < 0 || sqrtPriceX96.Cmp(MaxSqrtRatio) >= 0 {
		return 0, errors.New("sqrtPriceX96 out of range")
	}

	// 使用浮点近似计算 tick
	sqrtPriceFloat := new(big.Float).SetInt(sqrtPriceX96)
	q96Float := new(big.Float).SetInt(Q96)
	sqrtPrice, _ := new(big.Float).Quo(sqrtPriceFloat, q96Float).Float64()
	price := sqrtPrice * sqrtPrice
	tickApprox := math.Log(price) / math.Log(1.0001)
	tick := int32(math.Floor(tickApprox))

	// 验证并修正：检查 tick 和 tick+1 哪个正确
	sqrtAtTick, err := GetSqrtRatioAtTick(tick)
	if err != nil {
		return 0, err
	}
	sqrtAtTickPlus1, err := GetSqrtRatioAtTick(tick + 1)
	if err != nil {
		return 0, err
	}

	// tick 正确的条件：sqrtAtTick <= sqrtPriceX96 < sqrtAtTick+1
	if sqrtAtTick.Cmp(sqrtPriceX96) <= 0 && sqrtAtTickPlus1.Cmp(sqrtPriceX96) > 0 {
		return tick, nil
	}
	// 如果不满足，尝试 tick-1 和 tick+1
	if sqrtAtTick.Cmp(sqrtPriceX96) > 0 {
		return tick - 1, nil
	}
	return tick + 1, nil
}

// TickToPrice 将 tick 转换为人类可读的价格（浮点数）
//
// 公式：price = 1.0001^tick
// 用途：用于 API 响应和日志中展示可读价格
func TickToPrice(tick int32) float64 {
	return math.Pow(1.0001, float64(tick))
}

// PriceToTick 将价格转换为最近的 tick
//
// 公式：tick = floor(log(price) / log(1.0001))
func PriceToTick(price float64) int32 {
	return int32(math.Floor(math.Log(price) / math.Log(1.0001)))
}

// AlignTick 将 tick 对齐到 tickSpacing 的整数倍
// direction: -1 向下对齐, 0 最近对齐, 1 向上对齐
func AlignTick(tick int32, tickSpacing int32, direction int) int32 {
	if tickSpacing == 0 {
		return tick
	}
	aligned := tick / tickSpacing * tickSpacing
	if tick < 0 && tick%tickSpacing != 0 {
		aligned -= tickSpacing
	}
	switch direction {
	case 1: // 向上
		if aligned < tick {
			aligned += tickSpacing
		}
	case -1: // 向下（已完成）
	default: // 最近
		if tick-aligned > aligned+tickSpacing-tick {
			aligned += tickSpacing
		}
	}
	return aligned
}
