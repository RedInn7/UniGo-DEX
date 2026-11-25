package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"unigo-dex/arb"
	"unigo-dex/cache"
	"unigo-dex/chain"
	"unigo-dex/eip712"
	"unigo-dex/gas"
	"unigo-dex/indexer"
	"unigo-dex/kline"
	umath "unigo-dex/math"
	"unigo-dex/metrics"
	"unigo-dex/mev"
	"unigo-dex/oracle"
	"unigo-dex/router"
	"unigo-dex/ws"
)

// server.go — REST API 服务器
//
// API 端点：
//   GET  /api/v1/quote         — 获取交易报价
//   POST /api/v1/swap          — 提交交易（含 EIP-712 签名验证）
//   GET  /api/v1/pools         — 获取所有池子信息
//   GET  /api/v1/pool/:address — 获取单个池子详情
//   GET  /api/v1/status        — 系统状态（索引器、路由器、保护模式）
//   GET  /api/v1/tx/{hash}     — 交易状态（pending / pending_confirmation / confirmed / reverted）
//   GET  /api/v1/klines        — K 线（pool, interval, limit）
//   GET  /api/v1/trades        — 最近成交（pool, limit）
//   GET  /api/v1/mev/alerts    — 三明治攻击告警
//   POST /api/v1/swap/build    — 把路由编码成待签名交易（Manager 调用数据）
//   GET  /api/v1/nonce/{address} — EIP-712 签名用的 nonce
//   GET  /api/v1/twap          — 池子的 TWAP 与现价偏离
//   GET  /api/v1/arbitrage/opportunities — 套利机会与调用数据
//   GET  /api/v1/stream        — SSE 实时推送（成交 / K 线 / MEV 告警 / 套利机会）
//   GET  /metrics              — Prometheus 指标
//
// 架构特点：
//   - 标准库 net/http（无外部依赖）
//   - 中间件链：CORS → Logging → RateLimit → ConcurrencyLimit → Handler
//   - 所有响应使用 JSON 格式
//   - 错误统一通过 ErrorResponse 返回

// ServerConfig API 服务器配置
type ServerConfig struct {
	Port           int
	MaxConcurrent  int // 最大并发请求数，0 = 不限
	RateLimitPerIP int // 每 IP 每秒请求限制，0 = 不限

	DisableAccessLog bool // 关闭逐请求的访问日志
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration

	ManagerAddress string // UniGoManager 地址：待签名交易的目标，也是 EIP-712 的 verifyingContract
	QuoterAddress  string // UniGoQuoter 地址：配置后对链下报价做链上一致性抽检

	TWAPWindow    uint32 // TWAP 窗口（秒）
	TWAPMinWindow uint32 // 观测不足时可接受的最短窗口（秒），再短就视为 TWAP 不可用
}

// DefaultServerConfig 返回默认配置
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Port:           8080,
		MaxConcurrent:  500,
		RateLimitPerIP: 100,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   30 * time.Second,
		TWAPWindow:     600,
		TWAPMinWindow:  60,
	}
}

// Deps API 服务器依赖的各个模块
type Deps struct {
	Router   *router.Router
	State    *indexer.PoolStateManager
	Indexer  *indexer.Indexer
	Cache    *cache.Cache
	Oracle   *oracle.Oracle
	Chain    *chain.Client // 未连接节点时为 nil
	Gas      *gas.Estimator
	Klines   *kline.Aggregator
	Guard    *mev.Guard
	Arb      *arb.Scanner
	Hub      *ws.Hub
	Verifier *eip712.Verifier
	// Chainlink 喂价读取器；nil 表示没有配置任何喂价
	Chainlink *oracle.ChainlinkReader
}

// Server API 服务器
type Server struct {
	config     ServerConfig
	router     *router.Router
	state      *indexer.PoolStateManager
	idxer      *indexer.Indexer
	cache      *cache.Cache
	oracle     *oracle.Oracle
	chain      *chain.Client // 未连接节点时为 nil
	gas        *gas.Estimator
	klines     *kline.Aggregator
	guard      *mev.Guard
	arb        *arb.Scanner
	hub        *ws.Hub
	verifier   *eip712.Verifier
	chainlink  *oracle.ChainlinkReader
	httpServer *http.Server
}

// NewServer 创建 API 服务器
func NewServer(config ServerConfig, deps Deps) *Server {
	return &Server{
		config:    config,
		router:    deps.Router,
		state:     deps.State,
		idxer:     deps.Indexer,
		cache:     deps.Cache,
		oracle:    deps.Oracle,
		chain:     deps.Chain,
		gas:       deps.Gas,
		klines:    deps.Klines,
		guard:     deps.Guard,
		arb:       deps.Arb,
		hub:       deps.Hub,
		verifier:  deps.Verifier,
		chainlink: deps.Chainlink,
	}
}

// Handler 构建路由和中间件链
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 注册路由
	mux.HandleFunc("/api/v1/quote", s.handleQuote)
	mux.HandleFunc("/api/v1/swap", s.handleSwap)
	mux.HandleFunc("POST /api/v1/swap/build", s.handleBuildSwap)
	mux.HandleFunc("GET /api/v1/nonce/{address}", s.handleNonce)
	mux.HandleFunc("GET /api/v1/twap", s.handleTWAP)
	mux.HandleFunc("GET /api/v1/arbitrage/opportunities", s.handleArbitrage)
	mux.Handle("GET /api/v1/stream", s.hub.SSEHandler())
	mux.HandleFunc("/api/v1/pools", s.handlePools)
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/tx/{hash}", s.handleTx)
	mux.HandleFunc("GET /api/v1/klines", s.handleKlines)
	mux.HandleFunc("GET /api/v1/trades", s.handleTrades)
	mux.HandleFunc("GET /api/v1/mev/alerts", s.handleMEVAlerts)
	mux.Handle("GET /metrics", metrics.DefaultRegistry.Handler())
	mux.HandleFunc("/health", s.handleHealth)

	// 构建中间件链：CORS → Logging → RateLimit → ConcurrencyLimit → Handler
	// 两个限制都可以用 0 关闭（压测吞吐上限、或由前置网关负责限流时使用）
	var handler http.Handler = mux
	if s.config.MaxConcurrent > 0 {
		handler = NewConcurrencyLimiter(s.config.MaxConcurrent).Middleware(handler)
	}
	if n := s.config.RateLimitPerIP; n > 0 {
		// 桶容量 n、每 1/n 秒补 1 个 token，即每 IP 每秒 n 个请求
		handler = NewRateLimiter(n, time.Second/time.Duration(n)).Middleware(handler)
	}
	if !s.config.DisableAccessLog {
		handler = LoggingMiddleware(handler)
	}
	return CORSMiddleware(handler)
}

// Start 启动 HTTP 服务器
func (s *Server) Start() error {
	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.config.Port),
		Handler:      s.Handler(),
		ReadTimeout:  s.config.ReadTimeout,
		WriteTimeout: s.config.WriteTimeout,
	}

	log.Printf("[API] 服务器启动在 :%d", s.config.Port)
	return s.httpServer.ListenAndServe()
}

// Shutdown 优雅关闭
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// ===== Handler 实现 =====

// handleQuote 报价接口
//
// 流程：
//  1. 解析请求参数
//  2. 检查缓存（Singleflight 防击穿）
//  3. 调用路由引擎查找最优路径（逐 tick 模拟，与链上 Quoter 一致）
//  4. 价格安全检查：TWAP 操纵校验 + 预言机保护模式
//  5. MEV 保护：价格冲击、推荐滑点与 min_amount_out
//  6. Gas 成本评估
//  7. 返回报价结果
func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "only GET allowed")
		return
	}

	q := r.URL.Query()
	tokenIn := strings.ToLower(q.Get("token_in"))
	tokenOut := strings.ToLower(q.Get("token_out"))
	amountInStr := q.Get("amount_in")

	if tokenIn == "" || tokenOut == "" || amountInStr == "" {
		writeError(w, http.StatusBadRequest, "missing required params: token_in, token_out, amount_in")
		return
	}

	amountIn, ok := new(big.Int).SetString(amountInStr, 10)
	if !ok || amountIn.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "invalid amount_in")
		return
	}

	// slippage 可选，缺省时用默认滑点
	var slippage float64
	if v := q.Get("slippage"); v != "" {
		var err error
		if slippage, err = strconv.ParseFloat(v, 64); err != nil || slippage < 0 {
			writeError(w, http.StatusBadRequest, "invalid slippage")
			return
		}
	}

	route, err := s.findRoute(r.Context(), tokenIn, tokenOut, amountIn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "route search failed: "+err.Error())
		return
	}

	// 价格安全检查：报价只给警告，不拒绝
	safety := s.checkPriceSafety(r.Context(), route, 0.005)

	// MEV 保护
	slippage, mevWarning := s.guard.RecommendSlippage(poolAddresses(route), slippage, time.Now())

	resp := QuoteResponse{
		AmountOut:    route.AmountOut.String(),
		MinAmountOut: mev.MinAmountOut(route.AmountOut, slippage).String(),
		Slippage:     slippage,
		Path:         route.Path,
		Fees:         route.Fees(),
		PriceImpact:  mev.PriceImpact(route.Pools, tokenIn, amountIn, route.AmountOut),
		GasEstimate:  route.GasEstimate,
		Gas:          s.gas.Estimate(r.Context(), route.GasEstimate),
		TicksCrossed: route.TicksCrossed,
		IsDegraded:   route.IsDegraded,
		PriceChecks:  safety.checks,
		Warning:      joinWarnings(safety.warning, mevWarning),
	}

	writeJSON(w, http.StatusOK, resp)
}

// findRoute 查找路由，结果经缓存（Singleflight 防击穿）
//
// 缓存 key 带上索引器的最新区块号：池子状态只在新区块到来时变化，
// 同一区块内重复报价直接命中缓存，出了新区块自动失效，不会返回过期报价。
func (s *Server) findRoute(ctx context.Context, tokenIn, tokenOut string, amountIn *big.Int) (*router.Route, error) {
	cacheKey := fmt.Sprintf("quote:%s:%s:%s:%d", tokenIn, tokenOut, amountIn.String(), s.idxer.GetStats().LastBlockNumber)
	result, err := s.cache.GetOrLoad(cacheKey, func() (interface{}, error) {
		// 缓存未命中，执行路由搜索
		return s.router.FindBestRoute(ctx, tokenIn, tokenOut, amountIn)
	})
	if err != nil {
		return nil, err
	}
	return result.(*router.Route), nil
}

// handleSwap 提交交易接口
//
// 流程：
//  1. 解析请求
//  2. 验证 EIP-712 签名（域分隔符 + nonce + deadline 三层防重放），确认请求确实出自 owner
//  3. 价格安全检查：现价偏离 TWAP（疑似被操纵）或保护模式下滑点不合规时拒绝
//  4. MEV 交易前保护：价格冲击过大、deadline 已过期或过长、min_amount_out 过松时拒绝
//  5. 请求带有已签名交易（raw_tx）时广播到链上，返回真实交易哈希；
//     否则只返回检查结果，由客户端自行提交
//
// 后端不托管私钥，不会替用户签名。raw_tx 必须由 owner 签名、发给 Manager 合约。
func (s *Server) handleSwap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "only POST allowed")
		return
	}

	var req SwapRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tokenIn, tokenOut := strings.ToLower(req.TokenIn), strings.ToLower(req.TokenOut)
	amountIn, ok := new(big.Int).SetString(req.AmountIn, 10)
	if !ok || amountIn.Sign() <= 0 {
		writeError(w, http.StatusBadRequest, "invalid amount_in")
		return
	}
	minAmountOut, ok := new(big.Int).SetString(req.MinAmountOut, 10)
	if !ok || minAmountOut.Sign() < 0 {
		writeError(w, http.StatusBadRequest, "invalid min_amount_out")
		return
	}

	// EIP-712 验签
	sig, err := eip712.ParseSignature(req.Signature)
	if err == nil {
		err = s.verifier.Verify(&eip712.SwapPermit{
			Owner:        req.Owner,
			TokenIn:      req.TokenIn,
			TokenOut:     req.TokenOut,
			AmountIn:     req.AmountIn,
			MinAmountOut: req.MinAmountOut,
			Nonce:        req.Nonce,
			Deadline:     req.Deadline,
		}, sig)
	}
	if err != nil {
		metrics.SwapSignatureRejected.Inc()
		writeError(w, http.StatusUnauthorized, "signature rejected: "+err.Error())
		return
	}

	// 没有报价就无法评估价格冲击，不放行
	route, err := s.findRoute(r.Context(), tokenIn, tokenOut, amountIn)
	if err != nil {
		writeError(w, http.StatusBadRequest, "route search failed: "+err.Error())
		return
	}

	// 价格安全检查
	safety := s.checkPriceSafety(r.Context(), route, req.Slippage)
	if safety.rejected {
		writeError(w, http.StatusForbidden, safety.warning)
		return
	}

	// MEV 交易前保护
	now := time.Now()
	impact := mev.PriceImpact(route.Pools, tokenIn, amountIn, route.AmountOut)
	if err := s.guard.CheckSwap(impact, req.Deadline, now); err != nil {
		writeError(w, http.StatusBadRequest, "swap rejected: "+err.Error())
		return
	}
	if err := s.guard.CheckMinAmountOut(minAmountOut, route.AmountOut); err != nil {
		writeError(w, http.StatusBadRequest, "swap rejected: "+err.Error())
		return
	}
	slippage, mevWarning := s.guard.RecommendSlippage(poolAddresses(route), req.Slippage, now)

	resp := SwapResponse{
		Status:       "validated",
		PriceImpact:  impact,
		MinAmountOut: mev.MinAmountOut(route.AmountOut, slippage).String(),
		Warning:      joinWarnings(safety.warning, mevWarning),
	}

	if req.RawTx != "" {
		if s.chain == nil {
			writeError(w, http.StatusServiceUnavailable, "chain node not configured, cannot broadcast raw_tx")
			return
		}
		// 只转发 owner 自己签名、发给 Manager 的交易
		info, err := chain.InspectRawTx(req.RawTx)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if info.From != strings.ToLower(req.Owner) {
			writeError(w, http.StatusForbidden, "raw_tx is not signed by owner")
			return
		}
		if s.config.ManagerAddress != "" && info.To != strings.ToLower(s.config.ManagerAddress) {
			writeError(w, http.StatusForbidden, "raw_tx is not addressed to the Manager contract")
			return
		}
		txHash, err := s.chain.SendRawTransaction(r.Context(), req.RawTx)
		if err != nil {
			writeError(w, http.StatusBadGateway, "broadcast failed: "+err.Error())
			return
		}
		resp.TxHash = txHash
		resp.Status = chain.TxPending
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleTx 交易状态查询
//
// 按回执和确认数返回 pending / pending_confirmation / confirmed / reverted，
// 确认数阈值与索引器的延迟确认配置一致。
func (s *Server) handleTx(w http.ResponseWriter, r *http.Request) {
	if s.chain == nil {
		writeError(w, http.StatusServiceUnavailable, "chain node not configured")
		return
	}

	status, err := s.chain.TxStatus(r.Context(), r.PathValue("hash"), s.idxer.ConfirmationBlocks())
	switch {
	case errors.Is(err, chain.ErrInvalidHash):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, chain.ErrNotFound):
		writeError(w, http.StatusNotFound, "transaction not found")
	case err != nil:
		writeError(w, http.StatusBadGateway, "chain query failed: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, status)
	}
}

// handleKlines K 线查询：pool 必填，interval 默认 1m，limit 默认 100（最大 1000）
func (s *Server) handleKlines(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pool := strings.ToLower(q.Get("pool"))
	if pool == "" {
		writeError(w, http.StatusBadRequest, "missing required param: pool")
		return
	}
	interval := q.Get("interval")
	if interval == "" {
		interval = "1m"
	}
	limit, ok := parseLimit(q.Get("limit"), 100)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}

	candles, err := s.klines.Klines(pool, interval, limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, candles)
}

// handleTrades 最近成交查询：pool 必填，limit 默认 50（最大 1000）
func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pool := strings.ToLower(q.Get("pool"))
	if pool == "" {
		writeError(w, http.StatusBadRequest, "missing required param: pool")
		return
	}
	limit, ok := parseLimit(q.Get("limit"), 50)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	writeJSON(w, http.StatusOK, s.klines.Trades(pool, limit))
}

// handleMEVAlerts 三明治攻击告警：limit 默认 100（最大 1000），最新的在前
func (s *Server) handleMEVAlerts(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(r.URL.Query().Get("limit"), 100)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	writeJSON(w, http.StatusOK, s.guard.Alerts(limit))
}

// handlePools 获取所有池子信息
func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "only GET allowed")
		return
	}

	snapshots := s.state.GetAllPools()
	pools := make([]PoolInfo, 0, len(snapshots))

	for _, snap := range snapshots {
		pools = append(pools, PoolInfo{
			Address:     snap.Address,
			Token0:      snap.Token0,
			Token1:      snap.Token1,
			Fee:         snap.Fee,
			TickSpacing: snap.TickSpacing,
			Price:       umath.SqrtPriceX96ToPrice(snap.SqrtPriceX96),
			Tick:        snap.Tick,
			Liquidity:   snap.Liquidity.String(),
		})
	}

	writeJSON(w, http.StatusOK, pools)
}

// handleStatus 系统状态接口
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "only GET allowed")
		return
	}

	idxStats := s.idxer.GetStats()
	routerStats := s.router.GetStats()

	status := SystemStatus{
		IndexerBlocksProcessed: idxStats.BlocksProcessed,
		IndexerLastBlock:       idxStats.LastBlockNumber,
		IndexerReorgsDetected:  idxStats.ReorgsDetected,
		RouterTotalRequests:    routerStats.TotalRequests,
		RouterDegradedRequests: routerStats.DegradedRequests,
		RouterAvgLatencyMs:     routerStats.AvgLatencyMs,
		CacheEnabled:           true,
	}

	writeJSON(w, http.StatusOK, status)
}

// handleHealth 健康检查
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ===== 辅助函数 =====

// decodeJSON 解析请求体（限制 1MB），失败时写 400 并返回 false
func decodeJSON(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// parseLimit 解析 limit 参数：缺省用 def，范围 1~1000
func parseLimit(v string, def int) (int, bool) {
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, false
	}
	return min(n, 1000), true
}

// poolAddresses 路由经过的池子地址
func poolAddresses(route *router.Route) []string {
	addrs := make([]string, len(route.Pools))
	for i, p := range route.Pools {
		addrs[i] = p.Address
	}
	return addrs
}

// joinWarnings 合并多条警告
func joinWarnings(warnings ...string) string {
	var parts []string
	for _, w := range warnings {
		if w != "" {
			parts = append(parts, w)
		}
	}
	return strings.Join(parts, " ")
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{
		Error: msg,
		Code:  status,
	})
}
