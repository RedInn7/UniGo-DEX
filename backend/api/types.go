package api

import (
	"unigo-dex/gas"
	"unigo-dex/kline"
)

// types.go — API 请求/响应类型定义

// QuoteRequest 报价请求
type QuoteRequest struct {
	TokenIn  string `json:"token_in"`
	TokenOut string `json:"token_out"`
	AmountIn string `json:"amount_in"` // 字符串避免 JSON 精度丢失
}

// QuoteResponse 报价响应
type QuoteResponse struct {
	AmountOut    string       `json:"amount_out"`
	MinAmountOut string       `json:"min_amount_out"` // 按推荐滑点算出的最少应得数量
	Slippage     float64      `json:"slippage"`       // 推荐滑点（池子近期被夹过时会收紧）
	Path         []string     `json:"path"`
	Fees         []uint32     `json:"fees"`         // 路径上每一跳的费率
	PriceImpact  float64      `json:"price_impact"` // 价格冲击，0.01 = 1%
	GasEstimate  uint64       `json:"gas_estimate"`
	Gas          gas.Estimate `json:"gas"` // EIP-1559 口径的 Gas 成本评估
	TicksCrossed int          `json:"ticks_crossed"`
	IsDegraded   bool         `json:"is_degraded"` // 是否为降级路由
	Warning      string       `json:"warning,omitempty"`
}

// SwapRequest Swap 请求（含 EIP-712 签名）
//
// 签名的类型化数据：
//
//	SwapPermit(address owner,address tokenIn,address tokenOut,uint256 amountIn,
//	           uint256 minAmountOut,uint256 nonce,uint256 deadline)
//
// 域：name="UniGo-DEX" version="1" chainId=链 ID verifyingContract=Manager 地址。
// nonce 从 GET /api/v1/nonce/{address} 获取。
type SwapRequest struct {
	Owner        string  `json:"owner"` // 签名者地址
	TokenIn      string  `json:"token_in"`
	TokenOut     string  `json:"token_out"`
	AmountIn     string  `json:"amount_in"`
	MinAmountOut string  `json:"min_amount_out"`
	Slippage     float64 `json:"slippage"`  // 0.005 = 0.5%
	Nonce        uint64  `json:"nonce"`     // 防重放 nonce
	Deadline     int64   `json:"deadline"`  // Unix timestamp
	Signature    string  `json:"signature"` // EIP-712 签名，65 字节十六进制
	RawTx        string  `json:"raw_tx"`    // 用户钱包签好名的交易（0x 十六进制），提供时由后端广播
}

// BuildSwapRequest 构造待签名交易的请求
//
// amount_in 与 amount_out 二选一：给 amount_in 是精确输入（可多跳），
// 给 amount_out 是精确输出（只走单池）。
type BuildSwapRequest struct {
	TokenIn   string  `json:"token_in"`
	TokenOut  string  `json:"token_out"`
	AmountIn  string  `json:"amount_in"`
	AmountOut string  `json:"amount_out"`
	Slippage  float64 `json:"slippage"`  // 缺省用默认滑点
	Deadline  int64   `json:"deadline"`  // Unix 秒，缺省为 10 分钟后
	Recipient string  `json:"recipient"` // 接收输出 token 的地址
	From      string  `json:"from"`      // 发送方地址（可选），提供时让节点估算 gas
}

// BuildSwapResponse 待签名交易
//
// 用户钱包对 {to, data, value, gas_limit} 签名后，经 POST /api/v1/swap 的 raw_tx 广播。
type BuildSwapResponse struct {
	To       string `json:"to"`       // Manager 合约地址
	Data     string `json:"data"`     // 调用数据（0x 十六进制）
	Value    string `json:"value"`    // 随交易发送的 ETH，恒为 "0"
	ChainID  uint64 `json:"chain_id"` // 未连接节点时为 0
	Method   string `json:"method"`   // swap / exactInput / exactOutputSingle
	GasLimit uint64 `json:"gas_limit"`
	// gas_limit 的来源：node = 节点 eth_estimateGas 加 20% 余量，router = 路由引擎的静态估算
	GasLimitSource string       `json:"gas_limit_source"`
	Gas            gas.Estimate `json:"gas"`

	AmountIn     string   `json:"amount_in"`
	AmountOut    string   `json:"amount_out"`
	MinAmountOut string   `json:"min_amount_out,omitempty"` // 精确输入：写进交易的最少应得数量
	MaxAmountIn  string   `json:"max_amount_in,omitempty"`  // 精确输出：写进交易的最多付出数量
	Path         []string `json:"path"`
	Fees         []uint32 `json:"fees"`
	Slippage     float64  `json:"slippage"`
	Deadline     int64    `json:"deadline"`
	PriceImpact  float64  `json:"price_impact"`

	// 链上 Quoter 对同一路径的报价，以及是否与链下报价逐位一致（配置了 Quoter 才有）
	OnchainAmountOut string `json:"onchain_amount_out,omitempty"`
	QuoteConsistent  *bool  `json:"quote_consistent,omitempty"`

	Warning string `json:"warning,omitempty"`
}

// KlineUpdate 实时推送的 K 线更新（SSE 的 kline 事件）
type KlineUpdate struct {
	Pool     string       `json:"pool"`
	Interval string       `json:"interval"`
	Candle   kline.Candle `json:"candle"`
}

// TWAPResponse 池子的 TWAP 与现价偏离
type TWAPResponse struct {
	Pool      string  `json:"pool"`
	Status    string  `json:"status"` // ok / deviated / unavailable
	Spot      float64 `json:"spot"`
	TWAP      float64 `json:"twap,omitempty"`
	Tick      int32   `json:"tick"`      // 窗口内的平均 tick
	Window    uint32  `json:"window"`    // 实际使用的窗口（秒）
	Degraded  bool    `json:"degraded"`  // 观测不足，窗口小于配置值
	Deviation float64 `json:"deviation"` // |现价 - TWAP| / TWAP
	Detail    string  `json:"detail,omitempty"`
}

// SwapResponse Swap 响应
type SwapResponse struct {
	TxHash       string  `json:"tx_hash,omitempty"` // 广播后的真实交易哈希，用 /api/v1/tx/{hash} 跟踪
	Status       string  `json:"status"`            // validated（仅通过检查，未广播）/ pending（已广播）
	PriceImpact  float64 `json:"price_impact"`
	MinAmountOut string  `json:"min_amount_out"` // 推荐的最少应得数量
	Warning      string  `json:"warning,omitempty"`
}

// PoolInfo 池子信息
type PoolInfo struct {
	Address     string  `json:"address"`
	Token0      string  `json:"token0"`
	Token1      string  `json:"token1"`
	Fee         uint32  `json:"fee"`
	TickSpacing int32   `json:"tick_spacing"`
	Price       float64 `json:"price"`
	Tick        int32   `json:"tick"`
	Liquidity   string  `json:"liquidity"`
}

// SystemStatus 系统状态
type SystemStatus struct {
	// 索引器状态
	IndexerBlocksProcessed uint64 `json:"indexer_blocks_processed"`
	IndexerLastBlock       uint64 `json:"indexer_last_block"`
	IndexerReorgsDetected  uint64 `json:"indexer_reorgs_detected"`

	// 路由器状态
	RouterTotalRequests    uint64  `json:"router_total_requests"`
	RouterDegradedRequests uint64  `json:"router_degraded_requests"`
	RouterAvgLatencyMs     float64 `json:"router_avg_latency_ms"`

	// 缓存状态
	CacheEnabled bool `json:"cache_enabled"`

	// 保护模式
	ProtectedPairs []string `json:"protected_pairs,omitempty"`
}

// ErrorResponse 错误响应
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    int    `json:"code"`
	Details string `json:"details,omitempty"`
}
