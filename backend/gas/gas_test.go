package gas

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"
)

type fakeFees struct {
	baseFee, tip int64
	err          error
	calls        int
}

func (f *fakeFees) Fees(ctx context.Context) (*big.Int, *big.Int, error) {
	f.calls++
	if f.err != nil {
		return nil, nil, f.err
	}
	return big.NewInt(f.baseFee), big.NewInt(f.tip), nil
}

func TestEstimateFromChain(t *testing.T) {
	src := &fakeFees{baseFee: 30e9, tip: 2e9}
	e := NewEstimator(DefaultConfig(), src)

	got := e.Estimate(context.Background(), 150000)
	want := Estimate{
		GasLimit:     150000,
		BaseFee:      "30000000000",
		PriorityFee:  "2000000000",
		GasPrice:     "32000000000",      // 30 + 2 gwei
		MaxFeePerGas: "62000000000",      // 2 × 30 + 2 gwei
		Cost:         "4800000000000000", // 150000 × 32 gwei
		MaxCost:      "9300000000000000", // 150000 × 62 gwei
		Source:       "chain",
	}
	if got != want {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestEstimateCachesChainFees(t *testing.T) {
	src := &fakeFees{baseFee: 30e9, tip: 2e9}
	cfg := DefaultConfig()
	e := NewEstimator(cfg, src)
	for i := 0; i < 5; i++ {
		e.Estimate(context.Background(), 21000)
	}
	if src.calls != 1 {
		t.Fatalf("node queried %d times within TTL, want 1", src.calls)
	}

	// 缓存过期后重新取
	e.fetchedAt = time.Now().Add(-2 * cfg.CacheTTL)
	src.baseFee = 40e9
	if got := e.Estimate(context.Background(), 21000); got.BaseFee != "40000000000" || src.calls != 2 {
		t.Fatalf("after TTL: %+v, calls = %d", got, src.calls)
	}
}

func TestEstimateFallsBackToStatic(t *testing.T) {
	want := Estimate{
		GasLimit:     100000,
		BaseFee:      "20000000000",
		PriorityFee:  "1500000000",
		GasPrice:     "21500000000",
		MaxFeePerGas: "41500000000",
		Cost:         "2150000000000000",
		MaxCost:      "4150000000000000",
		Source:       "static",
	}

	// 没有配置节点
	if got := NewEstimator(DefaultConfig(), nil).Estimate(context.Background(), 100000); got != want {
		t.Fatalf("no node:\ngot  %+v\nwant %+v", got, want)
	}

	// 节点不可用
	src := &fakeFees{err: errors.New("circuit breaker is open")}
	e := NewEstimator(DefaultConfig(), src)
	if got := e.Estimate(context.Background(), 100000); got != want {
		t.Fatalf("node down:\ngot  %+v\nwant %+v", got, want)
	}

	// 节点恢复后切回链上数据（失败结果不缓存）
	src.err, src.baseFee, src.tip = nil, 10e9, 1e9
	if got := e.Estimate(context.Background(), 100000); got.Source != "chain" || got.GasPrice != "11000000000" {
		t.Fatalf("after recovery: %+v", got)
	}
}
