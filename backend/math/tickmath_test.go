package math

import (
	"math/big"
	"testing"
)

// TestGetSqrtRatioAtTick 验证链下 Go 实现与链上 Solidity 的计算一致性
//
// 测试策略：
//
//	使用已知的 tick -> sqrtPriceX96 映射值（从 Solidity 合约中获取）
//	确保 Go 实现的每一个输出都与 Solidity 完全匹配（bit-exact）
func TestGetSqrtRatioAtTick(t *testing.T) {
	tests := []struct {
		name     string
		tick     int32
		expected string // 来自 Solidity 合约的已知值
	}{
		{
			name:     "tick 0 -> price 1:1",
			tick:     0,
			expected: "79228162514264337593543950336", // sqrt(1) * 2^96
		},
		{
			name:     "MinTick",
			tick:     -887272,
			expected: "4295128739",
		},
		{
			name:     "MaxTick",
			tick:     887272,
			expected: "1461446703485210103287273052203988822378723970342",
		},
		{
			name:     "tick 1",
			tick:     1,
			expected: "79232123823359799118286999568",
		},
		{
			name:     "tick -1",
			tick:     -1,
			expected: "79224201403219477170569942574",
		},
		{
			name:     "tick 100",
			tick:     100,
			expected: "79625275426524748796330556128",
		},
		{
			name:     "tick -100",
			tick:     -100,
			expected: "78833030112140176575862854579",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GetSqrtRatioAtTick(tt.tick)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			expected, _ := new(big.Int).SetString(tt.expected, 10)
			if result.Cmp(expected) != 0 {
				t.Errorf("tick %d: got %s, want %s", tt.tick, result.String(), expected.String())
			}
		})
	}
}

// TestGetTickAtSqrtRatio 验证反向转换
func TestGetTickAtSqrtRatio(t *testing.T) {
	tests := []struct {
		name         string
		sqrtPriceX96 string
		expectedTick int32
	}{
		{
			name:         "price 1:1",
			sqrtPriceX96: "79228162514264337593543950336",
			expectedTick: 0,
		},
		{
			name:         "MinSqrtRatio",
			sqrtPriceX96: "4295128739",
			expectedTick: -887272,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sqrtPrice, _ := new(big.Int).SetString(tt.sqrtPriceX96, 10)
			tick, err := GetTickAtSqrtRatio(sqrtPrice)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tick != tt.expectedTick {
				t.Errorf("got tick %d, want %d", tick, tt.expectedTick)
			}
		})
	}
}

// TestTickToPrice 验证价格转换
func TestTickToPrice(t *testing.T) {
	// tick 0 -> price 1.0
	price := TickToPrice(0)
	if price != 1.0 {
		t.Errorf("tick 0: got %f, want 1.0", price)
	}

	// tick 100 -> price ≈ 1.01005
	price = TickToPrice(100)
	if price < 1.009 || price > 1.011 {
		t.Errorf("tick 100: got %f, want ~1.01", price)
	}
}

// TestRoundTrip 验证 tick -> sqrtPrice -> tick 的往返一致性
func TestRoundTrip(t *testing.T) {
	ticks := []int32{0, 1, -1, 100, -100, 1000, -1000, 10000, -10000, 100000, -100000}

	for _, tick := range ticks {
		sqrtPrice, err := GetSqrtRatioAtTick(tick)
		if err != nil {
			t.Fatalf("GetSqrtRatioAtTick(%d) error: %v", tick, err)
		}
		recoveredTick, err := GetTickAtSqrtRatio(sqrtPrice)
		if err != nil {
			t.Fatalf("GetTickAtSqrtRatio error: %v", err)
		}
		if recoveredTick != tick {
			t.Errorf("round trip failed for tick %d: got %d", tick, recoveredTick)
		}
	}
}

// TestAlignTick 验证 tick 对齐
func TestAlignTick(t *testing.T) {
	// tickSpacing = 60
	if got := AlignTick(65, 60, -1); got != 60 {
		t.Errorf("align down 65 to 60-spacing: got %d", got)
	}
	if got := AlignTick(65, 60, 1); got != 120 {
		t.Errorf("align up 65 to 60-spacing: got %d", got)
	}
	if got := AlignTick(-65, 60, -1); got != -120 {
		t.Errorf("align down -65 to 60-spacing: got %d", got)
	}
}

// TestComputeSwapStepPartial 输入不足以到达目标价格时，价格只应移动到输入能推到的位置
func TestComputeSwapStepPartial(t *testing.T) {
	liquidity, _ := new(big.Int).SetString("1000000000000000000000000", 10) // 1e24
	amountIn := big.NewInt(1e18)
	target, _ := GetSqrtRatioAtTick(-10)

	for _, zeroForOne := range []bool{true, false} {
		if !zeroForOne {
			target, _ = GetSqrtRatioAtTick(10)
		}
		next, in, out, fee, err := ComputeSwapStep(Q96, target, liquidity, amountIn, Fee3000)
		if err != nil {
			t.Fatal(err)
		}

		if next.Cmp(target) == 0 || next.Cmp(Q96) == 0 {
			t.Fatalf("zeroForOne=%v: price should stop between current and target, got %s", zeroForOne, next)
		}
		if zeroForOne != (next.Cmp(Q96) < 0) {
			t.Fatalf("zeroForOne=%v: price moved the wrong way", zeroForOne)
		}
		// 输入被完全消耗
		if total := new(big.Int).Add(in, fee); total.Cmp(amountIn) != 0 {
			t.Fatalf("zeroForOne=%v: in+fee = %s, want %s", zeroForOne, total, amountIn)
		}
		// 价格 1:1、流动性远大于输入：产出略低于 输入 × (1 - 0.3%)
		upper := big.NewInt(997e15)
		lower := big.NewInt(996e15)
		if out.Cmp(upper) >= 0 || out.Cmp(lower) <= 0 {
			t.Fatalf("zeroForOne=%v: out = %s, want in (%s, %s)", zeroForOne, out, lower, upper)
		}
	}
}
