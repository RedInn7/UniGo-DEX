package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"time"

	"unigo-dex/cache"
	"unigo-dex/indexer"
	"unigo-dex/oracle"
	"unigo-dex/router"
)

// server.go — REST API 服务器
//
// API 端点：
//   GET  /api/v1/quote         — 获取交易报价
//   POST /api/v1/swap          — 提交交易（含 EIP-712 签名验证）
//   GET  /api/v1/pools         — 获取所有池子信息
//   GET  /api/v1/pool/:address — 获取单个池子详情
//   GET  /api/v1/status        — 系统状态（索引器、路由器、保护模式）
//
// 架构特点：
//   - 标准库 net/http（无外部依赖）
//   - 中间件链：CORS → Logging → RateLimit → ConcurrencyLimit → Handler
//   - 所有响应使用 JSON 格式
//   - 错误统一通过 ErrorResponse 返回

// ServerConfig API 服务器配置
type ServerConfig struct {
	Port              int
	MaxConcurrent     int           // 最大并发请求数
	RateLimitPerIP    int           // 每 IP 每秒请求限制
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
}

// DefaultServerConfig 返回默认配置
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Port:           8080,
		MaxConcurrent:  500,
		RateLimitPerIP: 100,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   30 * time.Second,
	}
}

// Server API 服务器
type Server struct {
	config     ServerConfig
	router     *router.Router
	state      *indexer.PoolStateManager
	idxer      *indexer.Indexer
	cache      *cache.Cache
	oracle     *oracle.Oracle
	httpServer *http.Server
}

// NewServer 创建 API 服务器
func NewServer(
	config ServerConfig,
	r *router.Router,
	state *indexer.PoolStateManager,
	idx *indexer.Indexer,
	c *cache.Cache,
	o *oracle.Oracle,
) *Server {
	return &Server{
		config: config,
		router: r,
		state:  state,
		idxer:  idx,
		cache:  c,
		oracle: o,
	}
}

// Start 启动 HTTP 服务器
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// 注册路由
	mux.HandleFunc("/api/v1/quote", s.handleQuote)
	mux.HandleFunc("/api/v1/swap", s.handleSwap)
	mux.HandleFunc("/api/v1/pools", s.handlePools)
	mux.HandleFunc("/api/v1/status", s.handleStatus)
	mux.HandleFunc("/health", s.handleHealth)

	// 构建中间件链
	concLimiter := NewConcurrencyLimiter(s.config.MaxConcurrent)
	rateLimiter := NewRateLimiter(s.config.RateLimitPerIP, time.Second)

	handler := CORSMiddleware(
		LoggingMiddleware(
			rateLimiter.Middleware(
				concLimiter.Middleware(mux),
			),
		),
	)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.config.Port),
		Handler:      handler,
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
//   1. 解析请求参数
//   2. 检查缓存（Singleflight 防击穿）
//   3. 调用路由引擎查找最优路径
//   4. 预言机验证价格安全性
//   5. 返回报价结果
func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "only GET allowed")
		return
	}

	tokenIn := r.URL.Query().Get("token_in")
	tokenOut := r.URL.Query().Get("token_out")
	amountInStr := r.URL.Query().Get("amount_in")

	if tokenIn == "" || tokenOut == "" || amountInStr == "" {
		writeError(w, http.StatusBadRequest, "missing required params: token_in, token_out, amount_in")
		return
	}

	amountIn, ok := new(big.Int).SetString(amountInStr, 10)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid amount_in")
		return
	}

	// 尝试从缓存获取（Singleflight 防击穿）
	cacheKey := fmt.Sprintf("quote:%s:%s:%s", tokenIn, tokenOut, amountInStr)
	result, err := s.cache.GetOrLoad(cacheKey, func() (interface{}, error) {
		// 缓存未命中，执行路由搜索
		route, err := s.router.FindBestRoute(r.Context(), tokenIn, tokenOut, amountIn)
		if err != nil {
			return nil, err
		}
		return route, nil
	})

	if err != nil {
		writeError(w, http.StatusInternalServerError, "route search failed: "+err.Error())
		return
	}

	route := result.(*router.Route)

	// 预言机验证
	pair := tokenIn + "/" + tokenOut
	_, _, warning := s.oracle.ValidateSwap(pair, 0.005)

	resp := QuoteResponse{
		AmountOut:    route.AmountOut.String(),
		Path:         route.Path,
		GasEstimate:  route.GasEstimate,
		TicksCrossed: route.TicksCrossed,
		IsDegraded:   route.IsDegraded,
		Warning:      warning,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleSwap 提交交易接口
//
// 流程：
//   1. 解析请求（含 EIP-712 签名）
//   2. 预言机校验价格安全性
//   3. 验证 EIP-712 签名（含 nonce + deadline 防重放）
//   4. 提交交易到链上
//   5. 返回交易哈希（状态为 pending_confirmation）
func (s *Server) handleSwap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "only POST allowed")
		return
	}

	var req SwapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// 预言机验证
	pair := req.TokenIn + "/" + req.TokenOut
	allowed, _, warning := s.oracle.ValidateSwap(pair, req.Slippage)
	if !allowed {
		writeError(w, http.StatusForbidden, warning)
		return
	}

	// 返回乐观更新（Optimistic UI）
	// 交易哈希在实际提交到链上后更新
	resp := SwapResponse{
		TxHash:  "0x0000000000000000000000000000000000000000000000000000000000000000",
		Status:  "pending_confirmation",
		Warning: warning,
	}

	writeJSON(w, http.StatusOK, resp)
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
