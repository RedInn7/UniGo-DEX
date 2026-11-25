# 项目结构

> 小节编号沿用原文：1–13 见 [contracts.md](contracts.md)，14–22 见 [backend.md](backend.md)，23–26 见 [market-mev.md](market-mev.md)，27–32 见 [infrastructure.md](infrastructure.md)；测试见 [testing.md](testing.md)，压测见 [benchmark.md](benchmark.md)，部署与 API 见 [getting-started.md](getting-started.md)。

```
UniGo-DEX/
├── contracts/                              # Solidity 智能合约层（Hardhat）
│   ├── contracts/
│   │   ├── UniGoPool.sol                   # 核心流动性池（mint/burn/collect/swap/flash/observe）
│   │   ├── UniGoFactory.sol                # 池工厂（CREATE2 确定性部署）
│   │   ├── UniGoManager.sol                # 用户入口（期限/滑点保护、多跳、精确输出、Pool 真伪校验）
│   │   ├── UniGoQuoter.sol                 # 链上报价（revert-catch 模拟，单跳 + 多跳）
│   │   ├── FlashArbitrage.sol              # 闪电贷套利（借 → 卖 → 买回 → 还）
│   │   ├── interfaces/
│   │   │   ├── IUniGoPool.sol              # Pool 接口与事件
│   │   │   ├── IUniGoFactory.sol           # Factory 接口
│   │   │   └── IUniGoPoolCallbacks.sol     # mint/swap/flash 回调接口
│   │   ├── lib/                            # 库（11 个）
│   │   │   ├── Math.sol                    # 512 位 mulDiv（牛顿法模逆）
│   │   │   ├── TickMath.sol                # tick ↔ sqrtPrice（查表法）
│   │   │   ├── SqrtPriceMath.sol           # 基于价格计算 token 数量
│   │   │   ├── SwapMath.sol                # 单步 swap 计算
│   │   │   ├── TickBitmap.sol              # O(1) tick 查找（位图）
│   │   │   ├── BitMath.sol                 # MSB/LSB 位运算
│   │   │   ├── Tick.sol                    # tick 状态、手续费外侧累计值、单 tick 流动性上限
│   │   │   ├── Position.sol                # position 状态与手续费结算
│   │   │   ├── Oracle.sol                  # TWAP 观测环形数组（二分 + 插值）
│   │   │   ├── LiquidityMath.sol           # 流动性加减
│   │   │   └── FixedPoint96.sol            # Q64.96 常量
│   │   └── test/                           # 仅测试和本地部署使用的合约（10 个）
│   │       ├── TestERC20.sol               # 标准测试代币
│   │       ├── AggregatorV3Interface.sol   # Chainlink 聚合器接口
│   │       ├── TestAggregator.sol          # 可控的模拟聚合器（可设置任意轮次数据）
│   │       ├── TestNoReturnERC20.sol       # 没有返回值的代币（USDT 式）
│   │       ├── TestFalseReturnERC20.sol    # 失败时返回 false 的代币
│   │       ├── TestFlashBorrower.sol       # 闪电贷借款人
│   │       ├── TestFakePool.sol            # 伪造的 Pool（验证 Manager 的真伪校验）
│   │       ├── TestOracleReader.sol        # 读取 observe 并计算平均 tick
│   │       ├── TestMathHarness.sol         # 暴露数学库供测试调用
│   │       └── TestTickHarness.sol         # 暴露 Tick 库供测试调用
│   ├── test/                               # 109 个用例
│   │   ├── UniGoPool.test.js               # Pool：流动性、swap、手续费、闪电贷（24）
│   │   ├── UniGoManager.test.js            # Manager：多跳、精确输出、报价、真伪校验（18）
│   │   ├── Security.test.js                # 安全回归（15）
│   │   ├── Math.test.js                    # 数学库对照（13）
│   │   ├── Oracle.test.js                  # TWAP 预言机（12）
│   │   ├── FlashArbitrage.test.js          # 闪电贷套利（10）
│   │   ├── UniGoFactory.test.js            # Factory（10）
│   │   ├── Gas.test.js                     # Gas 基准（6）
│   │   ├── Invariants.test.js              # 固定种子的不变量随机测试（1）
│   │   └── helpers.js                      # 部署、建池、路径编码等公共函数
│   ├── scripts/
│   │   └── deploy.js                       # 部署全套合约、4 个代币、6 个池子并注入流动性
│   ├── hardhat.config.js
│   └── package.json
│
├── backend/                                # Go 后端（module unigo-dex）
│   ├── main.go                             # 入口 + 模块装配 + 优雅关闭
│   │
│   ├── math/                               # 链下数学库（与链上逐位一致）
│   │   ├── tickmath.go                     # tick ↔ sqrtPrice（20 魔数查表法）
│   │   ├── sqrtpricemath.go                # token 数量计算 + ComputeSwapStep
│   │   ├── constants.go                    # Q96, MIN/MAX_TICK, FeeToTickSpacing
│   │   └── tickmath_test.go
│   │
│   ├── chain/                              # 基于 go-ethereum 的链交互层
│   │   ├── client.go                       # 区块 / 日志 / 回执 / 广播 / 费用，RPC 经断路器
│   │   ├── contracts.go                    # slot0 / TWAP / Quoter / Chainlink / ERC20 余额读调用 + 调用数据编码
│   │   ├── events.go                       # 按区块范围批量拉日志 + 事件解码（签名取自内嵌 ABI）
│   │   ├── contractabi/                    # 内嵌 ABI
│   │   │   ├── contractabi.go              # go:embed + 解析
│   │   │   ├── gen.go                      # go generate：从 Hardhat 编译产物提取 ABI
│   │   │   └── *.json                      # Pool / Factory / Manager / Quoter / FlashArbitrage / AggregatorV3Interface / IERC20
│   │   └── *_test.go                       # 在 go-ethereum 模拟后端上测试
│   │
│   ├── indexer/                            # 事件索引器
│   │   ├── indexer.go                      # ChainSource 接口 + 分段追块 + Reorg 检测 + 撤销日志回滚
│   │   ├── pool_state.go                   # PoolState + PoolStateManager + Snapshot
│   │   ├── events.go                       # 事件结构体
│   │   ├── sim_source.go                   # 未配置节点时的模拟数据源
│   │   └── bench_test.go                   # 回放吞吐基准
│   │
│   ├── router/                             # 智能路由引擎
│   │   ├── quote.go                        # 逐 tick 精确的链下 swap 模拟（SimulateSwap）
│   │   ├── router.go                       # 路径枚举 + 并发评估 + 200ms 超时 + 二级降级
│   │   └── bench_test.go                   # 逐 tick 模拟、串行对比 Worker Pool 的基准
│   │
│   ├── oracle/                             # 价格安全
│   │   ├── oracle.go                       # 现价 / TWAP / Chainlink 三方交叉校验 + 保护模式
│   │   └── chainlink.go                    # 读链上聚合器：有效性检查、口径换算、缓存刷新
│   │
│   ├── eip712/                             # 签名验证
│   │   └── eip712.go                       # EIP-712 摘要 + ecrecover + 三层防重放
│   │
│   ├── gas/                                # Gas 成本评估
│   │   └── gas.go                          # EIP-1559：baseFee + priorityFee，取不到时退回静态值
│   │
│   ├── kline/                              # 行情
│   │   └── kline.go                        # 六周期 K 线（定点整数成交量）+ 逐笔成交 + 按区块检查点回滚
│   │
│   ├── mev/                                # MEV 防御
│   │   └── mev.go                          # 三明治检测（按交易方和方向建索引）+ 价格冲击 / 滑点 / deadline 检查
│   │
│   ├── arb/                                # 套利
│   │   └── arb.go                          # 跨池套利扫描 + 三分搜索最优借入量 + 放贷池余额检查
│   │
│   ├── ws/                                 # 实时推送
│   │   └── ws.go                           # Hub 发布-订阅 + SSE + 慢客户端断开
│   │
│   ├── cache/                              # 缓存层
│   │   └── cache.go                        # Singleflight 请求合并 + TTL + 条目数上限
│   │
│   ├── circuitbreaker/                     # 断路器
│   │   └── circuitbreaker.go               # 三态状态机（Closed→Open→HalfOpen）
│   │
│   ├── workerpool/                         # Goroutine 池
│   │   ├── pool.go                         # 固定 Worker + 有界 Channel + 泛型结果
│   │   └── pool_test.go                    # 功能测试 + Benchmark 对比
│   │
│   ├── logging/                            # 结构化日志
│   │   └── logging.go                      # slog + JSON/Text + TraceID（经 HTTP 中间件贯穿请求）
│   │
│   ├── metrics/                            # 可观测性指标
│   │   └── metrics.go                      # Counter/Gauge/Histogram + P99 + /metrics 端点
│   │
│   ├── api/                                # REST API
│   │   ├── server.go                       # 路由注册 + 报价 / 提交 / 状态 / K 线 / 告警等端点
│   │   ├── trade.go                        # 价格安全检查、待签名交易构造、TWAP 与套利查询
│   │   ├── middleware.go                   # Trace / Metrics / 访问日志 / Token Bucket 限流 / 并发信号量 / CORS
│   │   └── types.go                        # 请求/响应类型定义
│   │
│   ├── integration/                        # 端到端集成测试（integration 构建标签）
│   │   ├── harness_test.go                 # 拉起 Hardhat 节点、部署合约、发交易
│   │   └── e2e_test.go                     # 12 个全链路场景
│   │
│   ├── cmd/bench/                          # 压测辅助工具
│   │   └── main.go                         # chase（追块与 Reorg 重建耗时）、sse（推送延迟与慢客户端）
│   │
│   ├── go.mod
│   └── go.sum
│
├── subgraph/                               # The Graph 子图
│   ├── schema.graphql                      # Factory / Pool / Swap / Mint / Burn / Flash / Candle
│   ├── subgraph.yaml                       # 数据源与事件处理器声明
│   ├── src/
│   │   ├── factory.ts                      # PoolCreated → 登记池子 + 启动动态数据源
│   │   └── pool.ts                         # Swap / Mint / Burn / Flash 处理 + 多周期 K 线
│   ├── abis/                               # Factory 和 Pool 的 ABI
│   └── package.json
│
├── bench/                                  # 压测
│   ├── REPORT.md                           # 压测报告（由 report.py 生成）
│   ├── results/                            # 原始结果（k6 JSON、Go 基准输出、追块与 SSE 记录）
│   ├── run-all.sh                          # 一键复现全部压测
│   ├── up.sh / down.sh / backend.sh        # 拉起 / 清理压测环境，按不同配置启动后端
│   ├── run-http.sh                         # 跑 HTTP 场景并用后端指标核对请求数
│   ├── k6/http.js                          # k6 场景脚本（8 个场景）
│   ├── seed.js                             # 灌入 3000 笔历史成交（固定随机种子）
│   └── report.py / report.tmpl             # 由 results/ 生成报告
│
├── README.md                               # 本文件
├── Makefile                                # make compile / test / test-integration / gas / bench / abi / deploy ...
└── .gitignore
```

代码规模（`wc -l`）：合约 3157 行 Solidity（不含测试合约），合约测试 2768 行；后端 8755 行 Go（含压测工具 `cmd/bench` 473 行），后端测试 6181 行（其中集成测试 1568 行）。

