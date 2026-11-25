package mev

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	"unigo-dex/indexer"
	"unigo-dex/metrics"
)

const (
	poolA    = "0xpoola"
	poolB    = "0xpoolb"
	attacker = "0xattacker"
	victim   = "0xvictim"
	router   = "0xrouter"
)

var blockTime = time.Unix(1_700_000_000, 0)

// swap 构造一笔 Swap：buy=true 表示 token0 进池（zeroForOne）
func swap(pool string, block uint64, logIndex uint, who string, buy bool) *indexer.SwapEvent {
	amount0, amount1 := big.NewInt(100), big.NewInt(-100)
	if !buy {
		amount0, amount1 = big.NewInt(-100), big.NewInt(100)
	}
	return &indexer.SwapEvent{
		PoolAddress: pool,
		Sender:      who,
		Recipient:   who,
		Amount0:     amount0,
		Amount1:     amount1,
		BlockNumber: block,
		Timestamp:   uint64(blockTime.Unix()),
		LogIndex:    logIndex,
		TxHash:      fmt.Sprintf("0xtx-%d-%d", block, logIndex),
	}
}

func feed(g *Guard, swaps ...*indexer.SwapEvent) {
	for _, s := range swaps {
		g.OnSwap(s)
	}
}

func TestSandwichDetected(t *testing.T) {
	g := NewGuard(DefaultConfig())
	before := metrics.MEVSandwichDetected.Value()

	feed(g,
		swap(poolA, 10, 0, attacker, true),  // 抢跑：买入
		swap(poolA, 10, 1, victim, true),    // 受害者：同向买入
		swap(poolA, 10, 2, attacker, false), // 平仓：反向卖出
	)

	alerts := g.Alerts(0)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	a := alerts[0]
	if a.Pool != poolA || a.BlockNumber != 10 || a.Attacker != attacker ||
		a.FrontTx != "0xtx-10-0" || a.BackTx != "0xtx-10-2" ||
		len(a.VictimTxs) != 1 || a.VictimTxs[0] != "0xtx-10-1" {
		t.Fatalf("alert = %+v", a)
	}
	if got := metrics.MEVSandwichDetected.Value() - before; got != 1 {
		t.Fatalf("metric delta = %d, want 1", got)
	}

	// 同一组交易不重复告警：攻击者之后再卖一笔不算新的三明治
	g.OnSwap(swap(poolA, 10, 3, attacker, false))
	if n := len(g.Alerts(0)); n != 1 {
		t.Fatalf("got %d alerts after extra sell, want 1", n)
	}
}

func TestSandwichThroughRouter(t *testing.T) {
	// 都经路由合约下单：sender 相同，靠 recipient 区分攻击者和受害者
	viaRouter := func(logIndex uint, recipient string, buy bool) *indexer.SwapEvent {
		s := swap(poolA, 10, logIndex, router, buy)
		s.Recipient = recipient
		return s
	}
	g := NewGuard(DefaultConfig())
	feed(g, viaRouter(0, attacker, false), viaRouter(1, victim, false), viaRouter(2, attacker, true))
	if alerts := g.Alerts(0); len(alerts) != 1 || alerts[0].Attacker != attacker || alerts[0].AttackerSender != router {
		t.Fatalf("alerts = %+v", alerts)
	}
}

func TestSandwichNotDetected(t *testing.T) {
	cases := map[string][]*indexer.SwapEvent{
		"只有同向两笔": {
			swap(poolA, 10, 0, attacker, true),
			swap(poolA, 10, 1, victim, true),
		},
		"前后同向没有反向平仓": {
			swap(poolA, 10, 0, attacker, true),
			swap(poolA, 10, 1, victim, true),
			swap(poolA, 10, 2, attacker, true),
		},
		"不同池子": {
			swap(poolA, 10, 0, attacker, true),
			swap(poolB, 10, 1, victim, true),
			swap(poolA, 10, 2, attacker, false),
		},
		"平仓在另一个池子": {
			swap(poolA, 10, 0, attacker, true),
			swap(poolA, 10, 1, victim, true),
			swap(poolB, 10, 2, attacker, false),
		},
		"不同区块": {
			swap(poolA, 10, 0, attacker, true),
			swap(poolA, 10, 1, victim, true),
			swap(poolA, 11, 0, attacker, false),
		},
		"中间的交易与抢跑方向相反": {
			swap(poolA, 10, 0, attacker, true),
			swap(poolA, 10, 1, victim, false),
			swap(poolA, 10, 2, attacker, false),
		},
		"中间没有别人的交易": {
			swap(poolA, 10, 0, attacker, true),
			swap(poolA, 10, 1, attacker, false),
		},
	}
	for name, swaps := range cases {
		g := NewGuard(DefaultConfig())
		feed(g, swaps...)
		if alerts := g.Alerts(0); len(alerts) != 0 {
			t.Errorf("%s: unexpected alerts %+v", name, alerts)
		}
	}
}

func TestSandwichOrderedByLogIndex(t *testing.T) {
	// 事件乱序到达时仍按 logIndex 判定：实际顺序是 受害者(0) → 攻击者买(1) → 攻击者卖(2)，不构成三明治
	g := NewGuard(DefaultConfig())
	feed(g,
		swap(poolA, 10, 1, attacker, true),
		swap(poolA, 10, 0, victim, true),
		swap(poolA, 10, 2, attacker, false),
	)
	if alerts := g.Alerts(0); len(alerts) != 0 {
		t.Fatalf("unexpected alerts %+v", alerts)
	}
}

func TestReorgDropsAlerts(t *testing.T) {
	g := NewGuard(DefaultConfig())
	sandwich := func(block uint64) {
		feed(g, swap(poolA, block, 0, attacker, true), swap(poolA, block, 1, victim, true), swap(poolA, block, 2, attacker, false))
	}
	sandwich(10)
	sandwich(12)
	g.OnSwap(swap(poolA, 13, 0, attacker, true)) // 区块 13 的抢跑，随后被回滚

	g.OnReorg(12)
	if alerts := g.Alerts(0); len(alerts) != 1 || alerts[0].BlockNumber != 10 {
		t.Fatalf("alerts after reorg = %+v", alerts)
	}
	// 被回滚的抢跑交易不能和新链上的交易配对
	feed(g, swap(poolA, 13, 1, victim, true), swap(poolA, 13, 2, attacker, false))
	if n := len(g.Alerts(0)); n != 1 {
		t.Fatalf("got %d alerts, want 1", n)
	}
}

func TestAlertsBounded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxAlerts = 3
	g := NewGuard(cfg)
	for block := uint64(1); block <= 10; block++ {
		feed(g, swap(poolA, block, 0, attacker, true), swap(poolA, block, 1, victim, true), swap(poolA, block, 2, attacker, false))
	}
	alerts := g.Alerts(0)
	if len(alerts) != 3 || alerts[0].BlockNumber != 10 || alerts[2].BlockNumber != 8 {
		t.Fatalf("alerts = %+v", alerts)
	}
	if got := g.Alerts(1); len(got) != 1 || got[0].BlockNumber != 10 {
		t.Fatalf("limit=1: %+v", got)
	}
}

func TestRecommendSlippage(t *testing.T) {
	g := NewGuard(DefaultConfig())
	now := blockTime.Add(time.Minute)

	if s, w := g.RecommendSlippage([]string{poolA}, 0, now); s != 0.005 || w != "" {
		t.Fatalf("default: %v %q", s, w)
	}
	if s, _ := g.RecommendSlippage([]string{poolA}, 0.9, now); s != 0.5 {
		t.Fatalf("capped: %v", s)
	}

	feed(g, swap(poolA, 10, 0, attacker, true), swap(poolA, 10, 1, victim, true), swap(poolA, 10, 2, attacker, false))

	if s, w := g.RecommendSlippage([]string{poolB, poolA}, 0.01, now); s != 0.001 || w == "" {
		t.Fatalf("attacked pool: %v %q", s, w)
	}
	// 用户本来就更严格时不放宽
	if s, _ := g.RecommendSlippage([]string{poolA}, 0.0005, now); s != 0.0005 {
		t.Fatalf("tighter user slippage: %v", s)
	}
	// 其它池子不受影响
	if s, w := g.RecommendSlippage([]string{poolB}, 0.01, now); s != 0.01 || w != "" {
		t.Fatalf("other pool: %v %q", s, w)
	}
	// 超过告警窗口后恢复
	if s, w := g.RecommendSlippage([]string{poolA}, 0.01, now.Add(time.Hour)); s != 0.01 || w != "" {
		t.Fatalf("after window: %v %q", s, w)
	}
}

func TestPriceImpact(t *testing.T) {
	q96 := func(mul int64) *big.Int { return new(big.Int).Lsh(big.NewInt(mul), 96) }
	// 价格 4：1 个 token0 换 4 个 token1
	pool := indexer.PoolSnapshot{Token0: "0xa", Token1: "0xb", Fee: 3000, SqrtPriceX96: q96(2)}
	in := big.NewInt(1_000_000)

	approx := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

	// 按现价成交：4 × 0.997 × 1e6，没有冲击
	if got := PriceImpact([]indexer.PoolSnapshot{pool}, "0xa", in, big.NewInt(3_988_000)); !approx(got, 0) {
		t.Errorf("no impact: %v", got)
	}
	// 实际只拿到 90%
	if got := PriceImpact([]indexer.PoolSnapshot{pool}, "0xa", in, big.NewInt(3_589_200)); !approx(got, 0.1) {
		t.Errorf("10%% impact: %v", got)
	}
	// 反方向：token1 换 token0，现价产出 1e6 / 4 × 0.997
	if got := PriceImpact([]indexer.PoolSnapshot{pool}, "0xb", in, big.NewInt(249_250)); !approx(got, 0) {
		t.Errorf("reverse: %v", got)
	}
	// 两跳：a -> b -> c
	pool2 := indexer.PoolSnapshot{Token0: "0xb", Token1: "0xc", Fee: 3000, SqrtPriceX96: q96(1)}
	spot := 1e6 * 4 * 0.997 * 0.997
	if got := PriceImpact([]indexer.PoolSnapshot{pool, pool2}, "0xa", in, big.NewInt(int64(spot*0.95))); math.Abs(got-0.05) > 1e-6 {
		t.Errorf("two hops: %v", got)
	}
	// 没有价格的池子按 100% 冲击处理
	noPrice := indexer.PoolSnapshot{Token0: "0xa", Token1: "0xb", SqrtPriceX96: big.NewInt(0)}
	if got := PriceImpact([]indexer.PoolSnapshot{noPrice}, "0xa", in, in); got != 1 {
		t.Errorf("no price: %v", got)
	}
}

func TestMinAmountOut(t *testing.T) {
	if got := MinAmountOut(big.NewInt(1_000_000), 0.005); got.Int64() != 995_000 {
		t.Errorf("0.5%%: %s", got)
	}
	if got := MinAmountOut(big.NewInt(999), 0.001); got.Int64() != 998 { // 向下取整
		t.Errorf("round down: %s", got)
	}
	big1, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	want, _ := new(big.Int).SetString("122222221122222222112222222211", 10)
	if got := MinAmountOut(big1, 0.01); got.Cmp(want) != 0 {
		t.Errorf("big: %s", got)
	}
}

func TestCheckSwap(t *testing.T) {
	g := NewGuard(DefaultConfig())
	now := time.Unix(1_700_000_000, 0)
	ok := now.Add(10 * time.Minute).Unix()
	before := metrics.MEVSwapsRejected.Value()

	if err := g.CheckSwap(0.01, ok, now); err != nil {
		t.Fatalf("valid swap rejected: %v", err)
	}
	if err := g.CheckSwap(0.06, ok, now); !errors.Is(err, ErrPriceImpactTooHigh) {
		t.Fatalf("high impact: %v", err)
	}
	if err := g.CheckSwap(0.01, now.Unix(), now); !errors.Is(err, ErrDeadlineExpired) {
		t.Fatalf("expired: %v", err)
	}
	if err := g.CheckSwap(0.01, 0, now); !errors.Is(err, ErrDeadlineExpired) {
		t.Fatalf("missing deadline: %v", err)
	}
	if err := g.CheckSwap(0.01, now.Add(2*time.Hour).Unix(), now); !errors.Is(err, ErrDeadlineTooFar) {
		t.Fatalf("too far: %v", err)
	}
	if got := metrics.MEVSwapsRejected.Value() - before; got != 4 {
		t.Fatalf("rejected metric delta = %d, want 4", got)
	}
}
