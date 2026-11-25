//go:build integration

package integration

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"unigo-dex/chain"
	"unigo-dex/indexer"
	"unigo-dex/router"
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

	erc20, poolABI, managerABI artifact

	factory, manager, quoter, flashArb common.Address
	tokenA, tokenB, tokenC             common.Address // 地址升序
	poolAB3000, poolAB500              common.Address // 同一币对的两个池子（套利用）
	poolBC3000, poolAC10000            common.Address

	client *chain.Client
	state  *indexer.PoolStateManager
	idx    *indexer.Indexer
	router *router.Router
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

// startBackend 把后端接到节点上
func (w *world) startBackend(t *testing.T, ctx context.Context) {
	client, err := chain.Dial(ctx, w.n.url, w.factory.Hex())
	if err != nil {
		t.Fatalf("chain.Dial: %v", err)
	}
	w.client = client
	w.state = indexer.NewPoolStateManager()

	cfg := indexer.DefaultConfig()
	cfg.PollInterval = 100 * time.Millisecond
	cfg.ConfirmationBlocks = 3
	w.idx = indexer.NewIndexer(cfg, w.state, client)
	w.router = router.NewRouter(router.DefaultRouterConfig(), w.state)

	if err := w.idx.Start(ctx); err != nil {
		t.Fatal(err)
	}
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
	total, withCrossing, maxCrossed := 0, 0, 0
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
			// 两边都报不出价（比如 1 wei 被手续费吃光后输出为 0，或流动性不足）也算一致
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
	t.Logf("[%s] 对比 %d 条报价（其中 %d 条穿越了已初始化的 tick，单条最多穿越 %d 个），链下与链上 Quoter 逐位一致",
		stage, total, withCrossing, maxCrossed)
}

func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := deployWorld(t)
	w.startBackend(t, ctx)

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
}
