package mev

import (
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"sync"
	"time"

	"unigo-dex/indexer"
	umath "unigo-dex/math"
	"unigo-dex/metrics"
)

// mev.go — 基础 MEV 防御
//
// 三明治攻击：攻击者看到受害者的大额交易后，抢在前面同向买入推高价格，
// 等受害者以更差的价格成交，再反向卖出获利。受害者能被夹多狠，取决于
// 他愿意接受多差的成交价（min_amount_out 设得多松）。
//
// 这里做两件事：
//
//   1. 交易前保护
//      - 按报价和池内现价算价格冲击，冲击过大的交易直接拒绝（大单最容易被夹）
//      - 按用户滑点给出推荐的 min_amount_out
//      - deadline 已过期或设得过长的交易拒绝（长 deadline 给了攻击者择时的空间）
//
//   2. 三明治检测
//      - 在 Swap 事件流上识别"同一区块、同一池子内：X 交易 → 受害者同向交易 → X 反向交易"
//      - 记录告警、计入 metrics；近期被夹过的池子在报价时提示并收紧推荐滑点

// Config MEV 防御配置
type Config struct {
	MaxPriceImpact  float64       // 价格冲击上限，超过则拒绝 swap（0.05 = 5%）
	DefaultSlippage float64       // 用户未指定时的滑点
	MaxSlippage     float64       // 允许的最大滑点
	TightSlippage   float64       // 池子近期被夹过时，推荐滑点收紧到这个值
	MaxDeadline     time.Duration // deadline 距当前时间的最大间隔
	AlertWindow     time.Duration // 告警在多长时间内算"近期"
	MaxAlerts       int           // 保留的告警条数上限
}

// DefaultConfig 返回默认配置
func DefaultConfig() Config {
	return Config{
		MaxPriceImpact:  0.05,
		DefaultSlippage: 0.005,
		MaxSlippage:     0.5,
		TightSlippage:   0.001,
		MaxDeadline:     30 * time.Minute,
		AlertWindow:     10 * time.Minute,
		MaxAlerts:       500,
	}
}

// 单个区块内每个池子参与检测的 swap 数上限
// 已知上限：检测对每笔新 swap 是 O(n²)，n 有上限所以够用；真有单池单块上千笔 swap 再换成按地址索引
const maxSwapsPerBlock = 256

var (
	ErrPriceImpactTooHigh = errors.New("price impact too high")
	ErrDeadlineExpired    = errors.New("deadline expired")
	ErrDeadlineTooFar     = errors.New("deadline too far in the future")
)

// Alert 一次疑似三明治攻击
type Alert struct {
	Pool        string `json:"pool"`
	BlockNumber uint64 `json:"block_number"`
	Timestamp   uint64 `json:"timestamp"` // 区块时间戳（Unix 秒）
	// 攻击者按 (sender, recipient) 识别：经路由合约下单时 sender 都是路由合约，要靠 recipient 区分
	Attacker       string   `json:"attacker"`        // 前后两笔交易的资金接收地址
	AttackerSender string   `json:"attacker_sender"` // 前后两笔交易的调用者
	FrontTx        string   `json:"front_tx"`        // 抢跑交易
	BackTx         string   `json:"back_tx"`         // 反向平仓交易
	VictimTxs      []string `json:"victim_txs"`      // 被夹在中间的同向交易
}

// Guard MEV 防御：交易前检查 + 三明治检测，实现 indexer.SwapListener
type Guard struct {
	cfg Config

	mu       sync.RWMutex
	alerts   []Alert                         // 按时间升序
	curBlock uint64                          // pending 对应的区块号
	pending  map[string][]*indexer.SwapEvent // 当前区块内各池子的 swap，按 logIndex 升序
}

// NewGuard 创建 MEV 防御实例
func NewGuard(cfg Config) *Guard {
	return &Guard{cfg: cfg, pending: make(map[string][]*indexer.SwapEvent)}
}

// ===== 交易前保护 =====

// PriceImpact 计算一笔交易的价格冲击
//
// 以池内现价（不含冲击）成交能得到 spotOut，实际报价为 amountOut：
//
//	impact = 1 - amountOut / spotOut
//
// 手续费不计入冲击。任何一跳没有价格时按 100% 处理。
func PriceImpact(pools []indexer.PoolSnapshot, tokenIn string, amountIn, amountOut *big.Int) float64 {
	spotOut, _ := new(big.Float).SetInt(amountIn).Float64()
	token := tokenIn
	for _, pool := range pools {
		price := umath.SqrtPriceX96ToPrice(pool.SqrtPriceX96) // token1 / token0
		if price == 0 {
			return 1
		}
		if token == pool.Token0 {
			spotOut *= price
			token = pool.Token1
		} else {
			spotOut /= price
			token = pool.Token0
		}
		spotOut *= 1 - float64(pool.Fee)/1e6
	}
	if spotOut <= 0 {
		return 1
	}
	out, _ := new(big.Float).SetInt(amountOut).Float64()
	return min(max(1-out/spotOut, 0), 1)
}

// MinAmountOut 按滑点计算最少应得数量：amountOut × (1 - slippage)，向下取整
func MinAmountOut(amountOut *big.Int, slippage float64) *big.Int {
	const precision = 1_000_000
	keep := big.NewInt(precision - int64(slippage*precision+0.5))
	return keep.Div(keep.Mul(keep, amountOut), big.NewInt(precision))
}

// CheckSwap 交易前检查：价格冲击过大、deadline 已过期或过长时返回错误
func (g *Guard) CheckSwap(priceImpact float64, deadline int64, now time.Time) error {
	var err error
	switch {
	case priceImpact > g.cfg.MaxPriceImpact:
		err = fmt.Errorf("%w: %.2f%% > %.2f%%，请拆分订单", ErrPriceImpactTooHigh, priceImpact*100, g.cfg.MaxPriceImpact*100)
	case deadline <= now.Unix():
		err = ErrDeadlineExpired
	case deadline > now.Add(g.cfg.MaxDeadline).Unix():
		err = fmt.Errorf("%w: 最长 %v", ErrDeadlineTooFar, g.cfg.MaxDeadline)
	}
	if err != nil {
		metrics.MEVSwapsRejected.Inc()
	}
	return err
}

// RecommendSlippage 给出推荐滑点
//
// 用户滑点缺省或越界时回到合理范围；路径上有池子近期被夹过时收紧滑点并返回提示。
func (g *Guard) RecommendSlippage(pools []string, requested float64, now time.Time) (slippage float64, warning string) {
	slippage = requested
	if slippage <= 0 {
		slippage = g.cfg.DefaultSlippage
	}
	slippage = min(slippage, g.cfg.MaxSlippage)

	for _, pool := range pools {
		if g.recentlyAttacked(pool, now) {
			slippage = min(slippage, g.cfg.TightSlippage)
			return slippage, fmt.Sprintf(
				"⚠️ 池子 %s 近期检测到三明治攻击，推荐滑点已收紧到 %.2f%%，建议拆分订单或使用私有交易通道。",
				pool, slippage*100)
		}
	}
	return slippage, ""
}

// recentlyAttacked 池子在 AlertWindow 内是否有三明治告警（按区块时间）
func (g *Guard) recentlyAttacked(pool string, now time.Time) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	cutoff := now.Add(-g.cfg.AlertWindow).Unix()
	for i := len(g.alerts) - 1; i >= 0 && int64(g.alerts[i].Timestamp) >= cutoff; i-- {
		if g.alerts[i].Pool == pool {
			return true
		}
	}
	return false
}

// ===== 三明治检测 =====

// OnSwap 把 swap 记入当前区块，并检查它是否构成一次三明治的"反向平仓"
func (g *Guard) OnSwap(e *indexer.SwapEvent) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if e.BlockNumber != g.curBlock {
		g.curBlock = e.BlockNumber
		clear(g.pending)
	}

	swaps := append(g.pending[e.PoolAddress], e)
	sort.SliceStable(swaps, func(i, j int) bool { return swaps[i].LogIndex < swaps[j].LogIndex })
	if len(swaps) > maxSwapsPerBlock {
		swaps = swaps[len(swaps)-maxSwapsPerBlock:]
	}

	back := len(swaps) - 1
	for swaps[back] != e {
		back--
	}
	if front, victims := findFrontRun(swaps, back); front >= 0 {
		alert := Alert{
			Pool:           e.PoolAddress,
			BlockNumber:    e.BlockNumber,
			Timestamp:      e.Timestamp,
			Attacker:       e.Recipient,
			AttackerSender: e.Sender,
			FrontTx:        swaps[front].TxHash,
			BackTx:         e.TxHash,
			VictimTxs:      victims,
		}
		g.alerts = append(g.alerts, alert)
		if len(g.alerts) > g.cfg.MaxAlerts {
			g.alerts = g.alerts[len(g.alerts)-g.cfg.MaxAlerts:]
		}
		metrics.MEVSandwichDetected.Inc()
		slog.Warn("检测到疑似三明治攻击",
			"pool", alert.Pool, "block", alert.BlockNumber, "attacker", alert.Attacker,
			"front_tx", alert.FrontTx, "back_tx", alert.BackTx, "victims", len(victims))

		// 这一组交易已经结案，不再参与后续匹配
		swaps = swaps[back+1:]
	}
	g.pending[e.PoolAddress] = swaps
}

// findFrontRun 以 swaps[back] 为反向平仓交易，向前找与之配对的抢跑交易
//
// 配对条件：
//   - 抢跑交易与平仓交易来自同一地址（sender 和 recipient 都相同）、方向相反
//   - 两者之间至少有一笔别人的、与抢跑同向的交易（受害者）
//
// 返回抢跑交易的下标和受害者的交易哈希；没有命中时下标为 -1。
func findFrontRun(swaps []*indexer.SwapEvent, back int) (front int, victims []string) {
	b := swaps[back]
	for front = back - 1; front >= 0; front-- {
		f := swaps[front]
		if !sameActor(f, b) || zeroForOne(f) == zeroForOne(b) || f.TxHash == b.TxHash {
			continue
		}
		victims = nil
		for _, v := range swaps[front+1 : back] {
			if !sameActor(v, f) && zeroForOne(v) == zeroForOne(f) && v.TxHash != f.TxHash && v.TxHash != b.TxHash {
				victims = append(victims, v.TxHash)
			}
		}
		if len(victims) > 0 {
			return front, victims
		}
	}
	return -1, nil
}

func sameActor(a, b *indexer.SwapEvent) bool {
	return a.Sender == b.Sender && a.Recipient == b.Recipient
}

// zeroForOne swap 方向：池子收到 token0（amount0 > 0）即 token0 换 token1
func zeroForOne(e *indexer.SwapEvent) bool {
	return e.Amount0.Sign() > 0
}

// OnReorg 丢弃区块号 >= forkBlock 的检测状态和告警
func (g *Guard) OnReorg(forkBlock uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.curBlock >= forkBlock {
		g.curBlock = 0
		clear(g.pending)
	}
	kept := g.alerts[:0]
	for _, a := range g.alerts {
		if a.BlockNumber < forkBlock {
			kept = append(kept, a)
		}
	}
	g.alerts = kept
}

// Alerts 返回最近的 limit 条告警，最新的在前
func (g *Guard) Alerts(limit int) []Alert {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if limit <= 0 || limit > len(g.alerts) {
		limit = len(g.alerts)
	}
	out := make([]Alert, 0, limit)
	for i := len(g.alerts) - 1; i >= len(g.alerts)-limit; i-- {
		out = append(out, g.alerts[i])
	}
	return out
}
