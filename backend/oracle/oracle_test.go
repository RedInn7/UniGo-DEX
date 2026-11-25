package oracle

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"unigo-dex/indexer"
)

const pair = "0xa/0xb"

func newOracle() *Oracle {
	cfg := DefaultOracleConfig()
	cfg.ProtectionCooldown = 0
	return NewOracle(cfg)
}

func TestCheckTWAP(t *testing.T) {
	o := newOracle()

	// 现价与 TWAP 接近：正常
	if c := o.CheckTWAP(pair, 1.01, 1.0); c.Status != TWAPOK || c.Deviation < 0.0099 || c.Deviation > 0.0101 {
		t.Fatalf("small deviation: %+v", c)
	}
	if o.GetProtectionStatus(pair).Active {
		t.Fatal("protection should be off")
	}

	// 现价被拉高 8%：疑似操纵，进入保护模式
	c := o.CheckTWAP(pair, 1.08, 1.0)
	if c.Status != TWAPDeviated {
		t.Fatalf("large deviation: %+v", c)
	}
	status := o.GetProtectionStatus(pair)
	if !status.Active || !strings.Contains(status.Reason, "TWAP") {
		t.Fatalf("protection status: %+v", status)
	}

	// 价格回归后退出保护模式
	if c := o.CheckTWAP(pair, 1.0, 1.0); c.Status != TWAPOK {
		t.Fatalf("recovered: %+v", c)
	}
	if o.GetProtectionStatus(pair).Active {
		t.Fatal("protection should be lifted after prices converge")
	}

	// 取不到 TWAP：无法判断，不改变任何状态
	if c := o.CheckTWAP("0xc/0xd", 1.5, 0); c.Status != TWAPUnavailable {
		t.Fatalf("unavailable: %+v", c)
	}
	if len(o.GetPriceInfo("0xc/0xd")) != 0 {
		t.Fatal("unavailable TWAP should not record prices")
	}
}

func TestThreeWayCrossValidation(t *testing.T) {
	// 现价和 TWAP 一致，但两者都偏离 Chainlink：不是瞬时操纵，而是池子整体偏了
	o := newOracle()
	o.UpdateChainlinkPrice(pair, 1.0, time.Now())
	if c := o.CheckTWAP(pair, 1.1, 1.1); c.Status != TWAPOK {
		t.Fatalf("spot == twap: %+v", c)
	}
	status := o.GetProtectionStatus(pair)
	if !status.Active || !strings.Contains(status.Reason, "Chainlink") {
		t.Fatalf("protection status: %+v", status)
	}

	// 现价回到 Chainlink 附近，但 TWAP 还没回来：仍然保护
	o.CheckTWAP(pair, 1.0, 1.04)
	if status := o.GetProtectionStatus(pair); !status.Active || !strings.Contains(status.Reason, "TWAP 与 Chainlink") {
		t.Fatalf("twap vs chainlink: %+v", status)
	}

	// 三方一致：解除
	o.CheckTWAP(pair, 1.0, 1.005)
	if o.GetProtectionStatus(pair).Active {
		t.Fatal("protection should be lifted when all three sources agree")
	}

	// Chainlink 过期时不拿它做比较，只看现价 vs TWAP
	o2 := newOracle()
	o2.UpdateChainlinkPrice(pair, 5.0, time.Now().Add(-2*time.Hour))
	o2.CheckTWAP(pair, 1.0, 1.0)
	if status := o2.GetProtectionStatus(pair); !status.Active || !strings.Contains(status.Reason, "过期") {
		t.Fatalf("stale chainlink should keep its own protection reason: %+v", status)
	}
}

func TestPoolPriceFeed(t *testing.T) {
	o := newOracle()
	state := indexer.NewPoolStateManager()
	state.AddPool(indexer.NewPoolState("0xpool", "0xa", "0xb", 3000, 60))
	o.UpdateChainlinkPrice(pair, 1.0, time.Now())

	feed := o.PoolPriceFeed(state)
	// 成交后价格 4（sqrtPriceX96 = 2 × 2^96），与 Chainlink 的 1.0 严重偏离
	feed.OnSwap(&indexer.SwapEvent{PoolAddress: "0xpool", SqrtPriceX96: new(big.Int).Lsh(big.NewInt(2), 96)})

	info := o.GetPriceInfo(pair)
	if info["pool"] == nil || info["pool"].Price != 4 {
		t.Fatalf("pool price not recorded: %+v", info["pool"])
	}
	if !o.GetProtectionStatus(pair).Active {
		t.Fatal("deviation from Chainlink should activate protection")
	}

	// 不认识的池子忽略
	feed.OnSwap(&indexer.SwapEvent{PoolAddress: "0xother", SqrtPriceX96: big.NewInt(1)})
}
