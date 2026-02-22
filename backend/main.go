package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"unigo-dex/api"
	"unigo-dex/cache"
	"unigo-dex/indexer"
	"unigo-dex/oracle"
	"unigo-dex/router"
)

// UniGo-DEX 后端服务
//
// 系统架构：
//
//   ┌──────────┐    ┌──────────┐    ┌───────────┐
//   │ Ethereum │───>│ Indexer  │───>│ PoolState │
//   │  Node    │    │(事件索引) │    │ (状态管理) │
//   └──────────┘    └──────────┘    └─────┬─────┘
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
//   2. 启动 Indexer（事件索引）
//   3. 启动 Cache + Warmup（缓存预热）
//   4. 初始化 Oracle（价格验证）
//   5. 初始化 Router（路由引擎）
//   6. 启动 API Server（HTTP 服务）

func main() {
	fmt.Println("========================================")
	fmt.Println("  UniGo-DEX 后端服务 v0.2.0")
	fmt.Println("========================================")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// === 1. 初始化 PoolStateManager ===
	log.Println("[启动] 初始化池子状态管理器...")
	stateManager := indexer.NewPoolStateManager()

	// === 2. 初始化并启动 Indexer ===
	log.Println("[启动] 初始化事件索引器...")
	idxConfig := indexer.DefaultConfig()
	idxConfig.RPCURL = getEnv("RPC_URL", "http://127.0.0.1:8545")
	idx := indexer.NewIndexer(idxConfig, stateManager)
	if err := idx.Start(ctx); err != nil {
		log.Fatalf("索引器启动失败: %v", err)
	}

	// === 3. 初始化 Cache + Warmup ===
	log.Println("[启动] 初始化缓存层...")
	appCache := cache.NewCache(30 * time.Second)

	// 缓存预热（加载 Top 交易对）
	warmupConfig := cache.WarmupConfig{}
	loaded := appCache.Warmup(warmupConfig, func(token0, token1 string, fee uint32) (interface{}, error) {
		return nil, nil // 实际项目中从 The Graph 加载
	})
	log.Printf("[启动] 缓存预热完成，加载了 %d 个交易对", loaded)

	// === 4. 初始化 Oracle ===
	log.Println("[启动] 初始化价格预言机验证器...")
	oracleInst := oracle.NewOracle(oracle.DefaultOracleConfig())

	// === 5. 初始化 Router ===
	log.Println("[启动] 初始化智能路由引擎...")
	routerConfig := router.DefaultRouterConfig()
	routerInst := router.NewRouter(routerConfig, stateManager)
	log.Printf("[启动] 路由引擎: 最大跳数=%d, 超时=%v, 并发限制=%d",
		routerConfig.MaxHops, routerConfig.RouteTimeout, routerConfig.MaxConcurrentRoutes)

	// === 6. 启动 API Server ===
	log.Println("[启动] 初始化 API 服务器...")
	serverConfig := api.DefaultServerConfig()
	serverConfig.Port = getEnvInt("PORT", 8080)
	server := api.NewServer(serverConfig, routerInst, stateManager, idx, appCache, oracleInst)

	// 在独立协程中启动 HTTP 服务器
	go func() {
		if err := server.Start(); err != nil {
			log.Printf("[API] 服务器退出: %v", err)
		}
	}()

	fmt.Println()
	fmt.Println("========================================")
	fmt.Printf("  API 服务运行在 http://localhost:%d\n", serverConfig.Port)
	fmt.Println("  端点:")
	fmt.Println("    GET  /api/v1/quote   — 获取报价")
	fmt.Println("    POST /api/v1/swap    — 提交交易")
	fmt.Println("    GET  /api/v1/pools   — 池子列表")
	fmt.Println("    GET  /api/v1/status  — 系统状态")
	fmt.Println("    GET  /health         — 健康检查")
	fmt.Println("========================================")

	// === 优雅关闭 ===
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("[关闭] 收到退出信号，正在优雅关闭...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[关闭] API 服务器关闭错误: %v", err)
	}

	cancel() // 停止索引器和其他后台协程
	log.Println("[关闭] UniGo-DEX 后端服务已停止")
}

// getEnv 获取环境变量，带默认值
func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// getEnvInt 获取整数环境变量
func getEnvInt(key string, defaultValue int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	var result int
	fmt.Sscanf(v, "%d", &result)
	if result == 0 {
		return defaultValue
	}
	return result
}
