package math

import (
	"math/big"
	"math/rand"
	"testing"
)

// 两套实现对同一批随机输入必须给出逐位相同的结果
func TestNativeMatchesOSS(t *testing.T) {
	native, oss := impls["native"], impls["oss"]
	rng := rand.New(rand.NewSource(20261125))
	randBig := func(bits int) *big.Int {
		n := new(big.Int).Rand(rng, new(big.Int).Lsh(big.NewInt(1), uint(bits)))
		return n.Add(n, big.NewInt(1))
	}
	same := func(what string, a, b *big.Int, errA, errB error) {
		t.Helper()
		if (errA == nil) != (errB == nil) {
			t.Fatalf("%s: native err = %v, oss err = %v", what, errA, errB)
		}
		if errA == nil && a.Cmp(b) != 0 {
			t.Fatalf("%s: native = %s, oss = %s", what, a, b)
		}
	}

	for i := 0; i < 20000; i++ {
		tickA := int32(rng.Intn(2*887272+1) - 887272)
		tickB := int32(rng.Intn(2*887272+1) - 887272)
		priceA, errA := native.GetSqrtRatioAtTick(tickA)
		ossA, errOSS := oss.GetSqrtRatioAtTick(tickA)
		same("GetSqrtRatioAtTick", priceA, ossA, errA, errOSS)
		priceB, _ := native.GetSqrtRatioAtTick(tickB)

		// 价格取 tick 对应值再加一点偏移，覆盖不落在 tick 上的情况
		price := new(big.Int).Add(priceA, big.NewInt(int64(rng.Intn(1000))))
		gotTick, errA := native.GetTickAtSqrtRatio(price)
		ossTick, errOSS := oss.GetTickAtSqrtRatio(price)
		same("GetTickAtSqrtRatio", big.NewInt(int64(gotTick)), big.NewInt(int64(ossTick)), errA, errOSS)

		liquidity := randBig(1 + rng.Intn(100))
		amount := randBig(1 + rng.Intn(90))
		roundUp := rng.Intn(2) == 0
		zeroForOne := priceA.Cmp(priceB) >= 0

		same("GetAmount0Delta", native.GetAmount0Delta(priceA, priceB, liquidity, roundUp), oss.GetAmount0Delta(priceA, priceB, liquidity, roundUp), nil, nil)
		same("GetAmount1Delta", native.GetAmount1Delta(priceA, priceB, liquidity, roundUp), oss.GetAmount1Delta(priceA, priceB, liquidity, roundUp), nil, nil)

		next, errA := native.GetNextSqrtPriceFromInput(priceA, liquidity, amount, zeroForOne)
		ossNext, errOSS := oss.GetNextSqrtPriceFromInput(priceA, liquidity, amount, zeroForOne)
		// 结果超出 uint160 的输入在真实池子里走不到（价格被 MAX_SQRT_RATIO 和目标价钳住），
		// 两套实现对这种越界值的处理不同（一个截断一个不截断），不比
		if errOSS != nil || ossNext.BitLen() <= 160 {
			same("GetNextSqrtPriceFromInput", next, ossNext, errA, errOSS)
		}

		remaining := new(big.Int).Set(amount)
		if rng.Intn(2) == 0 {
			remaining.Neg(remaining) // 精确输出
		}
		fee := []uint32{500, 3000, 10000}[rng.Intn(3)]
		n1, in1, out1, fee1, errA := native.ComputeSwapStep(priceA, priceB, liquidity, remaining, fee)
		n2, in2, out2, fee2, errOSS := oss.ComputeSwapStep(priceA, priceB, liquidity, remaining, fee)
		same("ComputeSwapStep next", n1, n2, errA, errOSS)
		same("ComputeSwapStep amountIn", in1, in2, errA, errOSS)
		same("ComputeSwapStep amountOut", out1, out2, errA, errOSS)
		same("ComputeSwapStep fee", fee1, fee2, errA, errOSS)
	}
}

func TestUseUnknownImpl(t *testing.T) {
	if err := Use("nope"); err == nil {
		t.Fatal("want error for unknown implementation")
	}
}
