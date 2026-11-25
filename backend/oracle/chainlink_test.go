package oracle

import (
	"context"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"unigo-dex/chain"
	umath "unigo-dex/math"
)

const (
	weth = "0x00000000000000000000000000000000000000aa" // 18 位
	usdc = "0x00000000000000000000000000000000000000bb" // 6 位
	feed = "0x00000000000000000000000000000000000000fe"
)

// fakeFeed 可编程的聚合器
type fakeFeed struct {
	decimals      uint8
	round         chain.ChainlinkRound
	err           error
	roundCalls    int
	decimalsCalls int
}

func (f *fakeFeed) ChainlinkDecimals(context.Context, string) (uint8, error) {
	f.decimalsCalls++
	return f.decimals, f.err
}

func (f *fakeFeed) ChainlinkLatestRound(context.Context, string) (*chain.ChainlinkRound, error) {
	f.roundCalls++
	if f.err != nil {
		return nil, f.err
	}
	round := f.round
	return &round, nil
}

// goodRound ETH/USD = 2000，8 位小数，10 分钟前更新
func goodRound() chain.ChainlinkRound {
	return chain.ChainlinkRound{
		RoundID: big.NewInt(100), AnsweredInRound: big.NewInt(100),
		Answer:    big.NewInt(2000_00000000),
		StartedAt: 1_700_000_000, UpdatedAt: 1_700_000_000, BlockTime: 1_700_000_600,
	}
}

func TestParseFeeds(t *testing.T) {
	feeds, err := ParseFeeds(" 0x00000000000000000000000000000000000000AA:" + usdc + ":" + feed + ":18:6:7200 , " + usdc + ":" + weth + ":" + feed + ":6:18,")
	if err != nil {
		t.Fatal(err)
	}
	want := []FeedConfig{
		{Base: weth, Quote: usdc, Address: feed, BaseDecimals: 18, QuoteDecimals: 6, Heartbeat: 2 * time.Hour},
		{Base: usdc, Quote: weth, Address: feed, BaseDecimals: 6, QuoteDecimals: 18, Heartbeat: DefaultHeartbeat},
	}
	if len(feeds) != 2 || feeds[0] != want[0] || feeds[1] != want[1] {
		t.Fatalf("feeds = %+v", feeds)
	}
	if feeds, err := ParseFeeds(""); err != nil || len(feeds) != 0 {
		t.Fatalf("empty spec: %v %v", feeds, err)
	}

	for _, bad := range []string{
		weth + ":" + usdc + ":" + feed,                  // 字段不够
		weth + ":" + usdc + ":" + feed + ":18:6:0",      // 心跳为 0
		weth + ":" + usdc + ":" + feed + ":18:x",        // decimals 不是数字
		weth + ":" + usdc + ":" + feed + ":18:99",       // decimals 过大
		weth + ":" + weth + ":" + feed + ":18:18",       // base == quote
		"not-an-address:" + usdc + ":" + feed + ":18:6", // 地址非法
	} {
		if _, err := ParseFeeds(bad); err == nil {
			t.Errorf("ParseFeeds(%q) should fail", bad)
		}
	}
}

func TestValidateRound(t *testing.T) {
	if err := ValidateRound(ptr(goodRound()), time.Hour); err != nil {
		t.Fatalf("good round rejected: %v", err)
	}
	// 刚好踩在心跳阈值上仍然有效
	edge := goodRound()
	edge.BlockTime = edge.UpdatedAt + 3600
	if err := ValidateRound(&edge, time.Hour); err != nil {
		t.Fatalf("round exactly at heartbeat rejected: %v", err)
	}

	for name, tc := range map[string]struct {
		mutate func(r *chain.ChainlinkRound)
		reason string
	}{
		"answer 为 0":    {func(r *chain.ChainlinkRound) { r.Answer = big.NewInt(0) }, "不是正数"},
		"answer 为负":     {func(r *chain.ChainlinkRound) { r.Answer = big.NewInt(-5) }, "不是正数"},
		"轮次未完成":         {func(r *chain.ChainlinkRound) { r.UpdatedAt = 0 }, "轮次未完成"},
		"答案来自旧轮次":       {func(r *chain.ChainlinkRound) { r.AnsweredInRound = big.NewInt(99) }, "旧轮次"},
		"updatedAt 在未来": {func(r *chain.ChainlinkRound) { r.UpdatedAt = r.BlockTime + 1 }, "晚于"},
		"过期":            {func(r *chain.ChainlinkRound) { r.BlockTime = r.UpdatedAt + 3601 }, "已过期"},
	} {
		r := goodRound()
		tc.mutate(&r)
		if err := ValidateRound(&r, time.Hour); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: err = %v, want containing %q", name, err, tc.reason)
		}
	}
}

func ptr(r chain.ChainlinkRound) *chain.ChainlinkRound { return &r }

// 价格方向和 decimals 的换算：同一个喂价，池子里 token 顺序不同，换算结果互为倒数关系
func TestPoolPriceOrientationAndDecimals(t *testing.T) {
	approx := func(got, want float64) bool { return math.Abs(got-want)/want < 1e-12 }
	ethUSD := FeedConfig{Base: weth, Quote: usdc, BaseDecimals: 18, QuoteDecimals: 6}

	// WETH 是 token0：1 wei WETH = 2000 × 10^(6-18) 个 USDC 最小单位
	if got := ethUSD.PoolPrice(2000, weth); !approx(got, 2e-9) {
		t.Errorf("WETH as token0: %v, want 2e-9", got)
	}
	// USDC 是 token0：1 个 USDC 最小单位 = (1/2000) × 10^(18-6) wei WETH
	if got := ethUSD.PoolPrice(2000, usdc); !approx(got, 5e8) {
		t.Errorf("USDC as token0: %v, want 5e8", got)
	}
	// 两个方向互为倒数
	if got := ethUSD.PoolPrice(2000, weth) * ethUSD.PoolPrice(2000, usdc); !approx(got, 1) {
		t.Errorf("orientations should be reciprocal, product = %v", got)
	}

	// 与池子的真实口径对照：池子里放 1 WETH(1e18 wei) 对 2000 USDC(2000e6 最小单位)，
	// sqrtPriceX96 = sqrt(amount1/amount0) × 2^96，换算出的池价应与喂价换算结果一致
	poolPrice := func(amount1, amount0 *big.Int) float64 {
		ratio := new(big.Int).Lsh(amount1, 192)
		sqrt := new(big.Int).Sqrt(ratio.Div(ratio, amount0))
		return umath.SqrtPriceX96ToPrice(sqrt)
	}
	oneWETH := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	twoThousandUSDC := big.NewInt(2000_000000)
	if got, want := ethUSD.PoolPrice(2000, weth), poolPrice(twoThousandUSDC, oneWETH); math.Abs(got-want)/want > 1e-9 {
		t.Errorf("WETH/USDC pool: feed %v, pool %v", got, want)
	}
	if got, want := ethUSD.PoolPrice(2000, usdc), poolPrice(oneWETH, twoThousandUSDC); math.Abs(got-want)/want > 1e-9 {
		t.Errorf("USDC/WETH pool: feed %v, pool %v", got, want)
	}

	// decimals 相同的币对不需要缩放；喂价配置成反方向（base 是 token1）时取倒数
	same := FeedConfig{Base: usdc, Quote: weth, BaseDecimals: 18, QuoteDecimals: 18}
	if got := same.PoolPrice(4, usdc); !approx(got, 4) {
		t.Errorf("same decimals, base is token0: %v", got)
	}
	if got := same.PoolPrice(4, weth); !approx(got, 0.25) {
		t.Errorf("same decimals, base is token1: %v", got)
	}
}

func newReader(src *fakeFeed, refresh time.Duration) *ChainlinkReader {
	return NewChainlinkReader(src, []FeedConfig{
		{Base: weth, Quote: usdc, Address: feed, BaseDecimals: 18, QuoteDecimals: 6, Heartbeat: time.Hour},
	}, refresh)
}

func TestReaderNormalizesByFeedDecimals(t *testing.T) {
	ctx := context.Background()
	src := &fakeFeed{decimals: 8, round: goodRound()}
	r := newReader(src, time.Minute)

	// 池子的 token 顺序由地址大小决定，两种顺序都要能查到同一个喂价
	got := r.Read(ctx, weth, usdc)
	if got.Status != ChainlinkOK || math.Abs(got.Price-2e-9)/2e-9 > 1e-12 || got.UpdatedAt != 1_700_000_000 {
		t.Fatalf("weth/usdc: %+v", got)
	}
	if got := r.Read(ctx, usdc, weth); got.Status != ChainlinkOK || math.Abs(got.Price-5e8)/5e8 > 1e-12 {
		t.Fatalf("usdc/weth: %+v", got)
	}

	// 18 位小数的喂价（如 ETH 计价的喂价）
	src18 := &fakeFeed{decimals: 18, round: goodRound()}
	src18.round.Answer, _ = new(big.Int).SetString("2000000000000000000000", 10)
	if got := newReader(src18, time.Minute).Read(ctx, weth, usdc); math.Abs(got.Price-2e-9)/2e-9 > 1e-12 {
		t.Fatalf("18-decimal feed: %+v", got)
	}

	// 没有配置喂价的币对；nil 读取器
	if got := r.Read(ctx, weth, "0x00000000000000000000000000000000000000cc"); got.Status != ChainlinkNone {
		t.Fatalf("unconfigured pair: %+v", got)
	}
	var none *ChainlinkReader
	if got := none.Read(ctx, weth, usdc); got.Status != ChainlinkNone {
		t.Fatalf("nil reader: %+v", got)
	}
}

func TestReaderCachesAndRefreshes(t *testing.T) {
	ctx := context.Background()
	src := &fakeFeed{decimals: 8, round: goodRound()}
	r := newReader(src, 50*time.Millisecond)

	for i := 0; i < 20; i++ {
		r.Read(ctx, weth, usdc)
	}
	if src.roundCalls != 1 || src.decimalsCalls != 1 {
		t.Fatalf("within the refresh interval: %d round calls, %d decimals calls, want 1 and 1", src.roundCalls, src.decimalsCalls)
	}

	// 过了缓存时间：重新读一次价格，decimals 不再读
	src.round.Answer = big.NewInt(2100_00000000)
	time.Sleep(60 * time.Millisecond)
	if got := r.Read(ctx, weth, usdc); math.Abs(got.Price-2.1e-9)/2.1e-9 > 1e-12 {
		t.Fatalf("after refresh: %+v", got)
	}
	if src.roundCalls != 2 || src.decimalsCalls != 1 {
		t.Fatalf("after refresh: %d round calls, %d decimals calls, want 2 and 1", src.roundCalls, src.decimalsCalls)
	}
}

func TestReaderRejectsBadData(t *testing.T) {
	ctx := context.Background()

	// 过期的喂价：不可用，且不给出价格
	src := &fakeFeed{decimals: 8, round: goodRound()}
	src.round.BlockTime = src.round.UpdatedAt + 7200
	got := newReader(src, time.Minute).Read(ctx, weth, usdc)
	if got.Status != ChainlinkUnavailable || got.Price != 0 || !strings.Contains(got.Detail, "已过期") {
		t.Fatalf("stale feed: %+v", got)
	}

	// 节点查询失败：不可用，不缓存失败结果，节点恢复后下一次查询就能读到
	down := &fakeFeed{decimals: 8, round: goodRound(), err: errors.New("circuit breaker is open")}
	r := newReader(down, time.Minute)
	if got := r.Read(ctx, weth, usdc); got.Status != ChainlinkUnavailable || !strings.Contains(got.Detail, "节点查询失败") {
		t.Fatalf("node down: %+v", got)
	}
	down.err = nil
	if got := r.Read(ctx, weth, usdc); got.Status != ChainlinkOK {
		t.Fatalf("after recovery: %+v", got)
	}

	// 喂价先正常、后来变坏：刷新后不能继续沿用之前的好价格
	flaky := &fakeFeed{decimals: 8, round: goodRound()}
	r = newReader(flaky, 10*time.Millisecond)
	if got := r.Read(ctx, weth, usdc); got.Status != ChainlinkOK {
		t.Fatalf("initially: %+v", got)
	}
	flaky.round.UpdatedAt = 0
	time.Sleep(15 * time.Millisecond)
	if got := r.Read(ctx, weth, usdc); got.Status != ChainlinkUnavailable || got.Price != 0 {
		t.Fatalf("after the feed went bad: %+v", got)
	}
}

func TestCheckChainlinkThreeWayAndDegrade(t *testing.T) {
	o := newOracle() // MaxDeviation 2%，MaxTWAPDeviation 5%

	// 三方一致
	ok := FeedReading{Status: ChainlinkOK, Price: 1.0, UpdatedAt: 1}
	if c := o.CheckChainlink(pair, 1.01, ok); c.Status != ChainlinkOK || c.Deviation < 0.0099 || c.Deviation > 0.0101 {
		t.Fatalf("within tolerance: %+v", c)
	}
	o.CheckTWAP(pair, 1.01, 1.0)
	if o.GetProtectionStatus(pair).Active {
		t.Fatal("all three sources agree, protection should be off")
	}
	if info := o.GetPriceInfo(pair); info["chainlink"] == nil || info["twap"] == nil || info["pool"] == nil {
		t.Fatalf("three sources should be recorded: %+v", info)
	}

	// 现价偏离喂价 10%
	if c := o.CheckChainlink(pair, 1.10, ok); c.Status != ChainlinkDeviated {
		t.Fatalf("deviated: %+v", c)
	}
	if status := o.GetProtectionStatus(pair); !status.Active || !strings.Contains(status.Reason, "Chainlink") {
		t.Fatalf("protection: %+v", status)
	}

	// 喂价不可用：Chainlink 一方被清掉，只剩现价 vs TWAP 两方；现价与 TWAP 一致时不再因为旧喂价而保护
	bad := FeedReading{Status: ChainlinkUnavailable, Detail: "喂价已过期"}
	if c := o.CheckChainlink(pair, 1.10, bad); c.Status != ChainlinkUnavailable || c.Detail == "" || c.Price != 0 {
		t.Fatalf("unavailable: %+v", c)
	}
	if info := o.GetPriceInfo(pair); info["chainlink"] != nil {
		t.Fatal("an unusable feed must not stay in the cross validation")
	}
	o.CheckTWAP(pair, 1.10, 1.10)
	if o.GetProtectionStatus(pair).Active {
		t.Fatal("with the bad feed dropped, spot == twap should lift protection")
	}

	// 没配喂价：什么都不动
	if c := o.CheckChainlink(pair, 1.0, FeedReading{Status: ChainlinkNone}); c.Status != ChainlinkNone {
		t.Fatalf("none: %+v", c)
	}
}
