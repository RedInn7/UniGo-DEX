//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"unigo-dex/api"
	"unigo-dex/arb"
	"unigo-dex/cache"
	"unigo-dex/chain"
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

// e2e_test.go — 端到端集成测试
//
// 运行（需要先在 contracts/ 下装好依赖）：
//
//	cd backend && go test -p 2 -tags integration -count=1 -timeout 15m -v ./integration
//
// 测试自己拉起 Hardhat 节点、用编译产物部署全套合约，再把后端各模块接到这个节点上。
// 默认的 go test ./... 不带 integration 标签，不会跑到这里。

var theNode *node

func TestMain(m *testing.M) {
	n, err := startNode()
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动 Hardhat 节点失败:", err)
		os.Exit(1)
	}
	theNode = n
	code := m.Run()
	n.stop()
	os.Exit(code)
}

// world 一套部署好的合约和接在上面的后端
type world struct {
	n *node

	lp, trader, bot, victim account

	erc20, poolABI, managerABI, aggregatorABI artifact

	factory, manager, quoter, flashArb common.Address
	tokenA, tokenB, tokenC             common.Address // 地址升序
	poolAB3000, poolAB500              common.Address // 同一币对的两个池子（套利用）
	poolBC3000, poolAC10000            common.Address
	feedAB                             common.Address // 模拟的 Chainlink 聚合器：1 个 A 值多少 B

	client   *chain.Client
	state    *indexer.PoolStateManager
	idx      *indexer.Indexer
	router   *router.Router
	verifier *eip712.Verifier
	server   *httptest.Server
	events   *eventLog
}

// 流动性头寸
type position struct {
	pool             common.Address
	tickLower, upper int64
	liquidity        string
}

func deployWorld(t *testing.T) *world {
	n := theNode
	accounts := loadAccounts()
	w := &world{n: n, lp: accounts[0], trader: accounts[1], bot: accounts[2], victim: accounts[3]}

	w.erc20 = loadArtifact(t, "test/TestERC20.sol", "TestERC20")
	w.poolABI = loadArtifact(t, "UniGoPool.sol", "UniGoPool")
	w.managerABI = loadArtifact(t, "UniGoManager.sol", "UniGoManager")
	factory := loadArtifact(t, "UniGoFactory.sol", "UniGoFactory")
	quoter := loadArtifact(t, "UniGoQuoter.sol", "UniGoQuoter")
	flashArb := loadArtifact(t, "FlashArbitrage.sol", "FlashArbitrage")

	// Factory 是部署账户的第一笔交易，地址与 scripts/deploy.js 部署出来的相同
	w.factory = n.deploy(t, w.lp, factory)
	if w.factory != common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3") {
		t.Fatalf("Factory 地址 = %s", w.factory.Hex())
	}
	w.manager = n.deploy(t, w.lp, w.managerABI, w.factory)
	w.quoter = n.deploy(t, w.lp, quoter, w.factory)
	w.flashArb = n.deploy(t, w.lp, flashArb, w.factory)

	tokens := []common.Address{
		n.deploy(t, w.lp, w.erc20, "Token A", "TKA"),
		n.deploy(t, w.lp, w.erc20, "Token B", "TKB"),
		n.deploy(t, w.lp, w.erc20, "Token C", "TKC"),
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].Cmp(tokens[j]) < 0 })
	w.tokenA, w.tokenB, w.tokenC = tokens[0], tokens[1], tokens[2]

	// 每个账户铸币并授权给 Manager
	supply, _ := new(big.Int).SetString("1000000000000000000000000000", 10) // 1e27
	maxUint := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	for _, token := range tokens {
		for _, acct := range []account{w.lp, w.trader, w.bot, w.victim} {
			n.invoke(t, w.lp, token, w.erc20.ABI, "mint", acct.addr, supply)
			n.invoke(t, acct, token, w.erc20.ABI, "approve", w.manager, maxUint)
		}
	}

	// 创建并初始化池子，价格都是 1:1
	createPool := func(token0, token1 common.Address, fee int64) common.Address {
		n.invoke(t, w.lp, w.factory, factory.ABI, "createPool", token0, token1, big.NewInt(fee))
		pool := n.read(t, w.factory, factory.ABI, "getPool", token0, token1, big.NewInt(fee))[0].(common.Address)
		n.invoke(t, w.lp, pool, w.poolABI.ABI, "initialize", new(big.Int).Lsh(big.NewInt(1), 96))
		return pool
	}
	w.poolAB3000 = createPool(w.tokenA, w.tokenB, 3000)
	w.poolAB500 = createPool(w.tokenA, w.tokenB, 500)
	w.poolBC3000 = createPool(w.tokenB, w.tokenC, 3000)
	w.poolAC10000 = createPool(w.tokenA, w.tokenC, 10000)

	// A/B 的 Chainlink 喂价：8 位小数，初始价格 1.0，与池子的初始价格一致
	w.aggregatorABI = loadArtifact(t, "test/TestAggregator.sol", "TestAggregator")
	w.feedAB = n.deploy(t, w.lp, w.aggregatorABI, uint8(8), "A / B", big.NewInt(1_00000000))

	// 每个池子放一个宽区间打底，再叠几个窄区间和单边区间，让 swap 会穿越已初始化的 tick
	for _, p := range []position{
		{w.poolAB3000, -6000, 6000, "1000000000000000000000000"},
		{w.poolAB3000, -120, 120, "5000000000000000000000000"},
		{w.poolAB3000, 60, 600, "2000000000000000000000000"},
		{w.poolAB3000, -1200, -180, "1500000000000000000000000"},
		{w.poolAB500, -6000, 6000, "1000000000000000000000000"},
		{w.poolAB500, -20, 20, "3000000000000000000000000"},
		{w.poolAB500, -200, 100, "2000000000000000000000000"},
		{w.poolBC3000, -6000, 6000, "1000000000000000000000000"},
		{w.poolBC3000, -180, 60, "1000000000000000000000000"},
		{w.poolAC10000, -6000, 6000, "1000000000000000000000000"},
		{w.poolAC10000, -200, 400, "800000000000000000000000"},
	} {
		w.mint(t, w.lp, p)
	}
	return w
}

func (w *world) mint(t *testing.T, from account, p position) {
	t.Helper()
	liquidity, _ := new(big.Int).SetString(p.liquidity, 10)
	w.n.invoke(t, from, w.manager, w.managerABI.ABI, "mint", struct {
		Pool       common.Address
		TickLower  *big.Int
		TickUpper  *big.Int
		Amount     *big.Int
		Amount0Min *big.Int
		Amount1Min *big.Int
		Deadline   *big.Int
	}{p.pool, big.NewInt(p.tickLower), big.NewInt(p.upper), liquidity, big.NewInt(0), big.NewInt(0), farFuture})
}

var farFuture = big.NewInt(4102444800) // 2100 年

// swapOnChain 经 Manager 做一笔单池精确输入的 swap
func (w *world) swapOnChain(t *testing.T, from account, pool common.Address, zeroForOne bool, amountIn *big.Int) {
	t.Helper()
	data, err := chain.EncodeSwap(pool.Hex(), zeroForOne, amountIn, from.addr.Hex(), big.NewInt(0), farFuture.Int64())
	if err != nil {
		t.Fatal(err)
	}
	w.n.send(t, from, w.manager, data)
}

func (w *world) balance(t *testing.T, token, owner common.Address) *big.Int {
	t.Helper()
	return w.n.read(t, token, w.erc20.ABI, "balanceOf", owner)[0].(*big.Int)
}

// TWAP 窗口：集成测试里用 150 秒，观测不足 60 秒视为不可用
const (
	twapWindow    = 150
	twapMinWindow = 60
)

// startBackend 按 main.go 的装配方式把后端各模块接到节点上，并起一个 HTTP 服务
func (w *world) startBackend(t *testing.T, ctx context.Context) {
	client, err := chain.Dial(ctx, w.n.url, w.factory.Hex())
	if err != nil {
		t.Fatalf("chain.Dial: %v", err)
	}
	w.client = client
	w.state = indexer.NewPoolStateManager()

	oracleInst := oracle.NewOracle(oracle.DefaultOracleConfig())
	gasEstimator := gas.NewEstimator(gas.DefaultConfig(), client)
	klines := kline.NewAggregator(kline.DefaultConfig())
	guard := mev.NewGuard(mev.DefaultConfig())

	arbConfig := arb.DefaultConfig()
	arbConfig.FlashArbAddress = lower(w.flashArb)
	arbConfig.WETH = lower(w.tokenA) // 把 A 当作 WETH，gas 成本可以折算成 A 或 B
	scanner := arb.NewScanner(arbConfig, w.state, gasEstimator)
	scanner.Balances = client

	hub := ws.NewHub()
	go hub.Run(ctx)
	klines.OnTrade = func(trade kline.Trade, candle kline.Candle) {
		hub.Broadcast("trade", trade)
		hub.Broadcast("kline", api.KlineUpdate{Pool: trade.Pool, Interval: "1m", Candle: candle})
	}
	guard.OnAlert = func(alert mev.Alert) { hub.Broadcast("mev_alert", alert) }
	scanner.OnOpportunity = func(opp arb.Opportunity) { hub.Broadcast("arbitrage", opp) }

	cfg := indexer.DefaultConfig()
	cfg.PollInterval = 100 * time.Millisecond
	cfg.ConfirmationBlocks = 3
	w.idx = indexer.NewIndexer(cfg, w.state, client)
	w.idx.Subscribe(oracleInst.PoolPriceFeed(w.state))
	w.idx.Subscribe(klines)
	w.idx.Subscribe(guard)
	w.idx.Subscribe(scanner)
	w.router = router.NewRouter(router.DefaultRouterConfig(), w.state)

	w.verifier = eip712.NewVerifier(eip712.DomainSeparator{
		Name: "UniGo-DEX", Version: "1", ChainID: chainID.Uint64(), VerifyingContract: w.manager.Hex(),
	}, 5*time.Minute)

	serverConfig := api.DefaultServerConfig()
	serverConfig.ManagerAddress = w.manager.Hex()
	serverConfig.QuoterAddress = w.quoter.Hex()
	serverConfig.TWAPWindow = twapWindow
	serverConfig.TWAPMinWindow = twapMinWindow
	w.server = httptest.NewServer(api.NewServer(serverConfig, api.Deps{
		Router: w.router, State: w.state, Indexer: w.idx, Cache: cache.NewCache(30 * time.Second),
		Oracle: oracleInst, Chain: client, Gas: gasEstimator, Klines: klines, Guard: guard,
		Arb: scanner, Hub: hub, Verifier: w.verifier,
		// 读数只缓存 10 毫秒，测试里改完喂价马上就能读到
		Chainlink: oracle.NewChainlinkReader(client, []oracle.FeedConfig{{
			Base: lower(w.tokenA), Quote: lower(w.tokenB), Address: w.feedAB.Hex(),
			BaseDecimals: 18, QuoteDecimals: 18, Heartbeat: time.Hour,
		}}, 10*time.Millisecond),
	}).Handler())
	t.Cleanup(w.server.Close)

	// 订阅 SSE 推送，后面断言成交 / K 线 / 告警 / 套利机会都推出来了
	w.events = subscribeEvents(t, ctx, w.server.URL+"/api/v1/stream")
	waitFor(t, "SSE 订阅建立", func() bool { return hub.ClientCount() == 1 })

	if err := w.idx.Start(ctx); err != nil {
		t.Fatal(err)
	}
}

// ===== HTTP 与 SSE 辅助 =====

// eventLog 收集 SSE 推送的事件
type eventLog struct {
	mu     sync.Mutex
	events []sseEvent
}

type sseEvent struct {
	kind string
	data string
}

func subscribeEvents(t *testing.T, ctx context.Context, url string) *eventLog {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("订阅 SSE 失败: %d", resp.StatusCode)
	}
	log := &eventLog{}
	go func() {
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		var kind string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				log.mu.Lock()
				log.events = append(log.events, sseEvent{kind, strings.TrimPrefix(line, "data: ")})
				log.mu.Unlock()
			}
		}
	}()
	return log
}

// has 是否收到过类型为 kind、内容包含 substr 的推送
func (l *eventLog) has(kind, substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.kind == kind && strings.Contains(e.data, substr) {
			return true
		}
	}
	return false
}

// get 发 GET 请求并解析 JSON，返回状态码
func (w *world) get(t *testing.T, path string, out interface{}) int {
	t.Helper()
	resp, err := http.Get(w.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return decodeResponse(t, resp, out)
}

// post 发 POST 请求并解析 JSON，返回状态码
func (w *world) post(t *testing.T, path string, body, out interface{}) int {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(w.server.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return decodeResponse(t, resp, out)
}

func decodeResponse(t *testing.T, resp *http.Response, out interface{}) int {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("解析响应失败（状态码 %d）: %s", resp.StatusCode, raw)
		}
	}
	return resp.StatusCode
}

// signBuilt 用 from 的私钥给后端构造的待签名交易签名，返回 raw_tx
func (w *world) signBuilt(t *testing.T, from account, built api.BuildSwapResponse) (string, common.Hash) {
	t.Helper()
	nonce, err := w.n.eth.PendingNonceAt(context.Background(), from.addr)
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress(built.To)
	tip, _ := new(big.Int).SetString(built.Gas.PriorityFee, 10)
	maxFee, _ := new(big.Int).SetString(built.Gas.MaxFeePerGas, 10)
	tx, err := types.SignNewTx(from.key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID:   new(big.Int).SetUint64(built.ChainID),
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: maxFee,
		Gas:       built.GasLimit,
		To:        &to,
		Data:      common.FromHex(built.Data),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return hexutil.Encode(raw), tx.Hash()
}

// signedSwapRequest 构造带 EIP-712 签名的提交请求
func (w *world) signedSwapRequest(t *testing.T, from account, built api.BuildSwapResponse, tokenIn, tokenOut common.Address, rawTx string) api.SwapRequest {
	t.Helper()
	var nonce struct {
		Nonce uint64 `json:"nonce"`
	}
	if code := w.get(t, "/api/v1/nonce/"+from.addr.Hex(), &nonce); code != http.StatusOK {
		t.Fatalf("查询 nonce: %d", code)
	}
	req := api.SwapRequest{
		Owner: from.addr.Hex(), TokenIn: tokenIn.Hex(), TokenOut: tokenOut.Hex(),
		AmountIn: built.AmountIn, MinAmountOut: built.MinAmountOut,
		Slippage: built.Slippage, Nonce: nonce.Nonce, Deadline: built.Deadline, RawTx: rawTx,
	}
	hash, err := w.verifier.Hash(&eip712.SwapPermit{
		Owner: req.Owner, TokenIn: req.TokenIn, TokenOut: req.TokenOut, AmountIn: req.AmountIn,
		MinAmountOut: req.MinAmountOut, Nonce: req.Nonce, Deadline: req.Deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := eip712.SignPermit(from.key, hash)
	if err != nil {
		t.Fatal(err)
	}
	req.Signature = sig.Hex()
	return req
}

// sendRaw 直接把已签名交易发给节点并等它成功
func (w *world) sendRaw(t *testing.T, rawTx string, hash common.Hash) *types.Receipt {
	t.Helper()
	if err := w.n.rpc.Call(nil, "eth_sendRawTransaction", rawTx); err != nil {
		t.Fatalf("广播失败: %v", err)
	}
	receipt := w.n.waitReceipt(t, hash)
	if receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("交易 %s 执行失败", hash.Hex())
	}
	return receipt
}

func (w *world) price(t *testing.T, pool common.Address) float64 {
	t.Helper()
	slot0, err := w.client.Slot0(context.Background(), pool.Hex())
	if err != nil {
		t.Fatal(err)
	}
	return umath.SqrtPriceX96ToPrice(slot0.SqrtPriceX96)
}

func (w *world) tick(t *testing.T, pool common.Address) int32 {
	t.Helper()
	slot0, err := w.client.Slot0(context.Background(), pool.Hex())
	if err != nil {
		t.Fatal(err)
	}
	return slot0.Tick
}

func dec(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("not a decimal: " + s)
	}
	return n
}

// synced 等索引器追到链头
func (w *world) synced(t *testing.T) {
	t.Helper()
	head, err := w.n.eth.BlockNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, fmt.Sprintf("索引器追到区块 %d", head), func() bool {
		return w.idx.GetStats().LastBlockNumber >= head
	})
}

func (w *world) pools() map[string]common.Address {
	return map[string]common.Address{
		"A/B 0.3%": w.poolAB3000, "A/B 0.05%": w.poolAB500, "B/C 0.3%": w.poolBC3000, "A/C 1%": w.poolAC10000,
	}
}

// assertStateMatchesChain 链下重建的池子状态必须与链上 slot0 / liquidity 完全一致
func (w *world) assertStateMatchesChain(t *testing.T) {
	t.Helper()
	w.synced(t)
	ctx := context.Background()
	for name, addr := range w.pools() {
		pool, ok := w.state.GetPool(lower(addr))
		if !ok {
			t.Fatalf("%s: 索引器没有发现这个池子", name)
		}
		snap := pool.Snapshot()
		slot0, err := w.client.Slot0(ctx, addr.Hex())
		if err != nil {
			t.Fatal(err)
		}
		liquidity, err := w.client.PoolLiquidity(ctx, addr.Hex())
		if err != nil {
			t.Fatal(err)
		}
		if snap.SqrtPriceX96.Cmp(slot0.SqrtPriceX96) != 0 || snap.Tick != slot0.Tick || snap.Liquidity.Cmp(liquidity) != 0 {
			t.Errorf("%s 链下状态与链上不一致:\n  链下 sqrtPrice=%s tick=%d liquidity=%s\n  链上 sqrtPrice=%s tick=%d liquidity=%s",
				name, snap.SqrtPriceX96, snap.Tick, snap.Liquidity, slot0.SqrtPriceX96, slot0.Tick, liquidity)
		}
		// 每个已初始化 tick 的 liquidityNet 也要对上
		for _, tick := range snap.Ticks {
			onChain := w.n.read(t, addr, w.poolABI.ABI, "ticks", big.NewInt(int64(tick.Tick)))
			if net := onChain[1].(*big.Int); net.Cmp(tick.LiquidityNet) != 0 || !onChain[4].(bool) {
				t.Errorf("%s tick %d: 链下 liquidityNet=%s，链上=%s initialized=%v", name, tick.Tick, tick.LiquidityNet, net, onChain[4])
			}
		}
	}
}

// quoteCase 一条报价对比用例：按给定路径投入 amountIn
type quoteCase struct {
	name     string
	tokens   []common.Address
	fees     []uint32
	amountIn *big.Int
}

func (w *world) quoteCases() []quoteCase {
	big1, _ := new(big.Int).SetString("3000000000000000000000", 10)  // 3e21：推过窄区间
	big2, _ := new(big.Int).SetString("60000000000000000000000", 10) // 6e22：连续穿越多个 tick
	var cases []quoteCase
	single := []struct {
		name   string
		t0, t1 common.Address
		fee    uint32
	}{
		{"A/B 0.3%", w.tokenA, w.tokenB, 3000},
		{"A/B 0.05%", w.tokenA, w.tokenB, 500},
		{"B/C 0.3%", w.tokenB, w.tokenC, 3000},
		{"A/C 1%", w.tokenA, w.tokenC, 10000},
	}
	for _, p := range single {
		for _, amount := range []*big.Int{big.NewInt(1), big.NewInt(1000), big.NewInt(1e15), e18(1), big1, big2} {
			cases = append(cases,
				quoteCase{fmt.Sprintf("%s 0→1 %s", p.name, amount), []common.Address{p.t0, p.t1}, []uint32{p.fee}, amount},
				quoteCase{fmt.Sprintf("%s 1→0 %s", p.name, amount), []common.Address{p.t1, p.t0}, []uint32{p.fee}, amount},
			)
		}
	}
	for _, amount := range []*big.Int{big.NewInt(1e15), e18(1), big1, big2} {
		cases = append(cases,
			quoteCase{fmt.Sprintf("A→B→C %s", amount), []common.Address{w.tokenA, w.tokenB, w.tokenC}, []uint32{3000, 3000}, amount},
			quoteCase{fmt.Sprintf("C→B→A %s", amount), []common.Address{w.tokenC, w.tokenB, w.tokenA}, []uint32{3000, 500}, amount},
			quoteCase{fmt.Sprintf("A→C→B→A %s", amount), []common.Address{w.tokenA, w.tokenC, w.tokenB, w.tokenA}, []uint32{10000, 3000, 500}, amount},
		)
	}
	return cases
}

// offchainQuote 用链下状态逐池模拟一条路径
func (w *world) offchainQuote(c quoteCase) (*big.Int, int, error) {
	amount := c.amountIn
	crossed := 0
	for i, fee := range c.fees {
		poolAddr := w.poolFor(c.tokens[i], c.tokens[i+1], fee)
		pool, ok := w.state.GetPool(lower(poolAddr))
		if !ok {
			return nil, 0, fmt.Errorf("pool not indexed")
		}
		out, result, err := router.QuoteExactInputSingle(pool.Snapshot(), lower(c.tokens[i]), amount)
		if err != nil {
			return nil, 0, err
		}
		amount = out
		crossed += result.TicksCrossed
	}
	return amount, crossed, nil
}

func (w *world) poolFor(a, b common.Address, fee uint32) common.Address {
	has := func(x common.Address) bool { return x == a || x == b }
	switch {
	case has(w.tokenA) && has(w.tokenB) && fee == 3000:
		return w.poolAB3000
	case has(w.tokenA) && has(w.tokenB) && fee == 500:
		return w.poolAB500
	case has(w.tokenB) && has(w.tokenC):
		return w.poolBC3000
	default:
		return w.poolAC10000
	}
}

// assertQuotesMatchQuoter 链下报价与链上 Quoter 逐位一致；返回对比的用例数和其中穿越过 tick 的用例数
func (w *world) assertQuotesMatchQuoter(t *testing.T, stage string) {
	t.Helper()
	w.synced(t)
	ctx := context.Background()
	total, withCrossing, maxCrossed, bothFail := 0, 0, 0, 0
	for _, c := range w.quoteCases() {
		tokens := make([]string, len(c.tokens))
		for i, token := range c.tokens {
			tokens[i] = token.Hex()
		}
		path, err := chain.EncodePath(tokens, c.fees)
		if err != nil {
			t.Fatal(err)
		}
		onChain, onChainErr := w.client.QuoteExactInput(ctx, w.quoter.Hex(), path, c.amountIn)
		offChain, crossed, offChainErr := w.offchainQuote(c)

		switch {
		case onChainErr != nil && offChainErr != nil:
			// 两边都报不出价（链上 revert、链下报错）也算一致，单独计数
			bothFail++
		case onChainErr != nil || offChainErr != nil:
			t.Errorf("[%s] %s: 链上 err=%v，链下 err=%v", stage, c.name, onChainErr, offChainErr)
		case onChain.Cmp(offChain) != 0:
			t.Errorf("[%s] %s: 链上 %s，链下 %s，相差 %s", stage, c.name, onChain, offChain, new(big.Int).Sub(onChain, offChain))
		}
		total++
		if crossed > 0 {
			withCrossing++
		}
		if crossed > maxCrossed {
			maxCrossed = crossed
		}
	}
	t.Logf("[%s] 对比 %d 条报价：%d 条两边数值逐位一致（其中 %d 条穿越了已初始化的 tick，单条最多 %d 个），%d 条两边都无法成交",
		stage, total, total-bothFail, withCrossing, maxCrossed, bothFail)
}

func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := deployWorld(t)
	w.startBackend(t, ctx)
	deadline := func() int64 { return time.Now().Add(25 * time.Minute).Unix() }

	t.Run("索引器状态与链上一致", func(t *testing.T) {
		w.assertStateMatchesChain(t)
	})

	t.Run("链下报价与链上Quoter一致", func(t *testing.T) {
		w.assertQuotesMatchQuoter(t, "初始状态")

		// 做几笔真实的 swap 把各池子的价格推离初始位置（包括穿越 tick），再比一轮
		w.swapOnChain(t, w.trader, w.poolAB3000, true, e18(4000))
		w.swapOnChain(t, w.trader, w.poolAB500, false, e18(900))
		w.swapOnChain(t, w.trader, w.poolBC3000, false, e18(2500))
		w.swapOnChain(t, w.trader, w.poolAC10000, true, e18(700))
		w.assertStateMatchesChain(t)
		w.assertQuotesMatchQuoter(t, "若干笔 swap 之后")
	})

	// 多跳成交的那笔交易，后面的 K 线 / 成交断言要用
	var multiHopTx common.Hash

	t.Run("多跳交易构造签名广播并确认", func(t *testing.T) {
		w.synced(t)
		amountIn := e18(5)

		// 1. 后端构造待签名交易：A → C，最优路径应是 A→B→C（0.05% + 0.3%），而不是 1% 的直连池
		var built api.BuildSwapResponse
		code := w.post(t, "/api/v1/swap/build", api.BuildSwapRequest{
			TokenIn: w.tokenA.Hex(), TokenOut: w.tokenC.Hex(), AmountIn: amountIn.String(),
			Slippage: 0.005, Deadline: deadline(), Recipient: w.trader.addr.Hex(), From: w.trader.addr.Hex(),
		}, &built)
		if code != http.StatusOK {
			t.Fatalf("构造交易: %d %+v", code, built)
		}
		if built.Method != "exactInput" || len(built.Path) != 3 || built.Path[1] != lower(w.tokenB) {
			t.Fatalf("应该走两跳 A→B→C: method=%s path=%v", built.Method, built.Path)
		}
		if built.To != lower(w.manager) || built.ChainID != 31337 || built.GasLimitSource != "node" {
			t.Fatalf("交易信封: to=%s chain=%d gas来源=%s", built.To, built.ChainID, built.GasLimitSource)
		}
		// 链上 Quoter 抽检：与链下报价逐位一致
		if built.QuoteConsistent == nil || !*built.QuoteConsistent || built.OnchainAmountOut != built.AmountOut {
			t.Fatalf("链上抽检: 链下 %s，链上 %s", built.AmountOut, built.OnchainAmountOut)
		}
		// 这些池子的观测容量还是 1，且刚发生过交易：TWAP 不可用，必须明确提示而不是当作正常
		if !strings.Contains(built.Warning, "TWAP 不可用") {
			t.Fatalf("观测不足时应提示 TWAP 不可用，实际 warning=%q", built.Warning)
		}
		if built.MinAmountOut != mev.MinAmountOut(dec(built.AmountOut), built.Slippage).String() {
			t.Fatalf("min_amount_out=%s 与滑点 %v 不符", built.MinAmountOut, built.Slippage)
		}

		// 2. 用户钱包签名交易，再对请求做 EIP-712 签名，交给后端广播
		rawTx, txHash := w.signBuilt(t, w.trader, built)
		req := w.signedSwapRequest(t, w.trader, built, w.tokenA, w.tokenC, rawTx)
		balanceBefore := w.balance(t, w.tokenC, w.trader.addr)

		var swapResp api.SwapResponse
		if code := w.post(t, "/api/v1/swap", req, &swapResp); code != http.StatusOK {
			t.Fatalf("提交交易: %d %+v", code, swapResp)
		}
		if swapResp.TxHash != txHash.Hex() || swapResp.Status != chain.TxPending {
			t.Fatalf("提交结果: %+v，期望哈希 %s", swapResp, txHash.Hex())
		}
		multiHopTx = txHash

		// 同一个签名重放：nonce 已消费，拒绝
		var replay api.ErrorResponse
		if code := w.post(t, "/api/v1/swap", req, &replay); code != http.StatusUnauthorized || !strings.Contains(replay.Error, "nonce") {
			t.Fatalf("重放应被拒绝: %d %q", code, replay.Error)
		}
		// 别人签名的交易冒充 owner：拒绝
		botRaw, _ := w.signBuilt(t, w.bot, built)
		forged := w.signedSwapRequest(t, w.trader, built, w.tokenA, w.tokenC, botRaw)
		if code := w.post(t, "/api/v1/swap", forged, &replay); code != http.StatusForbidden || !strings.Contains(replay.Error, "not signed by owner") {
			t.Fatalf("raw_tx 签名者与 owner 不一致应被拒绝: %d %q", code, replay.Error)
		}

		// 3. 交易状态流转：已打包但确认数不足 → 再出两个区块后最终确认（阈值 3）
		var status chain.TxStatus
		w.n.waitReceipt(t, txHash)
		if code := w.get(t, "/api/v1/tx/"+txHash.Hex(), &status); code != http.StatusOK {
			t.Fatalf("查询交易状态: %d", code)
		}
		if status.Status != chain.TxPendingConfirmation || status.Confirmations != 1 || status.RequiredConfirmations != 3 {
			t.Fatalf("刚上链时: %+v", status)
		}
		w.n.mine(t)
		w.get(t, "/api/v1/tx/"+txHash.Hex(), &status)
		if status.Status != chain.TxPendingConfirmation || status.Confirmations != 2 {
			t.Fatalf("2 个确认时: %+v", status)
		}
		w.n.mine(t)
		w.get(t, "/api/v1/tx/"+txHash.Hex(), &status)
		if status.Status != chain.TxConfirmed || status.Confirmations != 3 || status.GasUsed == 0 {
			t.Fatalf("3 个确认时: %+v", status)
		}
		if code := w.get(t, "/api/v1/tx/"+common.HexToHash("0xdead").Hex(), nil); code != http.StatusNotFound {
			t.Fatalf("不存在的交易应返回 404，实际 %d", code)
		}

		// 4. 到手数量与报价逐位一致
		received := new(big.Int).Sub(w.balance(t, w.tokenC, w.trader.addr), balanceBefore)
		if received.Cmp(dec(built.AmountOut)) != 0 {
			t.Fatalf("到手 %s，报价 %s", received, built.AmountOut)
		}
		t.Logf("多跳 A→B→C：投入 %s，到手 %s，与链下报价逐位一致；gas 实际 %d / 估算上限 %d",
			built.AmountIn, received, status.GasUsed, built.GasLimit)
	})

	t.Run("单跳swap与exactOutputSingle", func(t *testing.T) {
		w.synced(t)

		// 单跳精确输入 → Manager.swap
		var built api.BuildSwapResponse
		if code := w.post(t, "/api/v1/swap/build", api.BuildSwapRequest{
			TokenIn: w.tokenB.Hex(), TokenOut: w.tokenC.Hex(), AmountIn: e18(2).String(),
			Deadline: deadline(), Recipient: w.trader.addr.Hex(), From: w.trader.addr.Hex(),
		}, &built); code != http.StatusOK || built.Method != "swap" {
			t.Fatalf("构造单跳交易: %d %+v", code, built)
		}
		before := w.balance(t, w.tokenC, w.trader.addr)
		raw, hash := w.signBuilt(t, w.trader, built)
		w.sendRaw(t, raw, hash)
		if got := new(big.Int).Sub(w.balance(t, w.tokenC, w.trader.addr), before); got.Cmp(dec(built.AmountOut)) != 0 {
			t.Fatalf("swap 到手 %s，报价 %s", got, built.AmountOut)
		}

		// 精确输出 → Manager.exactOutputSingle：拿到的数量分毫不差，付出的数量与链下算的一致
		w.synced(t)
		wantOut := e18(3)
		if code := w.post(t, "/api/v1/swap/build", api.BuildSwapRequest{
			TokenIn: w.tokenA.Hex(), TokenOut: w.tokenB.Hex(), AmountOut: wantOut.String(),
			Deadline: deadline(), Recipient: w.trader.addr.Hex(), From: w.trader.addr.Hex(),
		}, &built); code != http.StatusOK || built.Method != "exactOutputSingle" {
			t.Fatalf("构造精确输出交易: %d %+v", code, built)
		}
		beforeA, beforeB := w.balance(t, w.tokenA, w.trader.addr), w.balance(t, w.tokenB, w.trader.addr)
		raw, hash = w.signBuilt(t, w.trader, built)
		w.sendRaw(t, raw, hash)
		gotB := new(big.Int).Sub(w.balance(t, w.tokenB, w.trader.addr), beforeB)
		paidA := new(big.Int).Sub(beforeA, w.balance(t, w.tokenA, w.trader.addr))
		if gotB.Cmp(wantOut) != 0 || paidA.Cmp(dec(built.AmountIn)) != 0 {
			t.Fatalf("exactOutputSingle: 到手 %s（要 %s），付出 %s（链下算 %s）", gotB, wantOut, paidA, built.AmountIn)
		}
		if dec(built.MaxAmountIn).Cmp(paidA) < 0 {
			t.Fatalf("max_amount_in=%s 小于实际付出 %s", built.MaxAmountIn, paidA)
		}
	})

	t.Run("K线与成交记录", func(t *testing.T) {
		w.synced(t)

		// 多跳那笔交易在 B/C 池留下一笔成交（第二跳）
		var trades []kline.Trade
		if code := w.get(t, "/api/v1/trades?pool="+w.poolBC3000.Hex()+"&limit=1000", &trades); code != http.StatusOK {
			t.Fatalf("查询成交: %d", code)
		}
		var hop *kline.Trade
		for i := range trades {
			if trades[i].TxHash == multiHopTx.Hex() {
				hop = &trades[i]
			}
		}
		if hop == nil {
			t.Fatalf("B/C 池的成交记录里没有多跳交易 %s", multiHopTx.Hex())
		}
		// B 进池（amount0 > 0），C 出池
		if !hop.ZeroForOne || dec(hop.Amount0).Sign() <= 0 || dec(hop.Amount1).Sign() >= 0 || hop.Recipient != lower(w.trader.addr) {
			t.Fatalf("多跳第二跳的成交: %+v", hop)
		}

		// 最新一笔成交的价格就是链上现价；1m K 线的收盘价也是它
		if trades[0].Price != w.price(t, w.poolBC3000) {
			t.Fatalf("最新成交价 %v，链上现价 %v", trades[0].Price, w.price(t, w.poolBC3000))
		}
		var candles []kline.Candle
		if code := w.get(t, "/api/v1/klines?pool="+w.poolBC3000.Hex()+"&interval=1m&limit=1000", &candles); code != http.StatusOK || len(candles) == 0 {
			t.Fatalf("查询 K 线: %d，%d 根", code, len(candles))
		}
		if last := candles[len(candles)-1]; last.Close != trades[0].Price || last.High < last.Low || last.OpenTime%60 != 0 {
			t.Fatalf("最新 1m K 线: %+v", last)
		}
		// K 线的成交笔数和成交量之和等于逐笔成交的合计（1h 周期同理）
		for _, interval := range []string{"1m", "1h"} {
			w.get(t, "/api/v1/klines?pool="+w.poolBC3000.Hex()+"&interval="+interval+"&limit=1000", &candles)
			count, volume0 := 0, new(big.Int)
			for _, c := range candles {
				count += c.Trades
				volume0.Add(volume0, dec(c.Volume0))
			}
			wantVolume0 := new(big.Int)
			for _, tr := range trades {
				wantVolume0.Add(wantVolume0, new(big.Int).Abs(dec(tr.Amount0)))
			}
			// 成交量是定点整数，合计必须一位不差
			if count != len(trades) || volume0.Cmp(wantVolume0) != 0 {
				t.Fatalf("%s K 线合计 %d 笔 / volume0=%s，逐笔成交 %d 笔 / %s", interval, count, volume0, len(trades), wantVolume0)
			}
		}

		// 实时推送也收到了这笔成交和对应的 K 线更新
		waitFor(t, "SSE 推送成交和 K 线", func() bool {
			return w.events.has("trade", multiHopTx.Hex()) && w.events.has("kline", lower(w.poolBC3000))
		})
	})

	t.Run("TWAP读数与手算一致及降级", func(t *testing.T) {
		ctx := context.Background()
		pool := w.poolAC10000

		// 扩大观测容量，否则环形数组只有 1 格，新观测会覆盖旧的
		w.n.invoke(t, w.lp, pool, w.poolABI.ABI, "increaseObservationCardinalityNext", uint16(10))

		// 在精确的时间点做两笔 swap：t0 时 tick 变成 tick1，t0+100 时变成 tick2，再过 50 秒出一个空块
		t0 := w.n.latestTimestamp(t) + 10
		w.n.setNextTimestamp(t, t0)
		w.swapOnChain(t, w.trader, pool, true, e18(900))
		tick1 := w.tick(t, pool)
		w.n.setNextTimestamp(t, t0+100)
		w.swapOnChain(t, w.trader, pool, false, e18(2500))
		tick2 := w.tick(t, pool)
		w.n.setNextTimestamp(t, t0+150)
		w.n.mine(t)
		if tick1 == tick2 {
			t.Fatalf("两笔 swap 之后 tick 没变（%d），测不出时间加权", tick1)
		}

		// 手算：最近 150 秒里 tick1 持续了 100 秒、tick2 持续了 50 秒
		sum := int64(tick1)*100 + int64(tick2)*50
		want := sum / 150
		if sum < 0 && sum%150 != 0 {
			want-- // 向负无穷取整
		}
		twap, err := w.client.TWAP(ctx, pool.Hex(), 150, twapMinWindow)
		if err != nil {
			t.Fatal(err)
		}
		if int64(twap.Tick) != want || twap.Window != 150 || twap.Degraded {
			t.Fatalf("TWAP tick=%d window=%d degraded=%v，手算 %d（tick1=%d×100s，tick2=%d×50s）",
				twap.Tick, twap.Window, twap.Degraded, want, tick1, tick2)
		}
		t.Logf("TWAP：tick1=%d 持续 100s，tick2=%d 持续 50s，手算平均 tick=%d，链上读数 %d", tick1, tick2, want, twap.Tick)

		// API 返回同样的读数；现价与 TWAP 很接近，状态正常
		w.synced(t)
		var reading api.PoolPriceCheck
		if code := w.get(t, "/api/v1/twap?pool="+pool.Hex(), &reading); code != http.StatusOK {
			t.Fatalf("查询 TWAP: %d", code)
		}
		if int64(reading.Tick) != want || reading.Status != oracle.TWAPOK || reading.Degraded || reading.Spot != w.price(t, pool) {
			t.Fatalf("API 的 TWAP 读数: %+v", reading)
		}

		// 降级一：B/C 池观测容量是 1。让它在 80 秒前刚换过 tick，150 秒的窗口覆盖不了，
		// 退到能覆盖的最长窗口（80 秒），并标记为降级
		bc := w.poolBC3000
		t1 := w.n.latestTimestamp(t) + 5
		w.n.setNextTimestamp(t, t1)
		w.swapOnChain(t, w.trader, bc, true, e18(800))
		w.n.setNextTimestamp(t, t1+80)
		w.n.mine(t)
		twap, err = w.client.TWAP(ctx, bc.Hex(), 150, twapMinWindow)
		if err != nil {
			t.Fatal(err)
		}
		if !twap.Degraded || twap.Window != 80 || twap.Tick != w.tick(t, bc) {
			t.Fatalf("降级后的 TWAP: %+v，池子当前 tick=%d", twap, w.tick(t, bc))
		}

		// 降级二：刚换过 tick（观测只覆盖 1 秒），连最短窗口都不够，明确报不可用
		w.swapOnChain(t, w.trader, bc, false, e18(800))
		if _, err := w.client.TWAP(ctx, bc.Hex(), 150, twapMinWindow); err == nil || !strings.Contains(err.Error(), "twap unavailable") {
			t.Fatalf("观测不足时应返回 ErrTWAPUnavailable，实际 %v", err)
		}
		w.synced(t)
		if code := w.get(t, "/api/v1/twap?pool="+bc.Hex(), &reading); code != http.StatusOK || reading.Status != oracle.TWAPUnavailable {
			t.Fatalf("API 应报告 TWAP 不可用: %d %+v", code, reading)
		}
	})

	t.Run("现价偏离TWAP时拒绝交易", func(t *testing.T) {
		pool := w.poolAC10000
		before := w.price(t, pool)

		// 单个区块内用一笔大单把 A/C 池的价格砸下去（A 变便宜）
		dump := e18(120000)
		cBefore := w.balance(t, w.tokenC, w.bot.addr)
		w.swapOnChain(t, w.bot, pool, true, dump)
		cGot := new(big.Int).Sub(w.balance(t, w.tokenC, w.bot.addr), cBefore)
		after := w.price(t, pool)
		if (before-after)/before < 0.06 {
			t.Fatalf("价格只动了 %.2f%%，不足以触发操纵检测", (before-after)/before*100)
		}
		w.synced(t)

		// 用 C 换 A 现在"特别划算"，路由会选这个被操纵的池子；交易前检查必须拒绝
		var rejected api.ErrorResponse
		code := w.post(t, "/api/v1/swap/build", api.BuildSwapRequest{
			TokenIn: w.tokenC.Hex(), TokenOut: w.tokenA.Hex(), AmountIn: e18(10).String(),
			Slippage: 0.01, Deadline: deadline(), Recipient: w.trader.addr.Hex(),
		}, &rejected)
		if code != http.StatusForbidden || !strings.Contains(rejected.Error, "疑似被单区块操纵") {
			t.Fatalf("现价偏离 TWAP 时应拒绝: %d %q", code, rejected.Error)
		}
		// 报价接口不拒绝，但给出警告
		var quote api.QuoteResponse
		path := fmt.Sprintf("/api/v1/quote?token_in=%s&token_out=%s&amount_in=%s", w.tokenC.Hex(), w.tokenA.Hex(), e18(10))
		if code := w.get(t, path, &quote); code != http.StatusOK || !strings.Contains(quote.Warning, "疑似被单区块操纵") {
			t.Fatalf("报价应带操纵警告: %d %q", code, quote.Warning)
		}
		var reading api.PoolPriceCheck
		w.get(t, "/api/v1/twap?pool="+pool.Hex(), &reading)
		if reading.Status != oracle.TWAPDeviated || reading.Deviation < 0.05 {
			t.Fatalf("TWAP 读数应为偏离: %+v", reading)
		}
		t.Logf("操纵检测：现价 %.6f，%d 秒 TWAP %.6f，偏离 %.2f%%，交易被拒绝", reading.Spot, reading.Window, reading.TWAP, reading.Deviation*100)

		// 把价格买回去，偏离消失后交易恢复
		w.swapOnChain(t, w.bot, pool, false, cGot)
		w.synced(t)
		w.get(t, "/api/v1/twap?pool="+pool.Hex(), &reading)
		if reading.Status != oracle.TWAPOK {
			t.Fatalf("价格恢复后 TWAP 校验应通过: %+v", reading)
		}
		var built api.BuildSwapResponse
		if code := w.post(t, "/api/v1/swap/build", api.BuildSwapRequest{
			TokenIn: w.tokenC.Hex(), TokenOut: w.tokenA.Hex(), AmountIn: e18(10).String(),
			Slippage: 0.01, Deadline: deadline(), Recipient: w.trader.addr.Hex(),
		}, &built); code != http.StatusOK {
			t.Fatalf("价格恢复后应能构造交易: %d %+v", code, built)
		}
	})

	t.Run("三明治攻击检测", func(t *testing.T) {
		w.synced(t)
		pool := w.poolAB500

		// 关掉自动出块，把三笔交易放进同一个区块；小费从高到低决定区块内顺序：
		// 攻击者买入 → 受害者同向买入 → 攻击者反向卖出
		w.n.evm(t, "evm_setAutomine", false)
		swapTx := func(from account, zeroForOne bool, amount *big.Int, tipGwei int64) *types.Transaction {
			data, err := chain.EncodeSwap(pool.Hex(), zeroForOne, amount, from.addr.Hex(), big.NewInt(0), farFuture.Int64())
			if err != nil {
				t.Fatal(err)
			}
			tx := w.n.signTx(t, from, &w.manager, data, tipGwei)
			if err := w.n.eth.SendTransaction(context.Background(), tx); err != nil {
				t.Fatal(err)
			}
			return tx
		}
		front := swapTx(w.bot, true, e18(3000), 30)
		victimTx := swapTx(w.victim, true, e18(500), 20)
		back := swapTx(w.bot, false, e18(2900), 10)
		w.n.mine(t)
		w.n.evm(t, "evm_setAutomine", true)

		receipts := []*types.Receipt{w.n.waitReceipt(t, front.Hash()), w.n.waitReceipt(t, victimTx.Hash()), w.n.waitReceipt(t, back.Hash())}
		for i, r := range receipts {
			if r.Status != types.ReceiptStatusSuccessful || r.BlockNumber.Cmp(receipts[0].BlockNumber) != 0 || r.TransactionIndex != uint(i) {
				t.Fatalf("第 %d 笔: status=%d block=%s index=%d（三笔应在同一区块且按顺序）", i, r.Status, r.BlockNumber, r.TransactionIndex)
			}
		}
		w.synced(t)

		var alerts []mev.Alert
		if code := w.get(t, "/api/v1/mev/alerts", &alerts); code != http.StatusOK || len(alerts) != 1 {
			t.Fatalf("应有 1 条三明治告警: %d %+v", code, alerts)
		}
		a := alerts[0]
		if a.Pool != lower(pool) || a.Attacker != lower(w.bot.addr) || a.AttackerSender != lower(w.manager) ||
			a.FrontTx != front.Hash().Hex() || a.BackTx != back.Hash().Hex() ||
			len(a.VictimTxs) != 1 || a.VictimTxs[0] != victimTx.Hash().Hex() || a.BlockNumber != receipts[0].BlockNumber.Uint64() {
			t.Fatalf("告警内容: %+v", a)
		}
		waitFor(t, "SSE 推送 MEV 告警", func() bool { return w.events.has("mev_alert", back.Hash().Hex()) })

		// 被夹过的池子：报价给出提示并把推荐滑点收紧到 0.1%
		var quote api.QuoteResponse
		path := fmt.Sprintf("/api/v1/quote?token_in=%s&token_out=%s&amount_in=%s&slippage=0.01", w.tokenA.Hex(), w.tokenB.Hex(), e18(1))
		if code := w.get(t, path, &quote); code != http.StatusOK {
			t.Fatalf("报价: %d", code)
		}
		if len(quote.Fees) != 1 || quote.Fees[0] != 500 {
			t.Skipf("路由没有走被夹的 0.05%% 池（fees=%v），跳过滑点收紧的断言", quote.Fees)
		}
		if quote.Slippage != 0.001 || !strings.Contains(quote.Warning, "三明治") {
			t.Fatalf("被夹过的池子应收紧滑点: slippage=%v warning=%q", quote.Slippage, quote.Warning)
		}
	})

	t.Run("套利机会扫描与执行", func(t *testing.T) {
		w.synced(t)

		// 在 0.3% 池里砸一笔大单，把它的 A 价格压到比 0.05% 池低 2% 以上
		w.swapOnChain(t, w.trader, w.poolAB3000, true, e18(90000))
		low, high := w.price(t, w.poolAB3000), w.price(t, w.poolAB500)
		if (high-low)/high < 0.015 {
			t.Fatalf("价差只有 %.2f%%，不足以覆盖三项手续费", (high-low)/high*100)
		}
		w.synced(t)

		var opps []arb.Opportunity
		if code := w.get(t, "/api/v1/arbitrage/opportunities", &opps); code != http.StatusOK || len(opps) == 0 {
			t.Fatalf("应发现套利机会: %d %+v", code, opps)
		}
		opp := opps[0] // 净利润最高的
		// A 在 0.05% 池更贵：借 A 就在 0.05% 池卖、0.3% 池买回；借 B 反过来
		wantSell, wantBuy, wantLender := w.poolAB500, w.poolAB3000, w.poolAC10000
		if opp.BorrowToken == lower(w.tokenB) {
			wantSell, wantBuy, wantLender = w.poolAB3000, w.poolAB500, w.poolBC3000
		}
		if opp.SellPool != lower(wantSell) || opp.BuyPool != lower(wantBuy) || opp.Lender != lower(wantLender) || opp.To != lower(w.flashArb) {
			t.Fatalf("机会的池子安排不对: %+v", opp)
		}
		if !opp.GasPriced || dec(opp.NetProfit).Sign() <= 0 ||
			new(big.Int).Sub(dec(opp.GrossProfit), dec(opp.GasCost)).Cmp(dec(opp.NetProfit)) != 0 {
			t.Fatalf("利润核算: gross=%s gas=%s net=%s priced=%v", opp.GrossProfit, opp.GasCost, opp.NetProfit, opp.GasPriced)
		}
		waitFor(t, "SSE 推送套利机会", func() bool { return w.events.has("arbitrage", opp.Calldata) })

		// 发起人零本金执行后端给出的调用数据
		borrowToken := common.HexToAddress(opp.BorrowToken)
		before := w.balance(t, borrowToken, w.bot.addr)
		receipt := w.n.send(t, w.bot, common.HexToAddress(opp.To), common.FromHex(opp.Calldata))
		profit := new(big.Int).Sub(w.balance(t, borrowToken, w.bot.addr), before)

		// 链上实际利润与链下预测逐位一致
		if profit.Sign() <= 0 || profit.Cmp(dec(opp.GrossProfit)) != 0 {
			t.Fatalf("发起人实际获利 %s，链下预测 %s", profit, opp.GrossProfit)
		}
		t.Logf("套利：借入 %s，发起人获利 %s（与链下预测逐位一致），gas %d；执行前价差 %.3f%%，执行后 %.3f%%",
			opp.Amount, profit, receipt.GasUsed, (high-low)/high*100,
			(w.price(t, w.poolAB500)-w.price(t, w.poolAB3000))/w.price(t, w.poolAB500)*100)

		// 价格被套平后，同一个机会不再出现（残余利润不到原来的 1%，说明借入量确实在最优点附近）
		w.synced(t)
		w.get(t, "/api/v1/arbitrage/opportunities", &opps)
		for _, o := range opps {
			if o.SellPool == opp.SellPool && o.BuyPool == opp.BuyPool && o.BorrowToken == opp.BorrowToken &&
				dec(o.NetProfit).Cmp(new(big.Int).Div(dec(opp.NetProfit), big.NewInt(100))) > 0 {
				t.Fatalf("套利执行后同一机会仍有 %s 的净利润（原 %s），借入量不是最优", o.NetProfit, opp.NetProfit)
			}
		}
	})

	t.Run("真实节点上的Reorg回滚", func(t *testing.T) {
		w.synced(t)
		pool := w.poolBC3000

		// 快照 → 出一个带 swap 的区块 → 回滚快照 → 换一笔不同的 swap 并多出一个区块
		var snapshot string
		if err := w.n.rpc.Call(&snapshot, "evm_snapshot"); err != nil {
			t.Fatal(err)
		}
		w.swapOnChain(t, w.trader, pool, true, e18(1234))
		w.synced(t)
		var trades []kline.Trade
		w.get(t, "/api/v1/trades?pool="+pool.Hex()+"&limit=1", &trades)
		orphaned := trades[0].TxHash
		reorgsBefore := w.idx.GetStats().ReorgsDetected

		var reverted bool
		if err := w.n.rpc.Call(&reverted, "evm_revert", snapshot); err != nil || !reverted {
			t.Fatalf("evm_revert: %v %v", reverted, err)
		}
		w.swapOnChain(t, w.trader, pool, false, e18(777))
		w.n.mine(t)

		waitFor(t, "索引器发现并处理 Reorg", func() bool { return w.idx.GetStats().ReorgsDetected > reorgsBefore })
		w.assertStateMatchesChain(t)

		// 被回滚的成交不能留在成交记录和 K 线里
		w.get(t, "/api/v1/trades?pool="+pool.Hex()+"&limit=1000", &trades)
		for _, tr := range trades {
			if tr.TxHash == orphaned {
				t.Fatalf("被回滚的交易 %s 还在成交记录里", orphaned)
			}
		}
		if trades[0].ZeroForOne || trades[0].Price != w.price(t, pool) {
			t.Fatalf("回滚后最新成交应是新链上的那笔: %+v", trades[0])
		}
		var candles []kline.Candle
		w.get(t, "/api/v1/klines?pool="+pool.Hex()+"&interval=1m&limit=1000", &candles)
		count := 0
		for _, c := range candles {
			count += c.Trades
		}
		if count != len(trades) || candles[len(candles)-1].Close != trades[0].Price {
			t.Fatalf("回滚后 K 线合计 %d 笔，成交记录 %d 笔", count, len(trades))
		}
	})

	t.Run("Chainlink喂价三方校验与降级", func(t *testing.T) {
		pool := w.poolAB500
		build := func(out interface{}) int {
			return w.post(t, "/api/v1/swap/build", api.BuildSwapRequest{
				// 用精确输出：只走 A/B 直连池。精确输入的话路由可能绕道 A→C→B，那条路上没有喂价
				TokenIn: w.tokenA.Hex(), TokenOut: w.tokenB.Hex(), AmountOut: e18(1).String(),
				Slippage: 0.01, Deadline: deadline(), Recipient: w.trader.addr.Hex(),
			}, out)
		}
		// setFeed 把喂价设成池子现价的 factor 倍（8 位小数），更新时间为当前区块
		setFeed := func(factor float64) {
			answer := big.NewInt(int64(w.price(t, pool) * factor * 1e8))
			w.n.invoke(t, w.lp, w.feedAB, w.aggregatorABI.ABI, "setAnswer", answer)
			time.Sleep(20 * time.Millisecond) // 等读数缓存过期
		}
		// checkOf 取出 A/B 池的校验明细（两个 A/B 池价格相差不到 1%，路由选哪个都可以）
		checkOf := func(checks []api.PoolPriceCheck) api.PoolPriceCheck {
			for _, c := range checks {
				if c.Pool == lower(w.poolAB500) || c.Pool == lower(w.poolAB3000) {
					return c
				}
			}
			t.Fatalf("路由没有经过 A/B 池: %+v", checks)
			return api.PoolPriceCheck{}
		}

		// 让 TWAP 也可用：池子的 tick 保持不变过了 200 秒
		w.n.setNextTimestamp(t, w.n.latestTimestamp(t)+200)
		w.n.mine(t)

		// 1. 喂价正常：现价 / TWAP / Chainlink 三方都参与校验，交易放行
		setFeed(1.0)
		w.synced(t)
		var reading api.PoolPriceCheck
		if code := w.get(t, "/api/v1/twap?pool="+pool.Hex(), &reading); code != http.StatusOK {
			t.Fatalf("查询校验明细: %d", code)
		}
		if reading.ChainlinkStatus != oracle.ChainlinkOK || reading.Status != oracle.TWAPOK ||
			strings.Join(reading.Sources, ",") != "spot,chainlink,twap" || reading.ChainlinkDeviation > 0.0001 {
			t.Fatalf("喂价正常时应是三方校验: %+v", reading)
		}
		// 喂价按 8 位小数归一化后与池子现价同口径
		if diff := (reading.Chainlink - reading.Spot) / reading.Spot; diff > 1e-7 || diff < -1e-7 {
			t.Fatalf("喂价换算后 %v，池子现价 %v", reading.Chainlink, reading.Spot)
		}
		var built api.BuildSwapResponse
		if code := build(&built); code != http.StatusOK {
			t.Fatalf("喂价正常时应能构造交易: %d %+v", code, built)
		}
		if c := checkOf(built.PriceChecks); c.ChainlinkStatus != oracle.ChainlinkOK || len(c.Sources) != 3 {
			t.Fatalf("响应里的校验明细: %+v", c)
		}

		// 2. 喂价比池价高 10%：池价偏离喂价，交易被拒
		setFeed(1.10)
		var rejected api.ErrorResponse
		if code := build(&rejected); code != http.StatusForbidden || !strings.Contains(rejected.Error, "偏离 Chainlink 喂价") {
			t.Fatalf("池价偏离喂价时应拒绝: %d %q", code, rejected.Error)
		}
		w.get(t, "/api/v1/twap?pool="+pool.Hex(), &reading)
		if reading.ChainlinkStatus != oracle.ChainlinkDeviated || reading.ChainlinkDeviation < 0.08 || reading.Status != oracle.TWAPOK {
			t.Fatalf("校验明细应为喂价偏离、TWAP 正常: %+v", reading)
		}
		t.Logf("喂价偏离：池价 %.6f，Chainlink %.6f，偏离 %.2f%%，交易被拒绝", reading.Spot, reading.Chainlink, reading.ChainlinkDeviation*100)

		// 3. 各种不可用的喂价：降级为现价 vs TWAP 两方校验，交易放行并说明原因
		now := w.n.latestTimestamp(t)
		good := big.NewInt(int64(w.price(t, pool) * 1.10 * 1e8)) // 数值仍是偏离 10% 的坏价格：不可用时不能拿它来拒绝交易
		for name, tc := range map[string]struct {
			roundID, answer            *big.Int
			updatedAt, answeredInRound uint64
			reason                     string
		}{
			"过期":      {big.NewInt(50), good, now - 7200, 50, "已过期"},
			"轮次未完成":   {big.NewInt(51), good, 0, 51, "轮次未完成"},
			"答案来自旧轮次": {big.NewInt(52), good, now, 51, "旧轮次"},
			"价格为负":    {big.NewInt(53), big.NewInt(-1), now, 53, "不是正数"},
		} {
			w.n.invoke(t, w.lp, w.feedAB, w.aggregatorABI.ABI, "setRoundData",
				tc.roundID, tc.answer, new(big.Int).SetUint64(now), new(big.Int).SetUint64(tc.updatedAt), new(big.Int).SetUint64(tc.answeredInRound))
			time.Sleep(20 * time.Millisecond)
			w.synced(t)

			var built api.BuildSwapResponse // 每次用新的变量：omitempty 的字段不会覆盖上一次的值
			if code := build(&built); code != http.StatusOK {
				t.Fatalf("%s: 喂价不可用时应降级放行，实际 %d %+v", name, code, built)
			}
			c := checkOf(built.PriceChecks)
			if c.ChainlinkStatus != oracle.ChainlinkUnavailable || !strings.Contains(c.ChainlinkDetail, tc.reason) ||
				c.Chainlink != 0 || strings.Join(c.Sources, ",") != "spot,twap" {
				t.Fatalf("%s: 校验明细 %+v", name, c)
			}
			if !strings.Contains(built.Warning, "Chainlink 喂价不可用") || !strings.Contains(built.Warning, tc.reason) {
				t.Fatalf("%s: 响应应说明降级原因，warning=%q", name, built.Warning)
			}
		}

		// 4. 喂价恢复正常：回到三方校验
		setFeed(1.0)
		built = api.BuildSwapResponse{}
		if code := build(&built); code != http.StatusOK || checkOf(built.PriceChecks).ChainlinkStatus != oracle.ChainlinkOK {
			t.Fatalf("喂价恢复后: %d %+v", code, built.PriceChecks)
		}
	})

	t.Run("全部场景之后仍然一致", func(t *testing.T) {
		w.assertStateMatchesChain(t)
		w.assertQuotesMatchQuoter(t, "全部场景之后")

		// 指标：链上抽检全部一致；三明治、套利、TWAP 偏离都有计数
		resp, err := http.Get(w.server.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		metricsText := string(raw)
		for _, want := range []string{"quote_onchain_mismatch_total 0\n", "mev_sandwich_detected_total 1\n"} {
			if !strings.Contains(metricsText, want) {
				t.Errorf("指标里没有 %q", strings.TrimSpace(want))
			}
		}
		for _, zero := range []string{"quote_onchain_checks_total 0\n", "arb_opportunities_found_total 0\n", "oracle_twap_deviation_total 0\n", "oracle_twap_unavailable_total 0\n"} {
			if strings.Contains(metricsText, zero) {
				t.Errorf("指标 %q 应大于 0", strings.TrimSpace(zero))
			}
		}
	})
}
