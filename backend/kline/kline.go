package kline

import (
	"errors"
	"math/big"
	"sort"
	"sync"

	"unigo-dex/indexer"
	umath "unigo-dex/math"
)

// kline.go — K 线（OHLCV）与成交记录
//
// 消费索引器的 Swap 事件流，按池子聚合出 1m / 5m / 15m / 1h / 4h / 1d 六个周期的 K 线，
// 并保留最近的逐笔成交。
//
//   - 价格：由 swap 后的 sqrtPriceX96 换算，price = (sqrtPriceX96 / 2^96)^2，
//     即 1 个 token0 最小单位值多少 token1 最小单位（未按 decimals 调整）
//   - 成交量：amount0 / amount1 的绝对值，用定点整数（最小单位）累加，不丢精度；
//     JSON 里是十进制字符串，避免超出 JavaScript 的安全整数范围
//   - 时间：区块时间戳
//
// === Reorg 回滚 ===
//
// 一根 K 线的最高价、最低价不能"减去"一笔成交，所以回滚靠检查点：
// 每个区块的第一笔成交进来之前，先把当时最新的那根 1m K 线存一份。
// 回滚区块 >= forkBlock 时，取第一个被回滚区块的检查点：
//   - 1m：检查点之后的 K 线全部删除，检查点那一根恢复成存下来的样子
//   - 更高周期：受影响的那一根由 1m K 线重新合并，之后的删除
//
// 这样回滚后的 K 线与"从没见过被回滚的成交"完全一致，与保留了多少笔成交无关。
// 检查点保留最近 maxCheckpoints 个有成交的区块，远大于索引器的回滚窗口；
// 万一回滚得比这还深，就清空这个池子的 K 线，用保留的成交重建。
//
// === 内存上限 ===
//
// 每个池子每个周期最多保留 MaxCandles 根 K 线、MaxTrades 笔成交，池子数不超过 MaxPools。

// Config K 线聚合配置
type Config struct {
	MaxCandles int // 每个池子每个周期保留的 K 线数（至少 1440，保证 1d K 线可由 1m 重算）
	MaxTrades  int // 每个池子保留的成交笔数
	MaxPools   int // 最多跟踪的池子数
}

// DefaultConfig 返回默认配置
func DefaultConfig() Config {
	return Config{MaxCandles: 1500, MaxTrades: 1000, MaxPools: 1024}
}

// 支持的周期，第一个必须是 1m（其余周期在 Reorg 时由它重算）
var intervals = []struct {
	name string
	secs int64
}{
	{"1m", 60}, {"5m", 300}, {"15m", 900}, {"1h", 3600}, {"4h", 14400}, {"1d", 86400},
}

// ErrInvalidInterval 不支持的 K 线周期
var ErrInvalidInterval = errors.New("invalid interval, supported: 1m 5m 15m 1h 4h 1d")

// Candle 一根 K 线
type Candle struct {
	OpenTime int64   `json:"open_time"` // 周期起始时间（Unix 秒）
	Open     float64 `json:"open"`
	High     float64 `json:"high"`
	Low      float64 `json:"low"`
	Close    float64 `json:"close"`
	Volume0  string  `json:"volume0"` // token0 成交量（最小单位，十进制字符串）
	Volume1  string  `json:"volume1"` // token1 成交量（最小单位，十进制字符串）
	Trades   int     `json:"trades"`  // 成交笔数
}

// candle K 线的内部表示：成交量是定点整数
type candle struct {
	openTime               int64
	open, high, low, close float64
	volume0, volume1       *big.Int
	trades                 int
}

func (c candle) export() Candle {
	return Candle{
		OpenTime: c.openTime, Open: c.open, High: c.high, Low: c.low, Close: c.close,
		Volume0: c.volume0.String(), Volume1: c.volume1.String(), Trades: c.trades,
	}
}

// clone 深拷贝：成交量在合并时原地累加，存档和新建 K 线都要用自己的一份
func (c candle) clone() candle {
	c.volume0 = new(big.Int).Set(c.volume0)
	c.volume1 = new(big.Int).Set(c.volume1)
	return c
}

// checkpoint 某个区块的第一笔成交进来之前，池子最新一根 1m K 线的存档
type checkpoint struct {
	block uint64
	had   bool   // 当时是否已有 K 线
	last  candle // 当时最新的 1m K 线
}

// 每个池子保留的检查点数（有成交的区块数）
const maxCheckpoints = 256

// Trade 一笔成交
type Trade struct {
	Pool        string  `json:"pool"`
	TxHash      string  `json:"tx_hash"`
	LogIndex    uint    `json:"log_index"`
	BlockNumber uint64  `json:"block_number"`
	Timestamp   int64   `json:"timestamp"`
	Sender      string  `json:"sender"`
	Recipient   string  `json:"recipient"`
	Price       float64 `json:"price"`        // 成交后的价格
	Amount0     string  `json:"amount0"`      // 带符号，正数表示池子收入
	Amount1     string  `json:"amount1"`      // 带符号
	ZeroForOne  bool    `json:"zero_for_one"` // true = 卖出 token0 换 token1
}

type poolData struct {
	trades  []Trade    // 按时间升序
	candles [][]candle // 下标与 intervals 对应，按 openTime 升序

	checkpoints []checkpoint // 按区块号升序
	// 区块号大于 checkpointFloor 的有成交区块都有检查点（更早的已被淘汰）
	checkpointFloor uint64
	lastBlock       uint64 // 最近一笔成交所在的区块
	hasTrades       bool
}

// Aggregator K 线聚合器，实现 indexer.SwapListener
type Aggregator struct {
	cfg   Config
	mu    sync.RWMutex
	pools map[string]*poolData

	// OnTrade 每记入一笔成交后回调，参数是这笔成交和更新后的 1m K 线（实时推送用）。
	// 在索引协程里同步调用，必须在索引器启动前设置，且不能阻塞。
	OnTrade func(trade Trade, candle Candle)
}

// NewAggregator 创建 K 线聚合器
func NewAggregator(cfg Config) *Aggregator {
	if cfg.MaxCandles < 1440 {
		cfg.MaxCandles = 1440
	}
	return &Aggregator{cfg: cfg, pools: make(map[string]*poolData)}
}

// OnSwap 把一笔 Swap 记为成交并更新各周期 K 线
func (a *Aggregator) OnSwap(e *indexer.SwapEvent) {
	price := umath.SqrtPriceX96ToPrice(e.SqrtPriceX96)
	if price == 0 {
		return
	}
	trade := Trade{
		Pool:        e.PoolAddress,
		TxHash:      e.TxHash,
		LogIndex:    e.LogIndex,
		BlockNumber: e.BlockNumber,
		Timestamp:   int64(e.Timestamp),
		Sender:      e.Sender,
		Recipient:   e.Recipient,
		Price:       price,
		Amount0:     e.Amount0.String(),
		Amount1:     e.Amount1.String(),
		ZeroForOne:  e.Amount0.Sign() > 0,
	}

	candle, ok := a.record(e, trade)
	if ok && a.OnTrade != nil {
		a.OnTrade(trade, candle)
	}
}

// record 把成交记入池子并更新各周期 K 线，返回更新后的 1m K 线
func (a *Aggregator) record(e *indexer.SwapEvent, trade Trade) (Candle, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	p, ok := a.pools[e.PoolAddress]
	if !ok {
		if len(a.pools) >= a.cfg.MaxPools {
			return Candle{}, false
		}
		p = &poolData{candles: make([][]candle, len(intervals))}
		a.pools[e.PoolAddress] = p
	}
	// 区块时间戳单调递增；时间倒退的成交不可能来自一条合法的链，丢弃
	if n := len(p.trades); n > 0 && trade.Timestamp < p.trades[n-1].Timestamp {
		return Candle{}, false
	}

	a.apply(p, trade, new(big.Int).Abs(e.Amount0), new(big.Int).Abs(e.Amount1))
	oneMin := p.candles[0]
	return oneMin[len(oneMin)-1].export(), true
}

// apply 把一笔成交记入池子：必要时存检查点，追加成交记录，更新六个周期的 K 线
func (a *Aggregator) apply(p *poolData, trade Trade, volume0, volume1 *big.Int) {
	// 新区块的第一笔成交：先把当前最新的 1m K 线存档
	if !p.hasTrades || trade.BlockNumber != p.lastBlock {
		cp := checkpoint{block: trade.BlockNumber}
		if oneMin := p.candles[0]; len(oneMin) > 0 {
			cp.had, cp.last = true, oneMin[len(oneMin)-1].clone()
		}
		p.checkpoints = append(p.checkpoints, cp)
		if len(p.checkpoints) > maxCheckpoints {
			p.checkpointFloor = p.checkpoints[0].block
			p.checkpoints = p.checkpoints[1:]
		}
		p.lastBlock, p.hasTrades = trade.BlockNumber, true
	}

	p.trades = append(p.trades, trade)
	if len(p.trades) > a.cfg.MaxTrades {
		p.trades = p.trades[len(p.trades)-a.cfg.MaxTrades:]
	}

	c := candle{
		openTime: trade.Timestamp,
		open:     trade.Price, high: trade.Price, low: trade.Price, close: trade.Price,
		volume0: volume0, volume1: volume1,
		trades: 1,
	}
	for i, iv := range intervals {
		p.candles[i] = a.merge(p.candles[i], iv.secs, c)
	}
}

// OnReorg 丢弃区块号 >= forkBlock 的成交，并把受影响的 K 线恢复到这些成交发生之前的样子
func (a *Aggregator) OnReorg(forkBlock uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.pools {
		a.rollback(p, forkBlock)
	}
}

func (a *Aggregator) rollback(p *poolData, forkBlock uint64) {
	if !p.hasTrades || p.lastBlock < forkBlock {
		return // 这个池子在被回滚的区块里没有成交
	}
	firstTrade := sort.Search(len(p.trades), func(i int) bool { return p.trades[i].BlockNumber >= forkBlock })
	survivors := p.trades[:firstTrade]

	if forkBlock <= p.checkpointFloor {
		// 回滚得比检查点保留的范围还深：清空，用保留的成交重建
		*p = poolData{candles: make([][]candle, len(intervals))}
		for _, t := range survivors {
			amount0, _ := new(big.Int).SetString(t.Amount0, 10)
			amount1, _ := new(big.Int).SetString(t.Amount1, 10)
			a.apply(p, t, amount0.Abs(amount0), amount1.Abs(amount1))
		}
		return
	}

	// 第一个被回滚区块的检查点：那个区块的成交进来之前的状态
	i := sort.Search(len(p.checkpoints), func(i int) bool { return p.checkpoints[i].block >= forkBlock })
	cp := p.checkpoints[i]
	p.checkpoints = p.checkpoints[:i]
	p.trades = survivors
	// 最近一笔仍然有效的成交所在的区块：上一个检查点的区块；检查点都没了就是保留范围的下界
	if i > 0 {
		p.lastBlock = p.checkpoints[i-1].block
	} else {
		p.lastBlock, p.hasTrades = p.checkpointFloor, p.checkpointFloor > 0
	}

	// 1m：检查点之后的 K 线删除，检查点那一根恢复原样
	oneMin := p.candles[0]
	if cp.had {
		keep := sort.Search(len(oneMin), func(j int) bool { return oneMin[j].openTime >= cp.last.openTime })
		oneMin = append(oneMin[:keep], cp.last)
	} else {
		oneMin = oneMin[:0]
	}
	p.candles[0] = oneMin

	// 更高周期：包含检查点那一分钟的那一根由 1m K 线重新合并，之后的删除
	for k := 1; k < len(intervals); k++ {
		secs := intervals[k].secs
		if !cp.had {
			p.candles[k] = p.candles[k][:0]
			continue
		}
		start := bucket(cp.last.openTime, secs)
		series := p.candles[k]
		series = series[:sort.Search(len(series), func(j int) bool { return series[j].openTime >= start })]
		from := sort.Search(len(oneMin), func(j int) bool { return oneMin[j].openTime >= start })
		for _, c := range oneMin[from:] {
			series = a.merge(series, secs, c)
		}
		p.candles[k] = series
	}
}

// merge 把 c（一笔成交或一根更小周期的 K 线）并入 series 的最后一根，或追加新的一根
func (a *Aggregator) merge(series []candle, secs int64, c candle) []candle {
	openTime := bucket(c.openTime, secs)
	if n := len(series); n > 0 && series[n-1].openTime == openTime {
		last := &series[n-1]
		last.high = max(last.high, c.high)
		last.low = min(last.low, c.low)
		last.close = c.close
		last.volume0.Add(last.volume0, c.volume0)
		last.volume1.Add(last.volume1, c.volume1)
		last.trades += c.trades
		return series
	}
	c = c.clone()
	c.openTime = openTime
	series = append(series, c)
	if len(series) > a.cfg.MaxCandles {
		series = series[len(series)-a.cfg.MaxCandles:]
	}
	return series
}

// Klines 返回指定池子、周期最近的 limit 根 K 线，按时间升序
func (a *Aggregator) Klines(pool, interval string, limit int) ([]Candle, error) {
	idx := -1
	for i, iv := range intervals {
		if iv.name == interval {
			idx = i
		}
	}
	if idx < 0 {
		return nil, ErrInvalidInterval
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	p, ok := a.pools[pool]
	if !ok {
		return []Candle{}, nil
	}
	series := p.candles[idx]
	if limit <= 0 || limit > len(series) {
		limit = len(series)
	}
	out := make([]Candle, 0, limit)
	for _, c := range series[len(series)-limit:] {
		out = append(out, c.export())
	}
	return out, nil
}

// Trades 返回指定池子最近的 limit 笔成交，最新的在前
func (a *Aggregator) Trades(pool string, limit int) []Trade {
	a.mu.RLock()
	defer a.mu.RUnlock()
	p, ok := a.pools[pool]
	if !ok {
		return []Trade{}
	}
	trades := tail(p.trades, limit)
	for i, j := 0, len(trades)-1; i < j; i, j = i+1, j-1 {
		trades[i], trades[j] = trades[j], trades[i]
	}
	return trades
}

// tail 返回切片最后 limit 个元素的拷贝
func tail[T any](s []T, limit int) []T {
	if limit <= 0 || limit > len(s) {
		limit = len(s)
	}
	return append([]T{}, s[len(s)-limit:]...)
}

func bucket(ts, secs int64) int64 {
	return ts - ts%secs
}
