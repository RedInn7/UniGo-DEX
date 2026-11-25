# Benchmark 与压测

> 小节编号沿用原文：1–13 见 [contracts.md](contracts.md)，14–22 见 [backend.md](backend.md)，23–26 见 [market-mev.md](market-mev.md)，27–32 见 [infrastructure.md](infrastructure.md)；测试见 [testing.md](testing.md)，压测见 [benchmark.md](benchmark.md)，部署与 API 见 [getting-started.md](getting-started.md)。

## Benchmark 性能对比

### 合约 Gas 基准

`contracts/test/Gas.test.js` 逐个场景记录单笔交易的 `gasUsed`（经 Manager 调用，Solidity 0.8.20，optimizer runs=200，viaIR）：

```bash
cd contracts && REPORT_GAS=1 npx hardhat test
```

| 场景 | gasUsed |
|------|--------:|
| mint 新区间（两端 tick 均未初始化） | 264,208 |
| mint 追加到已有 position | 179,895 |
| 单跳 swap（不跨 tick） | 111,795 |
| 单跳 swap（跨 1 个已初始化 tick） | 150,411 |
| 两跳 `exactInput` | 205,465 |
| `exactOutputSingle`（不跨 tick） | 109,926 |
| flash（借 token0） | 75,798 |
| flash（同时借两种） | 114,597 |

| 观察 | 数据 | 原因 |
|------|------|------|
| 新建 position 比追加贵 8.4 万 gas | 264,208 vs 179,895 | 两端 tick 首次初始化要写新的 storage slot、翻转位图 |
| 穿越一个已初始化的 tick 多 3.9 万 gas | 150,411 vs 111,795 | `Tick.cross` 翻转两个 `feeGrowthOutside` 并更新活跃流动性，外加一轮循环 |
| 两跳不到单跳的 2 倍 | 205,465 vs 111,795 × 2 | 交易的固定开销只付一次 |
| 精确输出与精确输入基本同价 | 109,926 vs 111,795 | 走的是同一个 swap 循环 |

同一次运行的 hardhat-gas-reporter 汇总（部分）：

| 合约 / 方法 | 平均 gas |
|------------|---------:|
| `UniGoFactory.createPool`（CREATE2 部署一个 Pool） | 3,811,025 |
| `UniGoPool.initialize` | 70,502 |
| `UniGoPool.burn` | 85,760 |
| `UniGoPool.collect` | 54,668 |
| `FlashArbitrage.arbitrage`（闪电贷 + 两次 swap + 还款） | 304,040 |
| 部署 `UniGoManager` | 1,043,649 |
| 部署 `UniGoQuoter` | 564,918 |
| 部署 `FlashArbitrage` | 901,756 |

### Goroutine 池 vs 朴素 goroutine

在 Apple M2（Go 1.26，darwin/arm64）上运行 `go test ./workerpool/ -run '^$' -bench=. -benchmem -count=3`，取三次中的典型值：

```
                                        耗时/op       内存/op      allocs/op
BenchmarkNaiveGoroutine (1000 tasks)    206μs        16.0 KB      1001
BenchmarkPooledGoroutine (1000 tasks)   226μs        16.0 KB      1001

BenchmarkGoroutineMemory:
  Naive  (10000 tasks)                  2.85ms       1.12 MB      20030
  Pool   (10000 tasks)                  3.25ms       0.16 MB      10006
```

| 指标（10000 任务） | 朴素 go func() | Goroutine 池 | 变化 |
|-------------------|---------------|-------------|------|
| 内存分配 | 1.12 MB | 0.16 MB | **减少 86%** |
| 分配次数 | 20030 | 10006 | **减少 50%** |
| 耗时 | 2.85ms | 3.25ms | 同一量级 |

**关键洞察**：池化的收益在内存和可控性上，而不在单次耗时。任务本身很轻时，朴素方案的耗时与池化在同一量级（任务要经过一次 channel 传递）；但朴素方案每个任务都要分配一个新 goroutine 的栈，10000 个任务的内存分配是池化的 7 倍，GC 压力随任务数线性增长。池化方案把 goroutine 数固定在 Worker 数，内存开销与任务数无关，并且有界队列天然提供背压——这正是路由引擎在行情高峰期需要的性质。

---

## 压测

Benchmark 一节量的是单个组件；这一节把整套系统（Hardhat 节点 + 合约 + 后端）拉起来，量 HTTP 吞吐与延迟、限流和背压是否生效、SSE 推送、索引追块和 Reorg 回滚。下面是精选结果，完整报告（含每张表的全部列、波动范围和修改前后对比）见 [`bench/REPORT.md`](../bench/REPORT.md)，原始结果在 [`bench/results/`](../bench/results/)。

### 环境与数据规模

| 项目 | 值 |
|---|---|
| 机型 / CPU | Apple M2，8 核 |
| 内存 | 8 GB |
| 操作系统 | macOS 26.5.2 |
| Go / Node.js / Hardhat / k6 | go1.26.0 / v23.9.0 / 2.28.6 / v1.6.0 |
| 部署方式 | Hardhat 本地节点、后端、压测工具 k6 在同一台机器上，RPC 走本机回环 |
| 后端配置 | 日志级别 warn，访问日志关闭；吞吐上限一组关闭限流和并发上限 |

k6 与后端共用这 8 个核，HTTP 吞吐是两者同机运行时的结果；压测期间的 1 分钟系统负载为 21.8 – 48.5。

数据由 `bench/up.sh` 拉起，随机数用固定种子，每次相同：

- **合约**：`scripts/deploy.js` 部署的全套合约，6 个池子覆盖三档费率，每个池子 4 个流动性头寸（一个宽区间、一个窄区间、上下各一个单边区间），8 个已初始化的 tick
- **历史成交**：3,000 笔单池 swap，三个账户随机在六个池子里交易，每个区块 5 笔、区块间隔 12 秒，链头在区块 679；每个池子约 500 笔成交、约 120 根 1m K 线
- **路由**：WETH → USDC 有 2 条单跳候选路径；WETH → WBTC 必须走三跳（WETH → USDC → DAI → WBTC），每一跳两个费率档，共 8 条候选路径

### HTTP 吞吐上限

k6 脚本 `bench/k6/http.js`，100 个虚拟用户不加思考时间连续发请求，每个场景 20 秒、跑 3 轮；限流、并发上限、访问日志全部关闭。RPS 取中位轮，延迟取自同一轮。

| 场景 | 说明 | RPS（中位轮） | RPS 范围 | p50 ms | p95 ms | p99 ms | 错误率 |
|---|---|---:|---|---:|---:|---:|---:|
| quote_cached | 报价，缓存命中，单跳 | 40,965 | 40,855 – 51,628 | 1.34 | 6.12 | 13.40 | 0.00% |
| quote_miss_1hop | 报价，缓存未命中，单跳（2 条候选路径） | 31,329 | 29,087 – 34,986 | 2.65 | 7.38 | 10.96 | 0.00% |
| quote_cached_3hop | 报价，缓存命中，三跳 | 37,625 | 37,452 – 43,401 | 1.60 | 6.81 | 12.94 | 0.00% |
| quote_miss_3hop | 报价，缓存未命中，三跳（8 条候选路径 × 3 次 swap 模拟） | 11,098 | 10,396 – 12,135 | 8.04 | 18.72 | 25.34 | 0.00% |
| klines | K 线查询（最近 100 根 1m） | 18,602 | 18,168 – 20,612 | 3.21 | 17.94 | 35.21 | 0.00% |
| trades | 成交查询（最近 50 笔） | 28,194 | 27,353 – 28,366 | 2.07 | 10.55 | 22.45 | 0.00% |
| build | 构造待签名交易（重新搜路由 + 链上 Quoter `eth_call`） | 1,835 | 1,583 – 1,874 | 52.64 | 72.37 | 113.32 | 0.00% |
| mixed | 混合读（报价 50% / K 线 20% / 成交 15% / 其它 15%） | 25,679 | 25,316 – 26,528 | 2.36 | 11.49 | 21.65 | 0.00% |

- “缓存命中”指所有请求的币对和金额相同：报价缓存的 key 包含最新区块号，压测期间链不出新块，除第一个请求外全部命中。“缓存未命中”指每个请求的金额都不同，每次都要重新搜索路由并逐 tick 模拟。
- `build` 每个请求要对链上 Quoter 做一次 `eth_call`，吞吐由 Hardhat 节点决定。

**与后端指标交叉核对**。压测脚本在每轮前后读后端的 `http_requests_total`，增量必须等于 k6 发出的请求数——确认 k6 统计的每个请求后端都真的处理了：

| 场景 | k6 发出的请求数（3 轮合计） | 后端 `http_requests_total` 增量 | 核对 | 后端常驻内存峰值 MB |
|---|---:|---:|---|---:|
| quote_cached | 2,669,333 | 2,669,333 | 一致 | 71.9 |
| quote_miss_1hop | 1,908,309 | 1,908,309 | 一致 | 69.1 |
| quote_cached_3hop | 2,369,922 | 2,369,922 | 一致 | 72.2 |
| quote_miss_3hop | 672,831 | 672,831 | 一致 | 113.2 |
| klines | 1,148,005 | 1,148,005 | 一致 | 116.3 |
| trades | 1,678,661 | 1,678,661 | 一致 | 125.0 |
| build | 105,994 | 105,994 | 一致 | 45.3 |
| mixed | 1,550,863 | 1,550,863 | 一致 | 89.1 |

### 限流与背压生效

吞吐上限是关闭保护量的。下面两组打开保护，验证超限的请求确实被挡住（k6 的所有请求来自同一个 IP，100 并发 × 20 秒）：

| 后端配置 | 场景 | 请求总数 | 放行（200） | 限流拒绝（429） | 背压拒绝（503） | 后端计数 429 / 503 | p99 ms |
|---|---|---:|---:|---:|---:|---|---:|
| 每 IP 100 请求/秒、突发 200；全局并发上限 500 | mixed | 825,061 | 2,200（0.27%，110.0/s） | 822,861（99.73%） | 0 | 822,861 / 0 | 12.57 |
| 限流关闭；全局并发上限 20 | quote_miss_3hop | 698,277 | 105,301（15.08%，5,265.1/s） | 0 | 592,976（84.92%） | 0 / 592,976 | 17.66 |

- 第一行：令牌桶按 IP 限流。20 秒内理论上最多放行 200（突发）+ 100 × 20 = 2200 个请求，实际放行 2,200 个。
- 第二行：全局并发信号量。同时在处理的请求超过 20 个时，新请求立即返回 503（带 `Retry-After`），不排队。
- “后端计数”是 `http_rate_limited_total` / `http_concurrency_rejected_total` 在压测期间的增量，与 k6 看到的 429 / 503 数量一致。

**缓存条目数上限的效果**（`quote_miss_1hop`，每个请求一个新的缓存 key）：

| 配置 | RPS | p99 ms | 后端常驻内存 MB |
|---|---:|---:|---:|
| 缓存最多 10000 条 | 31,329 | 10.96 | 69.1 |
| 缓存不设上限 | 27,385 | 22.18 | 762.4 |

**访问日志的开销**：打开逐请求的结构化访问日志后，`quote_cached` 为 40,359 RPS（关闭时 40,965），`mixed` 为 25,109 RPS（关闭时 25,679），差别在各轮之间的波动范围之内。

### SSE 推送

工具 `backend/cmd/bench` 的 `sse` 子命令：N 个订阅端同时在线订阅成交事件，链上发 60 笔交易。

| 订阅端数 | 送达次数 | 未送达 | 端到端 p50 ms | 端到端 p99 ms | 扇出 p50 ms | 扇出 p95 ms | 扇出最大 ms |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 50 | 3,000 | 0 | 78.02 | 198.22 | 0.19 | 0.50 | 3.89 |
| 200 | 12,000 | 0 | 87.54 | 194.81 | 0.77 | 1.60 | 2.41 |
| 500 | 30,000 | 0 | 90.56 | 198.33 | 1.71 | 3.39 | 4.85 |

- **端到端延迟**：从发交易的 RPC 返回（Hardhat 自动出块，此时交易已上链）到订阅端读到这笔成交的推送。它包含索引器的轮询等待，压测时轮询间隔为 200 毫秒（`POLL_INTERVAL_MS=200`）。
- **扇出**：同一笔成交，最早收到和最晚收到的订阅端之间差多久，反映 Hub 把一条消息发给所有订阅端要多久。

**突发与慢客户端**：关掉自动出块，把成交按每个区块 100 笔打包，短时间灌入。

| 正常订阅端 | 只连不读的慢客户端 | 灌入成交笔数 | 全部送达耗时 s | 送达次数 / 应送达 | 收全的正常订阅端 | 后端断开的慢客户端数 |
|---:|---:|---:|---:|---|---|---:|
| 200 | 5 | 6,000 | 19.2 | 1,200,000 / 1,200,000 | 200 / 200 | 5 |

“只连不读”的客户端建立连接后不读取任何数据，它的发送队列（256 条）和内核缓冲区填满后被 Hub 断开（计入 `stream_dropped_clients_total`），正常订阅端不受影响。

### Go 基准：逐 tick 模拟与路由

`go test -bench`，每个基准跑 3 次取中位数。

| 基准 | ns/op（中位） | 换算吞吐（次/秒） | allocs/op | 含义 |
|---|---:|---:|---:|---|
| `router.SimulateSwap/cross=0` | 4,106 | 243,546 | 107 | 单池逐 tick 模拟一笔 swap，不穿越 tick |
| `router.SimulateSwap/cross=1` | 6,318 | 158,278 | 162 | 穿越 1 个已初始化的 tick |
| `router.SimulateSwap/cross=5` | 13,479 | 74,189 | 342 | 穿越 5 个 |
| `router.SimulateSwap/cross=20` | 40,210 | 24,869 | 1,026 | 穿越 20 个 |
| `router.RouteSearchConcurrent` | 431,663 | 2,317 | 18,703 | 32 条候选路径，多个协程同时发起搜索 |
| `kline.OnSwap/pools=1` | 1,027 | 973,710 | 19 | 写入一笔成交并更新 6 个周期的 K 线 |
| `kline.Klines` | 16,922 | 59,095 | 401 | 查询最近 100 根 1m K 线 |
| `mev.OnSwap/swaps_per_block=1000` | 273 | 3,668,379 | 3 | 三明治检测处理一笔 swap（随机输入，单区块 1000 笔） |
| `mev.OnSwapWorstCase/swaps_per_block=256` | 49 | 20,576,132 | 0 | 最不利输入：同一地址在一个区块里来回买卖 |

**路由：串行评估 对比 Worker Pool 并发评估**（三跳路由搜索，每条候选路径模拟 3 次 swap）：

| 候选路径数 | 串行评估 µs/次 | Worker Pool 并发评估 µs/次 | 加速比 |
|---:|---:|---:|---:|
| 8 | 187.4 | 109.0 | 1.72× |
| 32 | 755.2 | 385.0 | 1.96× |
| 64 | 1,523.6 | 767.9 | 1.98× |

**三明治检测：按交易方和方向建索引的效果**（最不利输入）：

| 基准 | 逐个向前配对 ns/op | 建索引后 ns/op | 倍数 |
|---|---:|---:|---:|
| `mev.OnSwapWorstCase/swaps_per_block=64` | 1,391 | 55 | 25.16× |
| `mev.OnSwapWorstCase/swaps_per_block=256` | 16,698 | 49 | 343.58× |

建索引之后单笔耗时不随区块内 swap 数增长。

### 索引器：回放吞吐、追块与 Reorg 回滚

**回放吞吐**（Go 基准 `indexer.Replay`：内存数据源回放 2000 个区块、2 万个事件，不含 RPC）：

| 基准 | ns/op（中位） | 事件/秒 | 区块/秒 |
|---|---:|---:|---:|
| `indexer.Replay/indexer_only`（只有索引器） | 13,256,237 | 1,508,725 | 150,873 |
| `indexer.Replay/with_kline_mev_oracle`（带 K 线、三明治检测、预言机三个订阅者） | 44,293,526 | 451,534 | 45,153 |

**对本地链追块**（`backend/cmd/bench` 的 `chase` 子命令：对着 Hardhat 节点从创世块追到链头，带上全部订阅者）：

| 实现 | 次数 | 区块数 | 事件数 | 耗时 ms（中位） | 区块/秒（中位） | 事件/秒（中位） |
|---|---:|---:|---:|---:|---:|---:|
| 每个区块一次 `eth_getLogs` | 6 | 713 | 3102 | 302 | 2,364 | 10,286 |
| 按区块范围批量 `eth_getLogs`（现在的实现） | 6 | 680 | 3036 | 193 | 3,517 | 15,701 |

追块速度主要取决于 RPC 往返次数：区块头每块 1 次，日志从每块 1 次变成每 500 个区块 1 次。

**一次 Reorg 的回滚并重建**（发现父哈希不一致 → 找分叉点 → 撤销池子状态、K 线、告警 → 重新索引到新链头）：

| 实现 | 次数 | Reorg 深度（区块） | 重新索引的区块数 | 重新索引的事件数 | 耗时 ms（中位） | 范围 ms |
|---|---:|---:|---:|---:|---:|---|
| 每个区块一次 `eth_getLogs` | 5 | 10 | 11 | 22 | 8.96 | 7.83 – 10.12 |
| 按区块范围批量 `eth_getLogs`（现在的实现） | 5 | 10 | 11 | 22 | 5.07 | 4.79 – 12.53 |

### 复现

```bash
# 依赖：Go、Node.js、k6；contracts/ 下已执行过 npm install
bench/run-all.sh          # 全部步骤，约 25 分钟，最后重新生成 bench/REPORT.md
```

分步执行：

```bash
bench/up.sh                                   # 节点 + 合约 + 历史成交 + 后端（保护全关）
LABEL=r1 bench/run-http.sh                    # 8 个 HTTP 场景，100 并发 × 20 秒
LABEL=r1 bench/run-http.sh quote_cached       # 只跑一个场景
VUS=50 DURATION=30s LABEL=x bench/run-http.sh mixed

ACCESS_LOG=on LOG_LEVEL=info bench/backend.sh start        # 换一种后端配置后重跑
RATE_LIMIT_PER_IP=100 RATE_LIMIT_BURST=200 MAX_CONCURRENT=500 bench/backend.sh start
RATE_LIMIT_PER_IP=0 MAX_CONCURRENT=20 bench/backend.sh start

cd backend
go run ./cmd/bench sse -clients 200 -events 40
go run ./cmd/bench sse -clients 200 -stalled 5 -events 0 -burst 6000
go run ./cmd/bench chase -runs 6 -reorg-depth 10 -reorg-runs 5
go test -p 2 -run '^$' -bench . -benchmem -count 3 ./router ./kline ./mev ./indexer
cd ..

bench/down.sh                                 # 停掉后端和节点
python3 bench/report.py                       # 由 bench/results/ 重新生成报告
```

压测环境默认使用端口 18545（节点）和 18080（后端），可用 `BENCH_RPC_PORT` / `BENCH_API_PORT` 修改。

