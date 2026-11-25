package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"unigo-dex/arb"
	"unigo-dex/cache"
	"unigo-dex/chain"
	"unigo-dex/chain/contractabi"
	"unigo-dex/eip712"
	"unigo-dex/gas"
	"unigo-dex/indexer"
	"unigo-dex/kline"
	umath "unigo-dex/math"
	"unigo-dex/mev"
	"unigo-dex/oracle"
	"unigo-dex/router"
	"unigo-dex/ws"
)

const (
	tokenA      = "0x00000000000000000000000000000000000000aa"
	tokenB      = "0x00000000000000000000000000000000000000bb"
	tokenC      = "0x00000000000000000000000000000000000000cc"
	testPool    = "0x0000000000000000000000000000000000001001" // A/B 0.3%
	testPoolBC  = "0x0000000000000000000000000000000000001002" // B/C 0.3%
	testManager = "0x0000000000000000000000000000000000002001"
)

type testServer struct {
	http.Handler
	klines   *kline.Aggregator
	guard    *mev.Guard
	state    *indexer.PoolStateManager
	scanner  *arb.Scanner
	hub      *ws.Hub
	verifier *eip712.Verifier
	key      *ecdsa.PrivateKey
	owner    string
}

func addTestPool(state *indexer.PoolStateManager, addr, token0, token1 string) {
	liquidity, _ := new(big.Int).SetString("1000000000000000000000000", 10)
	pool := indexer.NewPoolState(addr, token0, token1, 3000, 60)
	pool.ApplyInitialize(&indexer.InitializeEvent{SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96)})
	pool.ApplyMint(&indexer.MintEvent{TickLower: -6000, TickUpper: 6000, Amount: liquidity})
	state.AddPool(pool)
}

// newTestServer 不连接节点：A/B 和 B/C 两个价格 1:1、流动性 1e24 的池子
func newTestServer() *testServer {
	return newTestServerWith(func(*ServerConfig) {})
}

func newTestServerWith(configure func(*ServerConfig)) *testServer {
	state := indexer.NewPoolStateManager()
	addTestPool(state, testPool, tokenA, tokenB)
	addTestPool(state, testPoolBC, tokenB, tokenC)

	key, _ := crypto.GenerateKey()
	gasEstimator := gas.NewEstimator(gas.DefaultConfig(), nil)
	ts := &testServer{
		klines:  kline.NewAggregator(kline.DefaultConfig()),
		guard:   mev.NewGuard(mev.DefaultConfig()),
		state:   state,
		scanner: arb.NewScanner(arb.DefaultConfig(), state, gasEstimator),
		hub:     ws.NewHub(),
		verifier: eip712.NewVerifier(eip712.DomainSeparator{
			Name: "UniGo-DEX", Version: "1", ChainID: 31337, VerifyingContract: testManager,
		}, 5*time.Minute),
		key:   key,
		owner: strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex()),
	}
	config := DefaultServerConfig()
	config.ManagerAddress = testManager
	configure(&config)
	ts.Handler = NewServer(config, Deps{
		Router:   router.NewRouter(router.DefaultRouterConfig(), state),
		State:    state,
		Indexer:  indexer.NewIndexer(indexer.DefaultConfig(), state, nil),
		Cache:    cache.NewCache(time.Second),
		Oracle:   oracle.NewOracle(oracle.DefaultOracleConfig()),
		Gas:      gasEstimator,
		Klines:   ts.klines,
		Guard:    ts.guard,
		Arb:      ts.scanner,
		Hub:      ts.hub,
		Verifier: ts.verifier,
	}).Handler()
	return ts
}

// swapBody 构造一个带合法 EIP-712 签名的 swap 请求体；mutate 在签名之后改写请求
func (s *testServer) swapBody(t *testing.T, tokenOut, amountIn, minAmountOut string, deadline int64, mutate func(*SwapRequest)) string {
	t.Helper()
	req := SwapRequest{
		Owner: s.owner, TokenIn: tokenA, TokenOut: tokenOut, AmountIn: amountIn, MinAmountOut: minAmountOut,
		Slippage: 0.005, Nonce: s.verifier.GetNonce(s.owner), Deadline: deadline,
	}
	hash, err := s.verifier.Hash(&eip712.SwapPermit{
		Owner: req.Owner, TokenIn: req.TokenIn, TokenOut: req.TokenOut, AmountIn: req.AmountIn,
		MinAmountOut: req.MinAmountOut, Nonce: req.Nonce, Deadline: req.Deadline,
	})
	if err != nil {
		// 字段本身非法时无法签名，交给服务端去拒绝
		hash = make([]byte, 32)
	}
	sig, err := eip712.SignPermit(s.key, hash)
	if err != nil {
		t.Fatal(err)
	}
	req.Signature = sig.Hex()
	if mutate != nil {
		mutate(&req)
	}
	body, _ := json.Marshal(req)
	return string(body)
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
	url := "/api/v1/quote?token_in=0x00000000000000000000000000000000000000AA&token_out=" + tokenB + "&amount_in=1000000000000000000"
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
	const oneToken, minOut = "1000000000000000000", "990000000000000000"

	var ok SwapResponse
	if code := s.do(t, "POST", "/api/v1/swap", s.swapBody(t, tokenB, oneToken, minOut, deadline, nil), &ok); code != http.StatusOK {
		t.Fatalf("valid swap: status = %d", code)
	}
	if ok.Status != "validated" || ok.TxHash != "" || ok.MinAmountOut == "" || ok.PriceImpact <= 0 {
		t.Fatalf("valid swap: %+v", ok)
	}

	for name, tc := range map[string]struct {
		body func() string
		code int
		msg  string
	}{
		"价格冲击过大": {func() string { return s.swapBody(t, tokenB, "100000000000000000000000", "1", deadline, nil) }, http.StatusBadRequest, "price impact too high"},
		"deadline 过长": {func() string {
			return s.swapBody(t, tokenB, oneToken, minOut, time.Now().Add(24*time.Hour).Unix(), nil)
		}, http.StatusBadRequest, "deadline too far"},
		"min_out 过松": {func() string { return s.swapBody(t, tokenB, oneToken, "1", deadline, nil) }, http.StatusBadRequest, "min_amount_out too low"},
		"没有路由": {func() string {
			return s.swapBody(t, "0x00000000000000000000000000000000000000dd", "1000", "1", deadline, nil)
		}, http.StatusBadRequest, "route search failed"},
		"金额非法": {func() string { return s.swapBody(t, tokenB, "-5", minOut, deadline, nil) }, http.StatusBadRequest, "invalid amount_in"},
		"raw_tx 不是合法交易": {func() string {
			return s.swapBody(t, tokenB, oneToken, minOut, deadline, func(r *SwapRequest) { r.RawTx = "0x02" })
		}, http.StatusServiceUnavailable, "not configured"},
	} {
		var resp ErrorResponse
		if code := s.do(t, "POST", "/api/v1/swap", tc.body(), &resp); code != tc.code || !strings.Contains(resp.Error, tc.msg) {
			t.Errorf("%s: code = %d, error = %q", name, code, resp.Error)
		}
	}
}

func TestSwapRequiresValidSignature(t *testing.T) {
	s := newTestServer()
	deadline := time.Now().Add(10 * time.Minute).Unix()
	const oneToken, minOut = "1000000000000000000", "990000000000000000"
	post := func(body string) (int, string) {
		var resp ErrorResponse
		code := s.do(t, "POST", "/api/v1/swap", body, &resp)
		return code, resp.Error
	}

	var nonce struct {
		Address string `json:"address"`
		Nonce   uint64 `json:"nonce"`
	}
	if code := s.do(t, "GET", "/api/v1/nonce/"+s.owner, "", &nonce); code != http.StatusOK || nonce.Nonce != 0 || nonce.Address != s.owner {
		t.Fatalf("nonce: code = %d, %+v", code, nonce)
	}

	// 签名之后篡改请求：恢复出的签名者对不上
	tampered := s.swapBody(t, tokenB, oneToken, minOut, deadline, func(r *SwapRequest) { r.MinAmountOut = "1" })
	if code, msg := post(tampered); code != http.StatusUnauthorized || !strings.Contains(msg, "signature rejected") {
		t.Fatalf("tampered request: %d %q", code, msg)
	}
	// 没有签名
	if code, _ := post(s.swapBody(t, tokenB, oneToken, minOut, deadline, func(r *SwapRequest) { r.Signature = "" })); code != http.StatusUnauthorized {
		t.Fatalf("missing signature: %d", code)
	}
	// 冒充别人
	other := "0x00000000000000000000000000000000000000ee"
	if code, _ := post(s.swapBody(t, tokenB, oneToken, minOut, deadline, func(r *SwapRequest) { r.Owner = other })); code != http.StatusUnauthorized {
		t.Fatalf("impersonation: %d", code)
	}
	// 签名已过期
	if code, msg := post(s.swapBody(t, tokenB, oneToken, minOut, time.Now().Add(-time.Minute).Unix(), nil)); code != http.StatusUnauthorized || !strings.Contains(msg, "过期") {
		t.Fatalf("expired: %d %q", code, msg)
	}

	// 合法请求通过，nonce 递增；原样重放被拒
	valid := s.swapBody(t, tokenB, oneToken, minOut, deadline, nil)
	if code, msg := post(valid); code != http.StatusOK {
		t.Fatalf("valid request: %d %q", code, msg)
	}
	if code, msg := post(valid); code != http.StatusUnauthorized || !strings.Contains(msg, "nonce") {
		t.Fatalf("replay: %d %q", code, msg)
	}
	s.do(t, "GET", "/api/v1/nonce/"+s.owner, "", &nonce)
	if nonce.Nonce != 1 {
		t.Fatalf("nonce after one accepted swap = %d, want 1", nonce.Nonce)
	}
	if code := s.do(t, "GET", "/api/v1/nonce/not-an-address", "", nil); code != http.StatusBadRequest {
		t.Fatalf("bad address: %d", code)
	}
}

// decodeManagerCall 用 Manager ABI 解出调用数据里的方法名和结构体参数
func decodeManagerCall(t *testing.T, data string, out interface{}) string {
	t.Helper()
	raw, err := hexutil.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	method, err := contractabi.Manager.MethodById(raw[:4])
	if err != nil {
		t.Fatal(err)
	}
	args, err := method.Inputs.Unpack(raw[4:])
	if err != nil {
		t.Fatal(err)
	}
	if err := method.Inputs.Copy(out, args); err != nil {
		t.Fatal(err)
	}
	return method.Name
}

func TestBuildSwap(t *testing.T) {
	s := newTestServer()
	recipient := "0x00000000000000000000000000000000000000f1"
	deadline := time.Now().Add(5 * time.Minute).Unix()
	build := func(fields string) (BuildSwapResponse, int, string) {
		var resp BuildSwapResponse
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/swap/build", strings.NewReader("{"+fields+"}")))
		json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp, rec.Code, rec.Body.String()
	}
	common := fmt.Sprintf(`"token_in":%q,"recipient":%q,"deadline":%d,"slippage":0.01`, tokenA, recipient, deadline)

	// 单跳精确输入 → Manager.swap
	resp, code, body := build(common + fmt.Sprintf(`,"token_out":%q,"amount_in":"1000000000000000000"`, tokenB))
	if code != http.StatusOK {
		t.Fatalf("single hop: %d %s", code, body)
	}
	var swap struct {
		Params struct { // 字段顺序与 ABI 一致
			Pool              ethcommon.Address
			ZeroForOne        bool
			AmountSpecified   *big.Int
			SqrtPriceLimitX96 *big.Int
			Recipient         ethcommon.Address
			AmountOutMinimum  *big.Int
			Deadline          *big.Int
		}
	}
	if m := decodeManagerCall(t, resp.Data, &swap); m != "swap" || resp.Method != "swap" {
		t.Fatalf("method = %s / %s", m, resp.Method)
	}
	amountOut, _ := new(big.Int).SetString(resp.AmountOut, 10)
	if p := swap.Params; p.Pool != ethcommon.HexToAddress(testPool) || !p.ZeroForOne ||
		p.AmountSpecified.String() != "1000000000000000000" || p.Recipient != ethcommon.HexToAddress(recipient) ||
		p.Deadline.Int64() != deadline || p.AmountOutMinimum.Cmp(mev.MinAmountOut(amountOut, 0.01)) != 0 ||
		p.AmountOutMinimum.String() != resp.MinAmountOut {
		t.Fatalf("swap params: %+v", p)
	}
	if resp.To != testManager || resp.Value != "0" || resp.GasLimit == 0 || resp.GasLimitSource != "router" || resp.Gas.Cost == "" {
		t.Fatalf("tx envelope: %+v", resp)
	}
	if resp.QuoteConsistent != nil {
		t.Fatal("without a node there is no on-chain cross-check")
	}

	// 两跳精确输入 → Manager.exactInput，路径是 token+fee+token+fee+token
	resp, code, body = build(common + fmt.Sprintf(`,"token_out":%q,"amount_in":"1000000000000000000"`, tokenC))
	if code != http.StatusOK {
		t.Fatalf("multi hop: %d %s", code, body)
	}
	var exactInput struct {
		Params struct {
			Path                                 []byte
			Recipient                            ethcommon.Address
			AmountIn, AmountOutMinimum, Deadline *big.Int
		}
	}
	if m := decodeManagerCall(t, resp.Data, &exactInput); m != "exactInput" {
		t.Fatalf("method = %s", m)
	}
	wantPath, _ := chain.EncodePath([]string{tokenA, tokenB, tokenC}, []uint32{3000, 3000})
	if !bytes.Equal(exactInput.Params.Path, wantPath) || exactInput.Params.AmountOutMinimum.String() != resp.MinAmountOut ||
		len(resp.Path) != 3 || len(resp.Fees) != 2 {
		t.Fatalf("exactInput params: %+v, resp path %v fees %v", exactInput.Params, resp.Path, resp.Fees)
	}

	// 精确输出 → Manager.exactOutputSingle，带最多付出数量
	resp, code, body = build(common + fmt.Sprintf(`,"token_out":%q,"amount_out":"500000000000000000"`, tokenB))
	if code != http.StatusOK {
		t.Fatalf("exact output: %d %s", code, body)
	}
	var exactOutput struct {
		Params struct { // 字段顺序与 ABI 一致
			TokenIn           ethcommon.Address
			TokenOut          ethcommon.Address
			Fee               *big.Int
			Recipient         ethcommon.Address
			AmountOut         *big.Int
			AmountInMaximum   *big.Int
			SqrtPriceLimitX96 *big.Int
			Deadline          *big.Int
		}
	}
	if m := decodeManagerCall(t, resp.Data, &exactOutput); m != "exactOutputSingle" {
		t.Fatalf("method = %s", m)
	}
	amountIn, _ := new(big.Int).SetString(resp.AmountIn, 10)
	if p := exactOutput.Params; p.AmountOut.String() != "500000000000000000" || p.Fee.Int64() != 3000 ||
		p.AmountInMaximum.Cmp(mev.MaxAmountIn(amountIn, 0.01)) != 0 || p.AmountInMaximum.String() != resp.MaxAmountIn ||
		amountIn.Cmp(p.AmountOut) <= 0 {
		t.Fatalf("exactOutputSingle params: %+v (amount_in %s)", p, amountIn)
	}

	// 拒绝的情况
	for name, tc := range map[string]struct {
		fields string
		code   int
	}{
		"两个数量都给":   {common + fmt.Sprintf(`,"token_out":%q,"amount_in":"1","amount_out":"1"`, tokenB), http.StatusBadRequest},
		"都不给":      {common + fmt.Sprintf(`,"token_out":%q`, tokenB), http.StatusBadRequest},
		"地址非法":     {fmt.Sprintf(`"token_in":"abc","token_out":%q,"recipient":%q,"amount_in":"1"`, tokenB, recipient), http.StatusBadRequest},
		"价格冲击过大":   {common + fmt.Sprintf(`,"token_out":%q,"amount_in":"100000000000000000000000"`, tokenB), http.StatusBadRequest},
		"精确输出不能多跳": {common + fmt.Sprintf(`,"token_out":%q,"amount_out":"1000"`, tokenC), http.StatusBadRequest},
	} {
		if _, code, body := build(tc.fields); code != tc.code {
			t.Errorf("%s: code = %d, body = %s", name, code, body)
		}
	}

	// 没有配置 Manager 地址
	noManager := newTestServerWith(func(c *ServerConfig) { c.ManagerAddress = "" })
	rec := httptest.NewRecorder()
	noManager.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/swap/build", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no manager: %d", rec.Code)
	}
}

func TestArbitrageAndStreamEndpoints(t *testing.T) {
	s := newTestServer()

	var opps []arb.Opportunity
	if code := s.do(t, "GET", "/api/v1/arbitrage/opportunities", "", &opps); code != http.StatusOK || len(opps) != 0 {
		t.Fatalf("no opportunities expected: %d %+v", code, opps)
	}

	// 加一个比 testPool 贵约 2% 的同币对池子，再加一个能借出 A 的池子
	high := indexer.NewPoolState("0x0000000000000000000000000000000000001003", tokenA, tokenB, 500, 10)
	sqrtPrice, _ := umath.GetSqrtRatioAtTick(200)
	liquidity, _ := new(big.Int).SetString("1000000000000000000000000", 10)
	high.ApplyInitialize(&indexer.InitializeEvent{SqrtPriceX96: sqrtPrice, Tick: 200})
	high.ApplyMint(&indexer.MintEvent{TickLower: -6000, TickUpper: 6000, Amount: liquidity})
	s.state.AddPool(high)
	addTestPool(s.state, "0x0000000000000000000000000000000000001004", tokenA, tokenC)
	s.scanner.OnSwap(&indexer.SwapEvent{PoolAddress: high.Address})

	if code := s.do(t, "GET", "/api/v1/arbitrage/opportunities", "", &opps); code != http.StatusOK || len(opps) == 0 {
		t.Fatalf("opportunities: %d %+v", code, opps)
	}
	if !strings.HasPrefix(opps[0].Calldata, "0x") || opps[0].NetProfit == "" || opps[0].Amount == "" {
		t.Fatalf("opportunity: %+v", opps[0])
	}

	// SSE 经过完整的中间件链也能推送
	srv := httptest.NewServer(s.Handler)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/stream?types=trade", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for s.hub.ClientCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	s.hub.Broadcast("trade", map[string]string{"tx_hash": "0xabc"})

	reader := bufio.NewReader(resp.Body)
	var lines []string
	for len(lines) < 2 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if lines[0] != "event: trade" || lines[1] != `data: {"tx_hash":"0xabc"}` {
		t.Fatalf("stream lines: %q", lines)
	}
}

func TestKlinesAndTradesEndpoints(t *testing.T) {
	s := newTestServer()
	ts := time.Unix(1_700_000_000, 0)
	s.klines.OnSwap(testSwap(1, 0, "0xuser", true, ts))
	s.klines.OnSwap(testSwap(2, 0, "0xuser", false, ts.Add(90*time.Second)))

	var candles []kline.Candle
	if code := s.do(t, "GET", "/api/v1/klines?pool=0x"+strings.ToUpper(testPool[2:])+"&interval=1m", "", &candles); code != http.StatusOK || len(candles) != 2 {
		t.Fatalf("klines: code = %d, %+v", code, candles)
	}
	if candles[0].Close != 1 || candles[0].Volume0 != 100 || candles[0].Trades != 1 {
		t.Fatalf("candle = %+v", candles[0])
	}
	if code := s.do(t, "GET", "/api/v1/klines?pool="+testPool+"&interval=1h&limit=1", "", &candles); code != http.StatusOK || len(candles) != 1 || candles[0].Trades != 2 {
		t.Fatalf("1h klines: code = %d, %+v", code, candles)
	}

	var trades []kline.Trade
	if code := s.do(t, "GET", "/api/v1/trades?pool="+testPool+"&limit=1", "", &trades); code != http.StatusOK || len(trades) != 1 || trades[0].BlockNumber != 2 {
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

func TestPoolsIncludePrice(t *testing.T) {
	s := newTestServer()
	var pools []PoolInfo
	if code := s.do(t, "GET", "/api/v1/pools", "", &pools); code != http.StatusOK || len(pools) != 2 {
		t.Fatalf("pools: %d %+v", code, pools)
	}
	for _, p := range pools {
		if p.Price != 1 || p.Liquidity != "1000000000000000000000000" {
			t.Fatalf("pool %s: price=%v liquidity=%s", p.Address, p.Price, p.Liquidity)
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
