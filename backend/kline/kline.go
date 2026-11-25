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
//   - 成交量：amount0 / amount1 的绝对值
//   - 时间：区块时间戳
//
// === Reorg 重算 ===
//
// 区块时间戳随区块号单调递增，所以回滚区块 >= forkBlock 时：
//   - 第一笔被回滚成交所在的那一分钟之后的 K 线，里面全是被回滚的成交，直接删除
//   - 那一分钟本身可能还有未被回滚的成交，用幸存成交重算这根 1m K 线
//   - 更高周期受影响的那一根，用 1m K 线重新合并
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
	// ponytail: 成交量用 float64 累加，画图精度足够；需要精确到 wei 时改成 big.Int
	Volume0 float64 `json:"volume0"` // token0 成交量（最小单位）
	Volume1 float64 `json:"volume1"` // token1 成交量（最小单位）
	Trades  int     `json:"trades"`  // 成交笔数
}

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
	candles [][]Candle // 下标与 intervals 对应，按 OpenTime 升序
}

// Aggregator K 线聚合器，实现 indexer.SwapListener
type Aggregator struct {
	cfg   Config
	mu    sync.RWMutex
	pools map[string]*poolData
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

	a.mu.Lock()
	defer a.mu.Unlock()

	p, ok := a.pools[e.PoolAddress]
	if !ok {
		if len(a.pools) >= a.cfg.MaxPools {
			return
		}
		p = &poolData{candles: make([][]Candle, len(intervals))}
		a.pools[e.PoolAddress] = p
	}
	// 区块时间戳单调递增；时间倒退的成交不可能来自一条合法的链，丢弃
	if n := len(p.trades); n > 0 && trade.Timestamp < p.trades[n-1].Timestamp {
		return
	}

	p.trades = append(p.trades, trade)
	if len(p.trades) > a.cfg.MaxTrades {
		p.trades = p.trades[len(p.trades)-a.cfg.MaxTrades:]
	}

	c := tradeCandle(trade, absFloat(e.Amount0), absFloat(e.Amount1))
	for i, iv := range intervals {
		p.candles[i] = a.merge(p.candles[i], iv.secs, c)
	}
}

// OnReorg 丢弃区块号 >= forkBlock 的成交，并重算受影响的 K 线
func (a *Aggregator) OnReorg(forkBlock uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.pools {
		a.rollback(p, forkBlock)
	}
}

func (a *Aggregator) rollback(p *poolData, forkBlock uint64) {
	first := sort.Search(len(p.trades), func(i int) bool { return p.trades[i].BlockNumber >= forkBlock })
	if first == len(p.trades) {
		return
	}
	// 第一笔被回滚成交所在的分钟：此后的 K 线全部作废
	minute := bucket(p.trades[first].Timestamp, 60)
	p.trades = p.trades[:first]

	for i, iv := range intervals {
		start := bucket(minute, iv.secs)
		series := p.candles[i]
		p.candles[i] = series[:sort.Search(len(series), func(j int) bool { return series[j].OpenTime >= start })]
	}

	// 1m：用这一分钟内幸存的成交重算
	// ponytail: 这一分钟开始以来的成交若超过 MaxTrades，早期成交已被淘汰，重算出的这根 1m 会缺数据；
	// 需要时把成交的保留策略改成"回滚窗口内不淘汰"
	from := sort.Search(len(p.trades), func(i int) bool { return p.trades[i].Timestamp >= minute })
	for _, t := range p.trades[from:] {
		amount0, _ := new(big.Int).SetString(t.Amount0, 10)
		amount1, _ := new(big.Int).SetString(t.Amount1, 10)
		p.candles[0] = a.merge(p.candles[0], 60, tradeCandle(t, absFloat(amount0), absFloat(amount1)))
	}

	// 更高周期：受影响的那一根由 1m K 线重新合并
	oneMin := p.candles[0]
	for i := 1; i < len(intervals); i++ {
		start := bucket(minute, intervals[i].secs)
		from := sort.Search(len(oneMin), func(j int) bool { return oneMin[j].OpenTime >= start })
		for _, c := range oneMin[from:] {
			p.candles[i] = a.merge(p.candles[i], intervals[i].secs, c)
		}
	}
}

// merge 把 c（一笔成交或一根更小周期的 K 线）并入 series 的最后一根，或追加新的一根
func (a *Aggregator) merge(series []Candle, secs int64, c Candle) []Candle {
	c.OpenTime = bucket(c.OpenTime, secs)
	if n := len(series); n > 0 && series[n-1].OpenTime == c.OpenTime {
		last := &series[n-1]
		last.High = max(last.High, c.High)
		last.Low = min(last.Low, c.Low)
		last.Close = c.Close
		last.Volume0 += c.Volume0
		last.Volume1 += c.Volume1
		last.Trades += c.Trades
		return series
	}
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
	return tail(p.candles[idx], limit), nil
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

func tradeCandle(t Trade, volume0, volume1 float64) Candle {
	return Candle{
		OpenTime: t.Timestamp,
		Open:     t.Price, High: t.Price, Low: t.Price, Close: t.Price,
		Volume0: volume0, Volume1: volume1,
		Trades: 1,
	}
}

func absFloat(v *big.Int) float64 {
	f, _ := new(big.Float).SetInt(new(big.Int).Abs(v)).Float64()
	return f
}
