package oracle

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"unigo-dex/chain"
)

// chainlink.go — 读取链上 Chainlink 喂价
//
// 通过 AggregatorV3Interface 的 latestRoundData() 和 decimals() 读取喂价，
// 校验有效性后换算成与池子价格同口径的数，交给三方交叉验证使用。
//
// === 有效性检查 ===
//
// latestRoundData() 永远会返回一组数，但这组数不一定能用：
//   - answer <= 0：聚合器出错或被清零
//   - updatedAt == 0：这一轮还没完成
//   - answeredInRound < roundId：答案是从更早的轮次沿用过来的
//   - updatedAt 在最新区块之后：时间戳不可信
//   - 最新区块时间 - updatedAt > 心跳阈值：喂价已经停更（过期）
//
// 任何一项不通过，这个喂价就视为不可用：三方校验降级为"现价 vs TWAP"两方，
// 并在响应里说明原因，绝不拿坏数据去比较。过期判断用链上的区块时间，不用本机时钟。
//
// === 价格口径换算（最容易错的地方）===
//
// 喂价是"1 个 base 值多少 quote"，按人类单位计，再乘以 10^feedDecimals：
//
//	P = answer / 10^feedDecimals        例：ETH/USD answer=200000000000, decimals=8 → P = 2000
//
// 池子价格是 token1 最小单位 / token0 最小单位（由 sqrtPriceX96 换算，未按 decimals 调整）。
//
//	base 是 token0、quote 是 token1：poolPrice = P × 10^(quoteDecimals - baseDecimals)
//	base 是 token1、quote 是 token0：poolPrice = (1/P) × 10^(baseDecimals - quoteDecimals)
//
// 例：WETH(18 位) / USDC(6 位)，P = 2000
//   - WETH 是 token0：1 wei WETH 值 2000 × 10^(6-18) = 2e-9 个 USDC 最小单位
//   - USDC 是 token0：1 个 USDC 最小单位值 (1/2000) × 10^(18-6) = 5e8 wei WETH
//
// === 缓存与刷新 ===
//
// 每个喂价的读数缓存 RefreshInterval（默认 15 秒，约一个区块），过期后由下一次查询触发刷新，
// 同一个喂价同时只有一个刷新请求在途。decimals 不会变，只读一次。RPC 经过 chain 包的断路器。

// FeedConfig 一个币对的喂价配置
type FeedConfig struct {
	Base          string        // base token 地址（小写）：喂价报的是 1 个 base 的价格
	Quote         string        // quote token 地址（小写）：价格以它计
	Address       string        // 聚合器合约地址
	BaseDecimals  uint8         // base token 的 decimals
	QuoteDecimals uint8         // quote token 的 decimals
	Heartbeat     time.Duration // 心跳阈值：超过这么久没更新视为过期
}

// DefaultHeartbeat 配置里没写心跳时的默认值
const DefaultHeartbeat = time.Hour

// ParseFeeds 解析喂价配置
//
// 格式：多个喂价用逗号分隔，每个喂价的字段用冒号分隔：
//
//	base:quote:聚合器地址:baseDecimals:quoteDecimals[:心跳秒数]
//
// 例（WETH/USDC 用 ETH/USD 喂价，心跳 1 小时）：
//
//	0xC02a...6Cc2:0xA0b8...eB48:0x5f4e...8419:18:6:3600
func ParseFeeds(spec string) ([]FeedConfig, error) {
	var feeds []FeedConfig
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		fields := strings.Split(entry, ":")
		if len(fields) != 5 && len(fields) != 6 {
			return nil, fmt.Errorf("喂价配置 %q 应为 base:quote:feed:baseDecimals:quoteDecimals[:heartbeat]", entry)
		}
		for _, addr := range fields[:3] {
			if !isAddress(addr) {
				return nil, fmt.Errorf("喂价配置 %q 里的 %q 不是合法地址", entry, addr)
			}
		}
		baseDecimals, err1 := strconv.ParseUint(fields[3], 10, 8)
		quoteDecimals, err2 := strconv.ParseUint(fields[4], 10, 8)
		if err1 != nil || err2 != nil || baseDecimals > 36 || quoteDecimals > 36 {
			return nil, fmt.Errorf("喂价配置 %q 的 decimals 不合法", entry)
		}
		feed := FeedConfig{
			Base:          strings.ToLower(fields[0]),
			Quote:         strings.ToLower(fields[1]),
			Address:       strings.ToLower(fields[2]),
			BaseDecimals:  uint8(baseDecimals),
			QuoteDecimals: uint8(quoteDecimals),
			Heartbeat:     DefaultHeartbeat,
		}
		if feed.Base == feed.Quote {
			return nil, fmt.Errorf("喂价配置 %q 的 base 和 quote 相同", entry)
		}
		if len(fields) == 6 {
			seconds, err := strconv.ParseUint(fields[5], 10, 32)
			if err != nil || seconds == 0 {
				return nil, fmt.Errorf("喂价配置 %q 的心跳秒数不合法", entry)
			}
			feed.Heartbeat = time.Duration(seconds) * time.Second
		}
		feeds = append(feeds, feed)
	}
	return feeds, nil
}

func isAddress(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, c := range s[2:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// FeedSource 读取聚合器的底层调用（chain.Client 实现了它）
type FeedSource interface {
	ChainlinkDecimals(ctx context.Context, feed string) (uint8, error)
	ChainlinkLatestRound(ctx context.Context, feed string) (*chain.ChainlinkRound, error)
}

// 喂价读数的状态
const (
	ChainlinkOK          = "ok"          // 喂价有效
	ChainlinkDeviated    = "deviated"    // 喂价有效，但池内现价偏离它过大
	ChainlinkUnavailable = "unavailable" // 配了喂价但当前不可用（过期、轮次未完成、节点查询失败等）
	ChainlinkNone        = "none"        // 这个币对没有配置喂价
)

// FeedReading 一次喂价读数，价格已换算成池子口径（token1 最小单位 / token0 最小单位）
type FeedReading struct {
	Status    string
	Price     float64
	UpdatedAt uint64 // 喂价的更新时间（Unix 秒）
	Detail    string // 不可用时的原因
}

// ValidateRound 检查一轮喂价数据是否可用，不可用时返回原因
func ValidateRound(round *chain.ChainlinkRound, heartbeat time.Duration) error {
	switch {
	case round.Answer == nil || round.Answer.Sign() <= 0:
		return errors.New("answer 不是正数")
	case round.UpdatedAt == 0:
		return errors.New("轮次未完成（updatedAt = 0）")
	case round.AnsweredInRound.Cmp(round.RoundID) < 0:
		return fmt.Errorf("答案来自旧轮次（answeredInRound %s < roundId %s）", round.AnsweredInRound, round.RoundID)
	case round.UpdatedAt > round.BlockTime:
		return errors.New("updatedAt 晚于最新区块时间")
	case round.BlockTime-round.UpdatedAt > uint64(heartbeat/time.Second):
		return fmt.Errorf("喂价已过期：%d 秒未更新（心跳阈值 %d 秒）",
			round.BlockTime-round.UpdatedAt, uint64(heartbeat/time.Second))
	}
	return nil
}

// PoolPrice 把喂价（1 个 base 值多少 quote，人类单位）换算成池子口径的价格
//
// token0 是池子的 token0 地址（小写）。
func (f FeedConfig) PoolPrice(humanPrice float64, token0 string) float64 {
	if token0 == f.Base {
		return humanPrice * math.Pow10(int(f.QuoteDecimals)-int(f.BaseDecimals))
	}
	return 1 / humanPrice * math.Pow10(int(f.BaseDecimals)-int(f.QuoteDecimals))
}

// feedState 一个喂价的缓存
type feedState struct {
	cfg FeedConfig

	mu        sync.Mutex // 同时只允许一个刷新在途，其余查询等它
	decimals  *uint8     // 只读一次
	fetchedAt time.Time
	human     float64 // 1 个 base 值多少 quote
	updatedAt uint64
	invalid   string // 非空表示上次读到的数据不可用及原因
}

// ChainlinkReader 按币对读取 Chainlink 喂价，带缓存
type ChainlinkReader struct {
	src     FeedSource
	refresh time.Duration
	feeds   map[string]*feedState // key: 两个 token 地址按字典序拼接
}

// DefaultRefreshInterval 喂价读数的默认缓存时间
const DefaultRefreshInterval = 15 * time.Second

// NewChainlinkReader 创建喂价读取器；refresh 是读数的缓存时间
func NewChainlinkReader(src FeedSource, feeds []FeedConfig, refresh time.Duration) *ChainlinkReader {
	r := &ChainlinkReader{src: src, refresh: refresh, feeds: make(map[string]*feedState)}
	for _, cfg := range feeds {
		r.feeds[feedKey(cfg.Base, cfg.Quote)] = &feedState{cfg: cfg}
	}
	return r
}

func feedKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// Read 返回 token0/token1 这个币对的喂价读数（池子口径）
func (r *ChainlinkReader) Read(ctx context.Context, token0, token1 string) FeedReading {
	if r == nil {
		return FeedReading{Status: ChainlinkNone}
	}
	state, ok := r.feeds[feedKey(token0, token1)]
	if !ok {
		return FeedReading{Status: ChainlinkNone}
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.fetchedAt.IsZero() || time.Since(state.fetchedAt) >= r.refresh {
		if err := r.fetch(ctx, state); err != nil {
			// 节点查询失败：不缓存，也不沿用旧读数（旧读数是否还在心跳内已无法确认）
			state.fetchedAt = time.Time{}
			return FeedReading{Status: ChainlinkUnavailable, Detail: "节点查询失败: " + err.Error()}
		}
	}
	if state.invalid != "" {
		return FeedReading{Status: ChainlinkUnavailable, Detail: state.invalid, UpdatedAt: state.updatedAt}
	}
	return FeedReading{
		Status:    ChainlinkOK,
		Price:     state.cfg.PoolPrice(state.human, token0),
		UpdatedAt: state.updatedAt,
	}
}

// fetch 从链上刷新一个喂价（调用方持有 state.mu）
func (r *ChainlinkReader) fetch(ctx context.Context, state *feedState) error {
	if state.decimals == nil {
		decimals, err := r.src.ChainlinkDecimals(ctx, state.cfg.Address)
		if err != nil {
			return err
		}
		state.decimals = &decimals
	}
	round, err := r.src.ChainlinkLatestRound(ctx, state.cfg.Address)
	if err != nil {
		return err
	}

	state.fetchedAt = time.Now()
	state.updatedAt = round.UpdatedAt
	state.invalid = ""
	if err := ValidateRound(round, state.cfg.Heartbeat); err != nil {
		state.invalid = err.Error()
		return nil
	}
	// human = answer / 10^decimals
	scale := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(*state.decimals)), nil))
	state.human, _ = new(big.Float).Quo(new(big.Float).SetInt(round.Answer), scale).Float64()
	return nil
}
