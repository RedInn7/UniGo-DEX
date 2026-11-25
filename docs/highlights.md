# 技术亮点总览

每一行对应一个技术点和它所在的代码位置，详细说明见各主题文档。

| 分类 | 技术点 | 核心机制 | 代码位置 |
|------|--------|----------|----------|
| 合约 | Q64.96 定点数 + 512 位精度 | mulmod + 牛顿法模逆 | `contracts/contracts/lib/Math.sol` |
| 合约 | Tick 位图 O(1) 查找 | 256-bit word + MSB/LSB | `contracts/contracts/lib/TickBitmap.sol` |
| 合约 | 查表法 tick↔price 转换 | 20 个预计算魔数常量 | `contracts/contracts/lib/TickMath.sol` |
| 合约 | 回调模式安全转账 | mint/swap/flash callback + 余额校验 + 重入锁 | `contracts/contracts/UniGoPool.sol` |
| 合约 | CREATE2 确定性部署 | salt = keccak256(token0, token1, fee) | `contracts/contracts/UniGoFactory.sol` |
| 合约 | revert-catch 模拟执行 | 自定义错误携带结果 + 多跳报价 | `contracts/contracts/UniGoQuoter.sol` |
| 合约 | 手续费累计与按 position 领取 | feeGrowthGlobal + tick 外侧累计值翻转 + 做差结算 | `contracts/contracts/lib/Tick.sol`, `Position.sol` |
| 合约 | 闪电贷 | 先转出、回调、验余额；手续费归 LP | `contracts/contracts/UniGoPool.sol`（`flash`） |
| 合约 | 期限与滑点保护 | deadline + amountOutMinimum / amountInMaximum | `contracts/contracts/UniGoManager.sol` |
| 合约 | 多跳 exactInput / exactOutputSingle | 紧凑路径编码（20 + 23n 字节）+ Manager 中转 | `contracts/contracts/UniGoManager.sol` |
| 合约 | Pool 真伪校验 | 入口和回调都经 Factory 反查 | `contracts/contracts/UniGoManager.sol` |
| 合约 | TWAP 预言机 | 观测环形数组 + 二分查找 + 线性插值 | `contracts/contracts/lib/Oracle.sol` |
| 合约 | 闪电贷套利 | 第三个池子放贷 + 两次 swap + minProfit | `contracts/contracts/FlashArbitrage.sol` |
| 合约 | tick 流动性上限 | uint128 容量按 tick 数均分 + 受检运算 | `contracts/contracts/lib/Tick.sol` |
| 合约 | 兼容不规范代币 | SafeERC20 | `contracts/contracts/UniGoPool.sol`, `UniGoManager.sol` |
| 子图 | 事件索引 + 多周期 K 线 | 动态数据源 + 1m/5m/1h/1d Candle 实体 | `subgraph/src/pool.ts`, `subgraph/schema.graphql` |
| 后端 | 链下数学精确复现 | math/big + 相同魔数常量和取整方向 | `backend/math/` |
| 后端 | 链交互层 | go-ethereum + 由编译产物生成并内嵌的 ABI | `backend/chain/`, `backend/chain/contractabi/` |
| 后端 | 链数据源接口 | ChainSource：真实节点 / 模拟源可替换 | `backend/indexer/indexer.go` |
| 后端 | 分段追块 | 每 500 个区块一次 eth_getLogs + 逐条核对日志的区块哈希 | `backend/indexer/indexer.go`, `backend/chain/events.go` |
| 后端 | Reorg 检测 + 状态回滚 | parentHash 校验 + 找分叉点 + 撤销日志逆序回放 | `backend/indexer/indexer.go` |
| 后端 | 逐 tick 精确的链下 swap 模拟 | 复现 swap 主循环，在相同的 word 边界切步 | `backend/router/quote.go` |
| 后端 | 路由搜索 + 200ms 超时降级 | DFS 枚举 + Worker Pool 并发评估 + 直连池 fallback | `backend/router/router.go` |
| 后端 | Singleflight 防缓存击穿 | sync.WaitGroup 请求合并 + 按区块号失效 + 条目数上限 | `backend/cache/cache.go` |
| 后端 | 读取链上 Chainlink 喂价 | latestRoundData 五项有效性检查 + 口径换算 + 缓存刷新 | `backend/oracle/chainlink.go` |
| 后端 | 三方价格交叉校验 | 现价 / TWAP / Chainlink + 偏离拒绝 + 不可用时降级为两方 + 保护模式 | `backend/oracle/oracle.go`, `backend/api/trade.go` |
| 后端 | 待签名交易构造 | 路由 → Manager 调用数据 + 链上报价抽检 | `backend/api/trade.go`, `backend/chain/contracts.go` |
| 后端 | 交易状态查询 | 回执 + 确认数：pending → confirmed / reverted | `backend/chain/client.go` |
| 后端 | EIP-712 三层防重放 | Deadline + Nonce + DomainSeparator，拒绝高 s 签名 | `backend/eip712/eip712.go` |
| 后端 | EIP-1559 Gas 成本评估 | baseFee + priorityFee，静态值兜底 | `backend/gas/gas.go` |
| 后端 | K 线与成交 | 六周期聚合 + 定点整数成交量 + 按区块检查点回滚 | `backend/kline/kline.go` |
| 后端 | 三明治攻击检测 | 同块同池：抢跑 → 受害者同向 → 反向平仓；按交易方和方向建索引，单笔 O(log n) | `backend/mev/mev.go` |
| 后端 | 套利机会扫描 | 逐 tick 模拟的利润函数 + 三分搜索最优借入量 + 放贷池余额检查 | `backend/arb/arb.go` |
| 后端 | SSE 实时推送 | Hub 发布-订阅 + 非阻塞扇出 + 慢客户端断开 | `backend/ws/ws.go` |
| 后端 | 断路器（三态状态机） | Closed→Open→HalfOpen + 探测恢复 | `backend/circuitbreaker/` |
| 后端 | Goroutine 池化 | 固定 Worker + 有界 Channel 复用 | `backend/workerpool/pool.go` |
| 后端 | 结构化日志 + 链路追踪 | slog + X-Trace-Id 中间件 + context 传播 | `backend/logging/logging.go`, `backend/api/middleware.go` |
| 后端 | Prometheus 风格指标 | Counter/Gauge/Histogram + P99 | `backend/metrics/metrics.go` |
| 后端 | Token Bucket + 并发信号量 | 每 IP 100 请求/秒、突发 200 + 全局 503 背压 | `backend/api/middleware.go` |
| 测试 | 不变量随机测试 | 固定种子 300 步随机操作，每步检查 4 条不变量 | `contracts/test/Invariants.test.js` |
| 测试 | 端到端集成测试 | 自起 Hardhat 节点，12 个全链路场景 | `backend/integration/` |
| 压测 | HTTP 吞吐与保护生效 | k6 八个场景 + 后端指标交叉核对请求数 | `bench/`, `bench/REPORT.md` |
| 压测 | 追块、Reorg、SSE | 对本地链追块与回滚耗时、推送延迟与慢客户端隔离 | `backend/cmd/bench/` |
