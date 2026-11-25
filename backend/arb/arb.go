package arb

import (
	"context"
	"log/slog"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"unigo-dex/chain"
	"unigo-dex/gas"
	"unigo-dex/indexer"
	umath "unigo-dex/math"
	"unigo-dex/metrics"
	"unigo-dex/router"
)

// arb.go — 跨池套利机会扫描
//
// 同一个币对可以有多个费率档的池子，它们的价格由各自的成交决定，不会自动对齐。
// 两个池子出现价差时，可以零本金套利（对应合约 FlashArbitrage）：
//
//   1. 从第三个池子（放贷池）闪电贷借入 token X
//   2. 在 X 较贵的池子（卖出池）把 X 卖成另一种 token
//   3. 在 X 较便宜的池子（买入池）把另一种 token 买回 X
//   4. 归还本金和闪电贷手续费，剩下的就是利润
//
// 放贷池在放贷期间被重入锁锁住，不能同时用来交易，所以必须是第三个池子。
//
// === 借多少最赚 ===
//
// 借得越多，卖出池的价格被自己压得越低、买入池的价格被自己抬得越高，边际利润递减，
// 借过头反而亏。利润对借入数量是单峰的（先增后减），所以用三分搜索找峰值：
// 每次把区间三等分，比较两个分点的利润，丢掉利润较低一侧的三分之一。
// 利润函数直接用路由引擎的逐 tick 模拟计算，与链上执行结果一致（含两池手续费和取整）。
//
// 搜索上界取"把卖出池的价格一路压到买入池现价需要卖多少"：超过这个量，两池价格已经倒挂，一定更亏。
//
// 本模块只产出机会和调用数据，不代签、不自动发交易。

// Config 套利扫描配置
type Config struct {
	FlashArbAddress  string // FlashArbitrage 合约地址，用于填充待签名交易的 to（可空）
	WETH             string // WETH 地址（小写），用来把 gas 成本折算成借入 token（可空）
	GasLimit         uint64 // 一笔套利交易的 gas 用量估计
	MaxOpportunities int    // 保留的机会数上限
}

// DefaultConfig 返回默认配置
func DefaultConfig() Config {
	return Config{GasLimit: 500000, MaxOpportunities: 64}
}

// Opportunity 一个套利机会
type Opportunity struct {
	Lender      string `json:"lender"`       // 放贷池
	SellPool    string `json:"sell_pool"`    // 卖出借入 token 的池子（借入 token 较贵）
	BuyPool     string `json:"buy_pool"`     // 买回借入 token 的池子（借入 token 较便宜）
	BorrowToken string `json:"borrow_token"` // 借入的 token，利润也以它计
	Amount      string `json:"amount"`       // 最优借入数量

	GrossProfit string `json:"gross_profit"` // 扣除两池手续费和闪电贷手续费后的利润
	GasCost     string `json:"gas_cost"`     // gas 成本折算成借入 token 的数量
	GasPriced   bool   `json:"gas_priced"`   // false = 找不到 WETH 报价，gas 成本未计入
	NetProfit   string `json:"net_profit"`   // GrossProfit - GasCost
	MinProfit   string `json:"min_profit"`   // 写进调用数据的最低利润，达不到则链上 revert

	SellPrice float64 `json:"sell_price"` // 两个池子的现价（token1/token0）
	BuyPrice  float64 `json:"buy_price"`

	To       string `json:"to,omitempty"` // FlashArbitrage 合约地址
	Calldata string `json:"calldata"`     // FlashArbitrage.arbitrage 的调用数据
	GasLimit uint64 `json:"gas_limit"`

	BlockNumber uint64 `json:"block_number"` // 发现该机会时的区块
	Timestamp   uint64 `json:"timestamp"`
}

// Scanner 套利机会扫描器，实现 indexer.SwapListener
type Scanner struct {
	cfg   Config
	state *indexer.PoolStateManager
	gas   *gas.Estimator

	mu            sync.RWMutex
	opportunities map[string]Opportunity // key: 两个池子（地址升序）+ 借入 token

	// OnOpportunity 每发现一个机会后回调（实时推送用）。
	// 在索引协程里同步调用，必须在索引器启动前设置，且不能阻塞。
	OnOpportunity func(Opportunity)
}

// NewScanner 创建套利扫描器
func NewScanner(cfg Config, state *indexer.PoolStateManager, gasEstimator *gas.Estimator) *Scanner {
	return &Scanner{cfg: cfg, state: state, gas: gasEstimator, opportunities: make(map[string]Opportunity)}
}

// OnSwap 池子成交后，把它和同币对的其它池子逐一比价
func (s *Scanner) OnSwap(e *indexer.SwapEvent) {
	pools := s.state.GetAllPools()
	var changed *indexer.PoolSnapshot
	for i := range pools {
		if pools[i].Address == e.PoolAddress {
			changed = &pools[i]
		}
	}
	if changed == nil {
		return
	}

	for i := range pools {
		other := &pools[i]
		if other.Address == changed.Address || other.Token0 != changed.Token0 || other.Token1 != changed.Token1 {
			continue
		}
		for _, borrow := range []string{changed.Token0, changed.Token1} {
			key := pairKey(changed.Address, other.Address, borrow)
			opp, ok := s.evaluate(pools, changed, other, borrow)
			if ok {
				opp.BlockNumber, opp.Timestamp = e.BlockNumber, e.Timestamp
			}
			s.store(key, opp, ok)
		}
	}
}

// OnReorg 池子状态已回滚，已有的机会全部作废，等回滚后的成交重新触发扫描
func (s *Scanner) OnReorg(uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.opportunities)
	metrics.ArbOpportunitiesActive.Set(0)
}

func (s *Scanner) store(key string, opp Opportunity, ok bool) {
	s.mu.Lock()
	_, existed := s.opportunities[key]
	if ok && (existed || len(s.opportunities) < s.cfg.MaxOpportunities) {
		s.opportunities[key] = opp
	} else {
		delete(s.opportunities, key)
		ok = false
	}
	metrics.ArbOpportunitiesActive.Set(int64(len(s.opportunities)))
	s.mu.Unlock()

	if ok {
		metrics.ArbOpportunitiesFound.Inc()
		slog.Info("发现套利机会", "sell_pool", opp.SellPool, "buy_pool", opp.BuyPool,
			"borrow_token", opp.BorrowToken, "amount", opp.Amount, "net_profit", opp.NetProfit)
		if s.OnOpportunity != nil {
			s.OnOpportunity(opp)
		}
	}
}

// Opportunities 返回当前的套利机会，净利润高的在前
func (s *Scanner) Opportunities() []Opportunity {
	s.mu.RLock()
	out := make([]Opportunity, 0, len(s.opportunities))
	for _, opp := range s.opportunities {
		out = append(out, opp)
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		a, _ := new(big.Int).SetString(out[i].NetProfit, 10)
		b, _ := new(big.Int).SetString(out[j].NetProfit, 10)
		if c := a.Cmp(b); c != 0 {
			return c > 0
		}
		return out[i].Calldata < out[j].Calldata
	})
	return out
}

// evaluate 评估在 a、b 两个同币对池子之间借入 borrow 套利是否有利可图
func (s *Scanner) evaluate(pools []indexer.PoolSnapshot, a, b *indexer.PoolSnapshot, borrow string) (Opportunity, bool) {
	if a.SqrtPriceX96.Sign() == 0 || b.SqrtPriceX96.Sign() == 0 || a.SqrtPriceX96.Cmp(b.SqrtPriceX96) == 0 {
		return Opportunity{}, false
	}
	// 价格是 token1/token0：价格高的池子里 token0 更贵。
	// 借 token0 就在高价池卖出、低价池买回；借 token1 反过来。
	high, low := a, b
	if a.SqrtPriceX96.Cmp(b.SqrtPriceX96) < 0 {
		high, low = b, a
	}
	sell, buy := high, low
	if borrow == a.Token1 {
		sell, buy = low, high
	}

	lender := pickLender(pools, borrow, sell.Address, buy.Address)
	if lender == nil {
		return Opportunity{}, false
	}

	// 快速排除：借入量趋近于 0 时的边际利润都不为正，就不必搜索了。
	// 每借 1 个单位：卖出再买回得到 ratio × (1-卖出池费率) × (1-买入池费率)，要还 1 + 闪电贷费率
	ratio := umath.SqrtPriceX96ToPrice(high.SqrtPriceX96) / umath.SqrtPriceX96ToPrice(low.SqrtPriceX96)
	keep := (1 - float64(sell.Fee)/1e6) * (1 - float64(buy.Fee)/1e6)
	if ratio*keep <= 1+float64(lender.Fee)/1e6 {
		return Opportunity{}, false
	}

	amount, gross := OptimalAmount(*sell, *buy, borrow, lender.Fee)
	if amount == nil || gross.Sign() <= 0 {
		return Opportunity{}, false
	}

	gasCost, priced := s.gasCostIn(pools, borrow)
	net := new(big.Int).Sub(gross, gasCost)
	if net.Sign() <= 0 {
		return Opportunity{}, false
	}
	// 链上至少要赚回 gas 成本，否则 revert（只损失失败交易的 gas）
	minProfit := gasCost
	if minProfit.Sign() == 0 {
		minProfit = big.NewInt(1)
	}

	calldata, err := chain.EncodeArbitrage(lender.Address, sell.Address, buy.Address, borrow, amount, minProfit)
	if err != nil {
		slog.Warn("编码套利调用数据失败", "err", err)
		return Opportunity{}, false
	}
	return Opportunity{
		Lender:      lender.Address,
		SellPool:    sell.Address,
		BuyPool:     buy.Address,
		BorrowToken: borrow,
		Amount:      amount.String(),
		GrossProfit: gross.String(),
		GasCost:     gasCost.String(),
		GasPriced:   priced,
		NetProfit:   net.String(),
		MinProfit:   minProfit.String(),
		SellPrice:   umath.SqrtPriceX96ToPrice(sell.SqrtPriceX96),
		BuyPrice:    umath.SqrtPriceX96ToPrice(buy.SqrtPriceX96),
		To:          s.cfg.FlashArbAddress,
		Calldata:    hexutil.Encode(calldata),
		GasLimit:    s.cfg.GasLimit,
	}, true
}

// pickLender 选放贷池：含有借入 token、不是卖出池也不是买入池，取活跃流动性最大的
//
// 已知上限：链下没有跟踪池子的 token 余额，放贷池余额不足时链上会 revert；
// 需要时改成按 Mint / Burn / Swap / Collect 事件累计余额后再筛选
func pickLender(pools []indexer.PoolSnapshot, borrow, sellPool, buyPool string) *indexer.PoolSnapshot {
	var best *indexer.PoolSnapshot
	for i := range pools {
		p := &pools[i]
		if p.Address == sellPool || p.Address == buyPool || (p.Token0 != borrow && p.Token1 != borrow) {
			continue
		}
		if p.SqrtPriceX96.Sign() == 0 || p.Liquidity.Sign() == 0 {
			continue
		}
		if best == nil || p.Liquidity.Cmp(best.Liquidity) > 0 {
			best = p
		}
	}
	return best
}

// Profit 借入 amount 个 borrow、在 sell 卖出再在 buy 买回、还本付息后的利润
//
// 任何一步无法成交（流动性不足）时返回 nil。
func Profit(sell, buy indexer.PoolSnapshot, borrow string, lenderFee uint32, amount *big.Int) *big.Int {
	other := sell.Token0
	if borrow == sell.Token0 {
		other = sell.Token1
	}
	otherAmount, _, err := router.QuoteExactInputSingle(sell, borrow, amount)
	if err != nil || otherAmount.Sign() <= 0 {
		return nil
	}
	back, _, err := router.QuoteExactInputSingle(buy, other, otherAmount)
	if err != nil {
		return nil
	}
	// 闪电贷手续费向上取整，与 UniGoPool.flash 一致
	flashFee := umath.MulDivRoundingUp(amount, big.NewInt(int64(lenderFee)), big.NewInt(1e6))
	profit := new(big.Int).Sub(back, amount)
	return profit.Sub(profit, flashFee)
}

// OptimalAmount 三分搜索利润最大的借入数量，返回 (数量, 利润)；无利可图时数量为 nil
func OptimalAmount(sell, buy indexer.PoolSnapshot, borrow string, lenderFee uint32) (*big.Int, *big.Int) {
	// 搜索上界：把卖出池的价格推到买入池现价需要投入的数量
	zeroForOne := borrow == sell.Token0
	huge := new(big.Int).Lsh(big.NewInt(1), 200)
	toParity, err := router.SimulateSwap(sell, zeroForOne, huge, buy.SqrtPriceX96)
	if err != nil {
		return nil, nil
	}
	hi := toParity.Amount1
	if zeroForOne {
		hi = toParity.Amount0
	}
	if hi.Sign() <= 0 {
		return nil, nil
	}
	lo := big.NewInt(1)

	// 无法成交的数量视为负无穷
	less := func(a, b *big.Int) bool {
		if a == nil {
			return b != nil
		}
		return b != nil && a.Cmp(b) < 0
	}
	profit := func(amount *big.Int) *big.Int { return Profit(sell, buy, borrow, lenderFee, amount) }

	three := big.NewInt(3)
	for new(big.Int).Sub(hi, lo).Cmp(three) > 0 {
		third := new(big.Int).Sub(hi, lo)
		third.Div(third, three)
		m1 := new(big.Int).Add(lo, third)
		m2 := new(big.Int).Sub(hi, third)
		if less(profit(m1), profit(m2)) {
			lo = m1
		} else {
			hi = m2
		}
	}

	var bestAmount, bestProfit *big.Int
	for amount := new(big.Int).Set(lo); amount.Cmp(hi) <= 0; amount = new(big.Int).Add(amount, big.NewInt(1)) {
		if p := profit(amount); less(bestProfit, p) {
			bestAmount, bestProfit = amount, p
		}
	}
	if bestProfit == nil {
		return nil, nil
	}
	return bestAmount, bestProfit
}

// gasCostIn 把一笔套利交易的 gas 成本折算成 token 的数量
//
// token 就是 WETH 时直接用 wei；否则按 WETH/token 直连池的现价折算；
// 没有配置 WETH 或找不到直连池时返回 (0, false)，由调用方标注 gas 成本未计入。
func (s *Scanner) gasCostIn(pools []indexer.PoolSnapshot, token string) (*big.Int, bool) {
	if s.cfg.WETH == "" {
		return new(big.Int), false
	}
	costWei := s.gas.CostWei(context.Background(), s.cfg.GasLimit)
	if token == s.cfg.WETH {
		return costWei, true
	}
	for _, p := range pools {
		price := umath.SqrtPriceX96ToPrice(p.SqrtPriceX96) // token1/token0
		if price == 0 {
			continue
		}
		cost := new(big.Float).SetInt(costWei)
		switch {
		case p.Token0 == s.cfg.WETH && p.Token1 == token:
			cost.Mul(cost, big.NewFloat(price))
		case p.Token1 == s.cfg.WETH && p.Token0 == token:
			cost.Quo(cost, big.NewFloat(price))
		default:
			continue
		}
		out, _ := cost.Int(nil)
		return out, true
	}
	return new(big.Int), false
}

func pairKey(poolA, poolB, borrow string) string {
	if poolA > poolB {
		poolA, poolB = poolB, poolA
	}
	return poolA + "|" + poolB + "|" + borrow
}
