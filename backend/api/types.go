package api

// types.go — API 请求/响应类型定义

// QuoteRequest 报价请求
type QuoteRequest struct {
	TokenIn  string `json:"token_in"`
	TokenOut string `json:"token_out"`
	AmountIn string `json:"amount_in"` // 字符串避免 JSON 精度丢失
}

// QuoteResponse 报价响应
type QuoteResponse struct {
	AmountOut    string   `json:"amount_out"`
	Path         []string `json:"path"`
	PriceImpact  float64  `json:"price_impact"`
	GasEstimate  uint64   `json:"gas_estimate"`
	TicksCrossed int      `json:"ticks_crossed"`
	IsDegraded   bool     `json:"is_degraded"` // 是否为降级路由
	Warning      string   `json:"warning,omitempty"`
}

// SwapRequest Swap 请求（含 EIP-712 签名）
type SwapRequest struct {
	TokenIn      string `json:"token_in"`
	TokenOut     string `json:"token_out"`
	AmountIn     string `json:"amount_in"`
	MinAmountOut string `json:"min_amount_out"`
	Slippage     float64 `json:"slippage"` // 0.005 = 0.5%
	Deadline     int64  `json:"deadline"`  // Unix timestamp
	Signature    string `json:"signature"` // EIP-712 签名
}

// SwapResponse Swap 响应
type SwapResponse struct {
	TxHash  string `json:"tx_hash"`
	Status  string `json:"status"` // pending_confirmation / confirmed / rolled_back
	Warning string `json:"warning,omitempty"`
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
