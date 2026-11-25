package api

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"unigo-dex/cache"
	"unigo-dex/gas"
	"unigo-dex/indexer"
	"unigo-dex/kline"
	"unigo-dex/mev"
	"unigo-dex/oracle"
	"unigo-dex/router"
)

const (
	tokenA   = "0xaaaa"
	tokenB   = "0xbbbb"
	testPool = "0xpool"
)

type testServer struct {
	http.Handler
	klines *kline.Aggregator
	guard  *mev.Guard
}

// newTestServer 不连接节点：一个价格 1:1、流动性 1e24 的池子
func newTestServer() *testServer {
	state := indexer.NewPoolStateManager()
	liquidity, _ := new(big.Int).SetString("1000000000000000000000000", 10)
	pool := indexer.NewPoolState(testPool, tokenA, tokenB, 3000, 60)
	pool.ApplySwap(&indexer.SwapEvent{SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: liquidity})
	state.AddPool(pool)

	klines := kline.NewAggregator(kline.DefaultConfig())
	guard := mev.NewGuard(mev.DefaultConfig())
	s := NewServer(
		DefaultServerConfig(),
		router.NewRouter(router.DefaultRouterConfig(), state),
		state,
		indexer.NewIndexer(indexer.DefaultConfig(), state, nil),
		cache.NewCache(time.Second),
		oracle.NewOracle(oracle.DefaultOracleConfig()),
		nil,
		gas.NewEstimator(gas.DefaultConfig(), nil),
		klines,
		guard,
	)
	return &testServer{Handler: s.Handler(), klines: klines, guard: guard}
}

func (s *testServer) do(t *testing.T, method, url, body string, out interface{}) int {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: bad JSON %q: %v", method, url, rec.Body.String(), err)
		}
	}
	return rec.Code
}

func testSwap(block uint64, logIndex uint, who string, buy bool, ts time.Time) *indexer.SwapEvent {
	amount0, amount1 := big.NewInt(100), big.NewInt(-100)
	if !buy {
		amount0, amount1 = big.NewInt(-100), big.NewInt(100)
	}
	return &indexer.SwapEvent{
		PoolAddress: testPool, Sender: who, Recipient: who,
		Amount0: amount0, Amount1: amount1,
		SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96),
		BlockNumber:  block, LogIndex: logIndex, Timestamp: uint64(ts.Unix()),
		TxHash: fmt.Sprintf("0xtx%d-%d", block, logIndex),
	}
}

func TestQuoteIncludesImpactSlippageAndGas(t *testing.T) {
	s := newTestServer()
	var resp QuoteResponse
	// token 地址大小写不敏感
	url := "/api/v1/quote?token_in=0xAAAA&token_out=0xbbbb&amount_in=1000000000000000000"
	if code := s.do(t, "GET", url, "", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	out, _ := new(big.Int).SetString(resp.AmountOut, 10)
	if out.Cmp(big.NewInt(996e15)) <= 0 || out.Cmp(big.NewInt(997e15)) >= 0 {
		t.Fatalf("amount_out = %s", resp.AmountOut)
	}
	// 1e18 对 1e24 流动性：冲击很小但不为 0
	if resp.PriceImpact <= 0 || resp.PriceImpact > 0.0001 {
		t.Fatalf("price_impact = %v", resp.PriceImpact)
	}
	if resp.Slippage != 0.005 || resp.MinAmountOut != mev.MinAmountOut(out, 0.005).String() {
		t.Fatalf("slippage = %v, min_amount_out = %s", resp.Slippage, resp.MinAmountOut)
	}
	if resp.Gas.Source != "static" || resp.Gas.GasLimit != resp.GasEstimate || resp.Gas.Cost == "" || resp.Gas.Cost == "0" {
		t.Fatalf("gas = %+v", resp.Gas)
	}
	if resp.Warning != "" {
		t.Fatalf("unexpected warning %q", resp.Warning)
	}

	// 池子刚被夹过：报价给出提示并收紧滑点
	now := time.Now()
	s.guard.OnSwap(testSwap(5, 0, "0xbot", true, now))
	s.guard.OnSwap(testSwap(5, 1, "0xuser", true, now))
	s.guard.OnSwap(testSwap(5, 2, "0xbot", false, now))

	if code := s.do(t, "GET", url+"&slippage=0.01", "", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Slippage != 0.001 || !strings.Contains(resp.Warning, "三明治") {
		t.Fatalf("after sandwich: slippage = %v, warning = %q", resp.Slippage, resp.Warning)
	}

	var alerts []mev.Alert
	if code := s.do(t, "GET", "/api/v1/mev/alerts", "", &alerts); code != http.StatusOK || len(alerts) != 1 || alerts[0].Attacker != "0xbot" {
		t.Fatalf("alerts: code = %d, %+v", code, alerts)
	}
}

func TestSwapPreTradeProtection(t *testing.T) {
	s := newTestServer()
	deadline := time.Now().Add(10 * time.Minute).Unix()
	body := func(amountIn string, deadline int64, extra string) string {
		return fmt.Sprintf(`{"token_in":%q,"token_out":%q,"amount_in":%q,"slippage":0.005,"deadline":%d%s}`,
			tokenA, tokenB, amountIn, deadline, extra)
	}

	var ok SwapResponse
	if code := s.do(t, "POST", "/api/v1/swap", body("1000000000000000000", deadline, ""), &ok); code != http.StatusOK {
		t.Fatalf("valid swap: status = %d", code)
	}
	if ok.Status != "validated" || ok.TxHash != "" || ok.MinAmountOut == "" || ok.PriceImpact <= 0 {
		t.Fatalf("valid swap: %+v", ok)
	}

	for name, tc := range map[string]struct {
		body string
		code int
		msg  string
	}{
		"价格冲击过大":       {body("500000000000000000000000", deadline, ""), http.StatusBadRequest, "price impact too high"},
		"deadline 已过期": {body("1000000000000000000", time.Now().Add(-time.Minute).Unix(), ""), http.StatusBadRequest, "deadline expired"},
		"deadline 过长":  {body("1000000000000000000", time.Now().Add(24*time.Hour).Unix(), ""), http.StatusBadRequest, "deadline too far"},
		"没有路由":         {strings.Replace(body("1000", deadline, ""), tokenB, "0xcccc", 1), http.StatusBadRequest, "route search failed"},
		"金额非法":         {body("-5", deadline, ""), http.StatusBadRequest, "invalid amount_in"},
		"未连接节点时无法广播":   {body("1000000000000000000", deadline, `,"raw_tx":"0x02"`), http.StatusServiceUnavailable, "not configured"},
	} {
		var resp ErrorResponse
		if code := s.do(t, "POST", "/api/v1/swap", tc.body, &resp); code != tc.code || !strings.Contains(resp.Error, tc.msg) {
			t.Errorf("%s: code = %d, error = %q", name, code, resp.Error)
		}
	}
}

func TestKlinesAndTradesEndpoints(t *testing.T) {
	s := newTestServer()
	ts := time.Unix(1_700_000_000, 0)
	s.klines.OnSwap(testSwap(1, 0, "0xuser", true, ts))
	s.klines.OnSwap(testSwap(2, 0, "0xuser", false, ts.Add(90*time.Second)))

	var candles []kline.Candle
	if code := s.do(t, "GET", "/api/v1/klines?pool=0xPOOL&interval=1m", "", &candles); code != http.StatusOK || len(candles) != 2 {
		t.Fatalf("klines: code = %d, %+v", code, candles)
	}
	if candles[0].Close != 1 || candles[0].Volume0 != 100 || candles[0].Trades != 1 {
		t.Fatalf("candle = %+v", candles[0])
	}
	if code := s.do(t, "GET", "/api/v1/klines?pool=0xpool&interval=1h&limit=1", "", &candles); code != http.StatusOK || len(candles) != 1 || candles[0].Trades != 2 {
		t.Fatalf("1h klines: code = %d, %+v", code, candles)
	}

	var trades []kline.Trade
	if code := s.do(t, "GET", "/api/v1/trades?pool=0xpool&limit=1", "", &trades); code != http.StatusOK || len(trades) != 1 || trades[0].BlockNumber != 2 {
		t.Fatalf("trades: code = %d, %+v", code, trades)
	}

	for _, url := range []string{
		"/api/v1/klines?pool=0xpool&interval=7m",
		"/api/v1/klines?interval=1m",
		"/api/v1/klines?pool=0xpool&limit=abc",
		"/api/v1/trades",
		"/api/v1/mev/alerts?limit=0",
	} {
		if code := s.do(t, "GET", url, "", nil); code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", url, code)
		}
	}
}

func TestTxStatusRequiresNode(t *testing.T) {
	s := newTestServer()
	var resp ErrorResponse
	if code := s.do(t, "GET", "/api/v1/tx/0x1234", "", &resp); code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, %+v", code, resp)
	}
	if code := s.do(t, "GET", "/metrics", "", nil); code != http.StatusOK {
		t.Fatalf("/metrics code = %d", code)
	}
}
