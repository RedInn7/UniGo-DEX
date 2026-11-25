package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"unigo-dex/api"
	"unigo-dex/arb"
	"unigo-dex/cache"
	"unigo-dex/chain"
	"unigo-dex/eip712"
	"unigo-dex/gas"
	"unigo-dex/indexer"
	"unigo-dex/kline"
	"unigo-dex/logging"
	"unigo-dex/mev"
	"unigo-dex/oracle"
	"unigo-dex/router"
	"unigo-dex/ws"
)

// UniGo-DEX 后端服务
//
// 系统架构：
//
//   ┌──────────┐    ┌──────────┐    ┌───────────┐
//   │ Ethereum │───>│ Indexer  │───>│ PoolState │
//   │  Node    │    │(事件索引) │    │ (状态管理) │
//   └──────────┘    └────┬─────┘    └─────┬─────┘
//    (chain 包)          └──> 订阅 Swap 事件：K 线 / 三明治检测 / 套利扫描 / 预言机 / SSE 推送
//                                         │
//   ┌──────────┐    ┌──────────┐    ┌─────▼─────┐
//   │ Chainlink│───>│ Oracle   │───>│  Router   │
//   │(预言机)   │    │(价格验证) │    │(路由引擎)  │
//   └──────────┘    └──────────┘    └─────┬─────┘
//                                         │
//   ┌──────────┐    ┌──────────┐    ┌─────▼─────┐
//   │  Cache   │<──>│  API     │<──>│  EIP-712  │
//   │(缓存层)   │    │(REST/WS) │    │(签名验证)  │
//   └──────────┘    └──────────┘    └───────────┘
//
// 启动顺序：
//   1. 初始化 PoolStateManager（内存状态）
//   2. 连接链节点（连不上则用模拟源）
//   3. 初始化订阅 Swap 事件流的模块（预言机 / K 线 / MEV / 套利 / 推送）
//   4. 启动 Indexer（事件索引）
//   5. 初始化 Cache（缓存层）
//   6. 初始化 Router（路由引擎）
//   7. 启动 API Server（HTTP 服务）

func main() {
	fmt.Println("========================================")
	fmt.Println("  UniGo-DEX 后端服务 v0.3.0")
	fmt.Println("========================================")

	// 结构化日志：LOG_LEVEL=debug|info|warn|error，LOG_FORMAT=text|json
	logging.Init(getEnv("LOG_LEVEL", "info"), getEnv("LOG_FORMAT", "text"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// === 1. 初始化 PoolStateManager ===
	slog.Info("初始化池子状态管理器")
	stateManager := indexer.NewPoolStateManager()

	// === 2. 连接链节点 ===
	idxConfig := indexer.DefaultConfig()
	idxConfig.RPCURL = os.Getenv("RPC_URL")
	idxConfig.FactoryAddress = os.Getenv("FACTORY_ADDRESS")
	idxConfig.StartBlock = uint64(getEnvInt("START_BLOCK", 0))
	if n := getEnvInt("CONFIRMATION_BLOCKS", 0); n > 0 {
		idxConfig.ConfirmationBlocks = uint64(n)
	}
	if ms := getEnvInt("POLL_INTERVAL_MS", 0); ms > 0 {
		idxConfig.PollInterval = time.Duration(ms) * time.Millisecond
	}

	// 配置了 RPC_URL 且节点连得上就用真实链，否则用模拟源，保证不配节点也能启动
	var (
		chainClient *chain.Client
		chainSource indexer.ChainSource // nil = 模拟源
		feeSource   gas.FeeSource       // nil = 静态 Gas 估算
	)
	chainID := uint64(getEnvInt("CHAIN_ID", 31337))
	if idxConfig.RPCURL != "" {
		dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
		client, err := chain.Dial(dialCtx, idxConfig.RPCURL, idxConfig.FactoryAddress)
		if err == nil {
			if id, idErr := client.ChainID(dialCtx); idErr == nil {
				chainID = id.Uint64()
			}
		}
		dialCancel()
		switch {
		case err != nil:
			slog.Warn("连接节点失败，索引器使用模拟源", "rpc_url", idxConfig.RPCURL, "err", err)
		case idxConfig.FactoryAddress == "":
			chainClient, feeSource = client, client
			slog.Warn("已连接节点，但未配置 FACTORY_ADDRESS，无法发现池子，索引器使用模拟源")
		default:
			chainClient, feeSource, chainSource = client, client, client
			slog.Info("已连接节点", "rpc_url", idxConfig.RPCURL, "chain_id", chainID, "factory", idxConfig.FactoryAddress, "start_block", idxConfig.StartBlock)
		}
	} else {
		slog.Info("未配置 RPC_URL，索引器使用模拟源")
	}

	// === 3. 初始化订阅 Swap 事件流的各模块 ===
	slog.Info("初始化预言机、K 线、MEV 检测、套利扫描与实时推送")
	oracleInst := oracle.NewOracle(oracle.DefaultOracleConfig())
	gasEstimator := gas.NewEstimator(gas.DefaultConfig(), feeSource)
	klines := kline.NewAggregator(kline.DefaultConfig())
	guard := mev.NewGuard(mev.DefaultConfig())

	arbConfig := arb.DefaultConfig()
	arbConfig.FlashArbAddress = strings.ToLower(os.Getenv("FLASH_ARB_ADDRESS"))
	arbConfig.WETH = strings.ToLower(os.Getenv("WETH_ADDRESS"))
	scanner := arb.NewScanner(arbConfig, stateManager, gasEstimator)

	// 实时推送：各模块在索引协程里产生的数据经 Hub 扇出给 SSE 客户端（Broadcast 不阻塞）
	hub := ws.NewHub()
	go hub.Run(ctx)
	klines.OnTrade = func(trade kline.Trade, candle kline.Candle) {
		hub.Broadcast("trade", trade)
		hub.Broadcast("kline", api.KlineUpdate{Pool: trade.Pool, Interval: "1m", Candle: candle})
	}
	guard.OnAlert = func(alert mev.Alert) { hub.Broadcast("mev_alert", alert) }
	scanner.OnOpportunity = func(opp arb.Opportunity) { hub.Broadcast("arbitrage", opp) }

	// === 4. 初始化并启动 Indexer ===
	slog.Info("初始化事件索引器")
	idx := indexer.NewIndexer(idxConfig, stateManager, chainSource)
	idx.Subscribe(oracleInst.PoolPriceFeed(stateManager))
	idx.Subscribe(klines)
	idx.Subscribe(guard)
	idx.Subscribe(scanner)
	if err := idx.Start(ctx); err != nil {
		slog.Error("索引器启动失败", "err", err)
		os.Exit(1)
	}

	// === 5. 初始化 Cache ===
	slog.Info("初始化缓存层")
	appCache := cache.NewCache(30 * time.Second)

	// === 6. 初始化 Router ===
	slog.Info("初始化智能路由引擎")
	routerConfig := router.DefaultRouterConfig()
	routerInst := router.NewRouter(routerConfig, stateManager)
	slog.Info("路由引擎配置", "max_hops", routerConfig.MaxHops, "timeout", routerConfig.RouteTimeout, "max_concurrent", routerConfig.MaxConcurrentRoutes)

	// === 7. 启动 API Server ===
	slog.Info("初始化 API 服务器")
	serverConfig := api.DefaultServerConfig()
	serverConfig.Port = getEnvInt("PORT", 8080)
	serverConfig.RateLimitPerIP = getEnvInt("RATE_LIMIT_PER_IP", serverConfig.RateLimitPerIP)
	serverConfig.RateLimitBurst = getEnvInt("RATE_LIMIT_BURST", serverConfig.RateLimitBurst)
	serverConfig.MaxConcurrent = getEnvInt("MAX_CONCURRENT", serverConfig.MaxConcurrent)
	serverConfig.DisableAccessLog = os.Getenv("ACCESS_LOG") == "off"
	serverConfig.ManagerAddress = os.Getenv("MANAGER_ADDRESS")
	serverConfig.QuoterAddress = os.Getenv("QUOTER_ADDRESS")
	serverConfig.TWAPWindow = uint32(getEnvInt("TWAP_WINDOW", int(serverConfig.TWAPWindow)))

	// EIP-712 域：链 ID + Manager 地址，签名不能跨链、跨合约重放
	verifyingContract := serverConfig.ManagerAddress
	if verifyingContract == "" {
		verifyingContract = "0x0000000000000000000000000000000000000000"
	}
	verifier := eip712.NewVerifier(eip712.DomainSeparator{
		Name: "UniGo-DEX", Version: "1", ChainID: chainID, VerifyingContract: verifyingContract,
	}, 5*time.Minute)

	// Chainlink 喂价：按币对配置聚合器地址，读链需要节点
	var chainlinkReader *oracle.ChainlinkReader
	if spec := os.Getenv("CHAINLINK_FEEDS"); spec != "" {
		feeds, err := oracle.ParseFeeds(spec)
		switch {
		case err != nil:
			slog.Error("CHAINLINK_FEEDS 配置有误", "err", err)
			os.Exit(1)
		case chainClient == nil:
			slog.Warn("配置了 Chainlink 喂价，但没有连接节点，喂价不会生效", "feeds", len(feeds))
		default:
			refresh := time.Duration(getEnvInt("CHAINLINK_REFRESH_SECONDS", int(oracle.DefaultRefreshInterval/time.Second))) * time.Second
			chainlinkReader = oracle.NewChainlinkReader(chainClient, feeds, refresh)
			slog.Info("已配置 Chainlink 喂价", "feeds", len(feeds), "refresh", refresh)
		}
	}

	server := api.NewServer(serverConfig, api.Deps{
		Router:    routerInst,
		State:     stateManager,
		Indexer:   idx,
		Cache:     appCache,
		Oracle:    oracleInst,
		Chain:     chainClient,
		Gas:       gasEstimator,
		Klines:    klines,
		Guard:     guard,
		Arb:       scanner,
		Hub:       hub,
		Verifier:  verifier,
		Chainlink: chainlinkReader,
	})

	// 在独立协程中启动 HTTP 服务器
	go func() {
		if err := server.Start(); err != nil {
			slog.Info("API 服务器退出", "reason", err)
		}
	}()

	fmt.Println()
	fmt.Println("========================================")
	fmt.Printf("  API 服务运行在 http://localhost:%d\n", serverConfig.Port)
	fmt.Println("  端点:")
	fmt.Println("    GET  /api/v1/quote        — 获取报价")
	fmt.Println("    POST /api/v1/swap/build   — 构造待签名交易")
	fmt.Println("    POST /api/v1/swap         — 提交交易（EIP-712 验签）")
	fmt.Println("    GET  /api/v1/nonce/{addr} — 签名用 nonce")
	fmt.Println("    GET  /api/v1/tx/{hash}    — 交易状态")
	fmt.Println("    GET  /api/v1/pools        — 池子列表")
	fmt.Println("    GET  /api/v1/klines       — K 线")
	fmt.Println("    GET  /api/v1/trades       — 最近成交")
	fmt.Println("    GET  /api/v1/twap         — TWAP 与现价偏离")
	fmt.Println("    GET  /api/v1/mev/alerts   — 三明治攻击告警")
	fmt.Println("    GET  /api/v1/arbitrage/opportunities — 套利机会")
	fmt.Println("    GET  /api/v1/stream       — SSE 实时推送")
	fmt.Println("    GET  /api/v1/status       — 系统状态")
	fmt.Println("    GET  /metrics             — 监控指标")
	fmt.Println("    GET  /health              — 健康检查")
	fmt.Println("========================================")

	// === 优雅关闭 ===
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("收到退出信号，正在优雅关闭")
	cancel() // 停止索引器，并让 Hub 断开所有推送连接，否则长连接会拖住 HTTP 服务器的关闭
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("API 服务器关闭出错", "err", err)
	}

	slog.Info("UniGo-DEX 后端服务已停止")
}

// getEnv 获取环境变量，带默认值
func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// getEnvInt 获取整数环境变量；未设置或不是整数时用默认值
func getEnvInt(key string, defaultValue int) int {
	result, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return defaultValue
	}
	return result
}
