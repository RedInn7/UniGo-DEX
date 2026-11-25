package arb

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"unigo-dex/chain/contractabi"
	"unigo-dex/gas"
	"unigo-dex/indexer"
	umath "unigo-dex/math"
)

const (
	tokenA = "0x00000000000000000000000000000000000000aa"
	tokenB = "0x00000000000000000000000000000000000000bb"
	tokenC = "0x00000000000000000000000000000000000000cc"

	poolLow   = "0x0000000000000000000000000000000000001001" // A/B 0.05%，价格 1
	poolHigh  = "0x0000000000000000000000000000000000001002" // A/B 0.3%，价格由用例指定
	poolLendA = "0x0000000000000000000000000000000000001003" // A/C，可以借出 A
	poolLendB = "0x0000000000000000000000000000000000001004" // B/C，可以借出 B
	flashArb  = "0x0000000000000000000000000000000000009999"
	liquidity = "1000000000000000000000000" // 1e24
)

// addPool 注册一个在 tick 处初始化、[-6000, 6000] 上有 1e24 流动性的池子
func addPool(state *indexer.PoolStateManager, addr, token0, token1 string, fee uint32, spacing, tick int32) {
	sqrtPrice, _ := umath.GetSqrtRatioAtTick(tick)
	amount, _ := new(big.Int).SetString(liquidity, 10)
	p := indexer.NewPoolState(addr, token0, token1, fee, spacing)
	p.ApplyInitialize(&indexer.InitializeEvent{SqrtPriceX96: sqrtPrice, Tick: tick})
	p.ApplyMint(&indexer.MintEvent{TickLower: -6000, TickUpper: 6000, Amount: amount})
	state.AddPool(p)
}

// newWorld 两个 A/B 池子（低价池价格 1，高价池在 highTick）加两个放贷池
func newWorld(highTick int32, cfg Config) (*Scanner, *indexer.PoolStateManager) {
	state := indexer.NewPoolStateManager()
	addPool(state, poolLow, tokenA, tokenB, 500, 10, 0)
	addPool(state, poolHigh, tokenA, tokenB, 3000, 60, highTick)
	addPool(state, poolLendA, tokenA, tokenC, 3000, 60, 0)
	addPool(state, poolLendB, tokenB, tokenC, 3000, 60, 0)
	return NewScanner(cfg, state, gas.NewEstimator(gas.DefaultConfig(), nil)), state
}

func snapshot(state *indexer.PoolStateManager, addr string) indexer.PoolSnapshot {
	p, _ := state.GetPool(addr)
	return p.Snapshot()
}

func bigInt(s string) *big.Int {
	n, _ := new(big.Int).SetString(s, 10)
	return n
}

func TestFindsOpportunityWithOptimalAmount(t *testing.T) {
	// 高价池比低价池贵约 2%（tick 200），远大于 0.05% + 0.3% + 0.3% 的总费用
	s, state := newWorld(200, DefaultConfig())
	s.cfg.FlashArbAddress = flashArb
	var pushed []Opportunity
	s.OnOpportunity = func(o Opportunity) { pushed = append(pushed, o) }

	s.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh, BlockNumber: 42, Timestamp: 1700000000})

	opps := s.Opportunities()
	if len(opps) != 2 || len(pushed) != 2 {
		t.Fatalf("got %d opportunities (%d pushed), want 2 (借 A 和借 B 各一个)", len(opps), len(pushed))
	}

	for _, opp := range opps {
		var sellPool, buyPool, lender string
		switch opp.BorrowToken {
		case tokenA: // A 在高价池更贵：高价池卖 A，低价池买回
			sellPool, buyPool, lender = poolHigh, poolLow, poolLendA
		case tokenB: // B 在低价池更贵
			sellPool, buyPool, lender = poolLow, poolHigh, poolLendB
		default:
			t.Fatalf("unexpected borrow token %s", opp.BorrowToken)
		}
		if opp.SellPool != sellPool || opp.BuyPool != buyPool || opp.Lender != lender {
			t.Fatalf("borrow %s: sell=%s buy=%s lender=%s", opp.BorrowToken, opp.SellPool, opp.BuyPool, opp.Lender)
		}
		if opp.BlockNumber != 42 || opp.To != flashArb || opp.GasPriced {
			t.Fatalf("metadata: %+v", opp)
		}

		// 报出的利润就是在该借入量下逐 tick 模拟的结果
		sell, buy := snapshot(state, sellPool), snapshot(state, buyPool)
		amount, gross := bigInt(opp.Amount), bigInt(opp.GrossProfit)
		if got := Profit(sell, buy, opp.BorrowToken, 3000, amount); got.Cmp(gross) != 0 || gross.Sign() <= 0 {
			t.Fatalf("profit at optimum = %s, reported %s", got, gross)
		}

		// 最优性：借少一点、多一点，或者差很多，利润都不会更高
		for _, permille := range []int64{1, 500, 900, 990, 999, 1001, 1010, 1100, 1500, 3000} {
			other := new(big.Int).Mul(amount, big.NewInt(permille))
			other.Div(other, big.NewInt(1000))
			if p := Profit(sell, buy, opp.BorrowToken, 3000, other); p != nil && p.Cmp(gross) > 0 {
				t.Errorf("borrow %s: amount×%d‰ yields %s > optimum %s", opp.BorrowToken, permille, p, gross)
			}
		}

		// 调用数据解出来与机会一致
		raw, _ := hexutil.Decode(opp.Calldata)
		method, err := contractabi.FlashArbitrage.MethodById(raw[:4])
		if err != nil || method.Name != "arbitrage" {
			t.Fatalf("calldata selector: %v", err)
		}
		args, err := method.Inputs.Unpack(raw[4:])
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Params struct {
				Lender, SellPool, BuyPool, BorrowToken common.Address
				Amount, MinProfit                      *big.Int
			}
		}
		if err := method.Inputs.Copy(&decoded, args); err != nil {
			t.Fatal(err)
		}
		p := decoded.Params
		if p.Lender != common.HexToAddress(lender) || p.SellPool != common.HexToAddress(sellPool) ||
			p.BuyPool != common.HexToAddress(buyPool) || p.BorrowToken != common.HexToAddress(opp.BorrowToken) ||
			p.Amount.Cmp(amount) != 0 || p.MinProfit.String() != opp.MinProfit {
			t.Fatalf("decoded calldata: %+v", p)
		}
	}
}

func TestNoOpportunity(t *testing.T) {
	// 价差约 0.5%（tick 50），小于三项费用之和 0.65%
	s, _ := newWorld(50, DefaultConfig())
	s.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})
	if opps := s.Opportunities(); len(opps) != 0 {
		t.Fatalf("gap below fees should not be an opportunity: %+v", opps)
	}

	// 价格相同
	s, _ = newWorld(0, DefaultConfig())
	s.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})
	if len(s.Opportunities()) != 0 {
		t.Fatal("equal prices should not be an opportunity")
	}

	// 没有第三个池子可以放贷
	state := indexer.NewPoolStateManager()
	addPool(state, poolLow, tokenA, tokenB, 500, 10, 0)
	addPool(state, poolHigh, tokenA, tokenB, 3000, 60, 200)
	s = NewScanner(DefaultConfig(), state, gas.NewEstimator(gas.DefaultConfig(), nil))
	s.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})
	if len(s.Opportunities()) != 0 {
		t.Fatal("without a third pool to lend there is no executable opportunity")
	}
}

func TestGasCostIsDeducted(t *testing.T) {
	// A 就是 WETH：gas 成本直接以 A 计（静态估算 21.5 gwei × 500000 gas ≈ 0.01075 A）
	cfg := DefaultConfig()
	cfg.WETH = tokenA
	s, _ := newWorld(200, cfg)
	s.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})

	found := map[string]Opportunity{}
	for _, opp := range s.Opportunities() {
		found[opp.BorrowToken] = opp
	}
	wantCost := "10750000000000000"
	a := found[tokenA]
	if !a.GasPriced || a.GasCost != wantCost || a.MinProfit != wantCost {
		t.Fatalf("borrow A: %+v", a)
	}
	if net := new(big.Int).Sub(bigInt(a.GrossProfit), bigInt(a.GasCost)); net.String() != a.NetProfit {
		t.Fatalf("net profit = %s, want %s", a.NetProfit, net)
	}
	// 借 B 时按 A/B 直连池的现价折算（两个 A/B 池价格在 1 ~ 1.02 之间）
	b := found[tokenB]
	cost := bigInt(b.GasCost)
	if !b.GasPriced || cost.Cmp(bigInt(wantCost)) < 0 || cost.Cmp(bigInt("11000000000000000")) > 0 {
		t.Fatalf("borrow B gas cost = %s", b.GasCost)
	}

	// gas 贵到吃掉全部利润：不算机会
	expensive := gas.DefaultConfig()
	expensive.StaticBaseFee = new(big.Int).Lsh(big.NewInt(1), 80)
	s2, state := newWorld(200, cfg)
	s2.gas = gas.NewEstimator(expensive, nil)
	_ = state
	s2.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})
	if opps := s2.Opportunities(); len(opps) != 0 {
		t.Fatalf("gas cost above profit should not be an opportunity: %+v", opps)
	}
}

func TestOpportunityLifecycle(t *testing.T) {
	s, state := newWorld(200, DefaultConfig())
	s.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})
	if len(s.Opportunities()) != 2 {
		t.Fatal("expected opportunities")
	}

	// 价格被套平后再次成交：机会消失
	high, _ := state.GetPool(poolHigh)
	low := snapshot(state, poolLow)
	high.ApplySwap(&indexer.SwapEvent{SqrtPriceX96: low.SqrtPriceX96, Tick: low.Tick, Liquidity: low.Liquidity})
	s.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})
	if opps := s.Opportunities(); len(opps) != 0 {
		t.Fatalf("opportunities should be removed once prices converge: %+v", opps)
	}

	// Reorg 清空
	s2, _ := newWorld(200, DefaultConfig())
	s2.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})
	s2.OnReorg(1)
	if len(s2.Opportunities()) != 0 {
		t.Fatal("reorg should clear opportunities")
	}

	// 数量上限
	cfg := DefaultConfig()
	cfg.MaxOpportunities = 1
	s3, _ := newWorld(200, cfg)
	s3.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})
	if len(s3.Opportunities()) != 1 {
		t.Fatalf("got %d opportunities, want 1 (capped)", len(s3.Opportunities()))
	}
}

// fakeBalances 各放贷池持有的借入 token 余额；没列出的池子余额为 0
type fakeBalances map[string]*big.Int

func (f fakeBalances) TokenBalance(_ context.Context, _, holder string) (*big.Int, error) {
	if balance, ok := f[holder]; ok {
		return balance, nil
	}
	return new(big.Int), nil
}

// 放贷池必须真的借得出最优借入量：余额不够的跳过，换下一个；都不够就不算机会
func TestLenderMustHoldEnoughBalance(t *testing.T) {
	const poolLendA2 = "0x0000000000000000000000000000000000001005" // 另一个能借出 A 的池子，费率 1%，流动性较小
	huge := new(big.Int).Lsh(big.NewInt(1), 120)

	borrowA := func(s *Scanner) (Opportunity, bool) {
		s.OnSwap(&indexer.SwapEvent{PoolAddress: poolHigh})
		for _, opp := range s.Opportunities() {
			if opp.BorrowToken == tokenA {
				return opp, true
			}
		}
		return Opportunity{}, false
	}

	// 首选放贷池（流动性最大的）余额充足
	s, state := newWorld(200, DefaultConfig())
	addPool(state, poolLendA2, tokenA, "0x00000000000000000000000000000000000000dd", 10000, 200, 0)
	half, _ := new(big.Int).SetString("500000000000000000000000", 10)
	second, _ := state.GetPool(poolLendA2)
	second.ApplyBurn(&indexer.BurnEvent{TickLower: -6000, TickUpper: 6000, Amount: half})
	s.Balances = fakeBalances{poolLendA: huge, poolLendA2: huge}
	first, ok := borrowA(s)
	if !ok || first.Lender != poolLendA {
		t.Fatalf("should borrow from the deepest lender: %+v", first)
	}

	// 首选放贷池余额不足：换成第二个；它的闪电贷费率更高，最优借入量和利润都更小
	s.Balances = fakeBalances{poolLendA: big.NewInt(1), poolLendA2: huge}
	fallback, ok := borrowA(s)
	if !ok || fallback.Lender != poolLendA2 {
		t.Fatalf("should fall back to the second lender: %+v", fallback)
	}
	if bigInt(fallback.GrossProfit).Cmp(bigInt(first.GrossProfit)) >= 0 || bigInt(fallback.Amount).Cmp(bigInt(first.Amount)) >= 0 {
		t.Fatalf("a 1%% flash fee should shrink amount and profit: %s/%s vs %s/%s",
			fallback.Amount, fallback.GrossProfit, first.Amount, first.GrossProfit)
	}

	// 余额恰好等于借入量可以借；少 1 就不行
	exact := bigInt(first.Amount)
	s.Balances = fakeBalances{poolLendA: exact}
	if opp, ok := borrowA(s); !ok || opp.Lender != poolLendA {
		t.Fatalf("balance == amount should be enough: %+v", opp)
	}
	s.Balances = fakeBalances{poolLendA: new(big.Int).Sub(exact, big.NewInt(1))}
	if opp, ok := borrowA(s); ok {
		t.Fatalf("no lender can cover the amount, there should be no opportunity: %+v", opp)
	}
}
