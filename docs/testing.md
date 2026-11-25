# 测试体系

> 小节编号沿用原文：1–13 见 [contracts.md](contracts.md)，14–22 见 [backend.md](backend.md)，23–26 见 [market-mev.md](market-mev.md)，27–32 见 [infrastructure.md](infrastructure.md)；测试见 [testing.md](testing.md)，压测见 [benchmark.md](benchmark.md)，部署与 API 见 [getting-started.md](getting-started.md)。

测试分三层：合约测试验证链上逻辑本身；后端单元测试验证各模块；端到端集成测试把两者接在一个真实节点上，验证“链下算的和链上跑的是同一个结果”。吞吐、延迟和各项保护是否生效另见[压测](benchmark.md#压测)。

### 合约测试（109 个用例）

```bash
cd contracts && npx hardhat test
```

| 测试文件 | 用例数 | 覆盖内容 |
|---------|-------|---------|
| `UniGoPool.test.js` | 24 | 初始化、mint、swap、burn / collect、跨 tick 的手续费分配、Manager 期限与滑点保护、回调权限、闪电贷 |
| `UniGoManager.test.js` | 18 | 多跳 `exactInput`、`exactOutputSingle`、多跳报价 `quoteExactInput`、Pool 真伪校验 |
| `Security.test.js` | 15 | 安全回归：tick 流动性上限、代币兼容性、零流动性闪电贷、套利部分成交、Manager 只接受精确输入 |
| `Math.test.js` | 13 | TickMath 双向转换、BitMath，与已知值和高精度参照实现对照 |
| `Oracle.test.js` | 12 | TWAP：初始化、恒定价格、价格变动加权、查询范围、扩容与环形覆盖 |
| `FlashArbitrage.test.js` | 10 | 套利获利、价格收敛、无利可图时回滚、参数与权限校验 |
| `UniGoFactory.test.js` | 10 | 费率配置、创建池子、重复创建与非法参数、Pool 属性 |
| `Gas.test.js` | 6 | Gas 基准（见 [Benchmark](benchmark.md)） |
| `Invariants.test.js` | 1 | 固定种子的不变量随机测试 |

**固定种子的不变量随机测试**

单元测试只能覆盖想得到的场景。`Invariants.test.js` 用固定种子（默认 42）的伪随机数发生器（mulberry32）对一个池子连续执行 300 步随机操作——mint、burn、双向 swap、精确输出 swap、flash、collect，以及在极端区间 mint 接近 `int128` 上限流动性的攻击性操作——**每一步之后**都检查四条不变量：

| 不变量 | 含义 |
|--------|------|
| 偿付能力 | 池子两种 token 的余额 ≥ 所有 position 的应得本金 + 未领手续费 + 已结算未提取数量 |
| 活跃流动性 | `pool.liquidity` 等于所有覆盖当前 tick 的 position 流动性之和 |
| 手续费单调 | `feeGrowthGlobal` 单调不减 |
| tick 记账 | 每个 tick 的 `liquidityGross / liquidityNet` 与所有 position 的流动性吻合，且不超过单 tick 上限 |

测试还断言每种操作都被执行过、价格确实多次越过区间边界、攻击性的巨额 mint 既有被上限挡住的也有在上限以内如实记账的。最后所有 LP 全部撤出并提取，池子必须付得出每一笔，且只剩舍入产生的零头。种子固定意味着失败可以稳定复现；换种子或加长步数：

```bash
INVARIANT_SEED=7 INVARIANT_STEPS=1000 npx hardhat test test/Invariants.test.js
```

**与 Uniswap V3 已知值对照**

- `TickMath.getSqrtRatioAtTick` 在边界 tick 的返回值与 V3 一致；20 个魔数常量逐个（tick = ±2^i）与高精度计算比对
- `getTickAtSqrtRatio` 与正向函数往返一致，任意价格都满足 `ratio(tick) <= price < ratio(tick + 1)`
- BitMath 的 256 个单比特输入逐一验证，并与参照实现做伪随机对比
- 三个费率档的 `maxLiquidityPerTick` 与 V3 对应池的已知值一致

**安全回归测试**

`Security.test.js` 的每一组对应一类问题，描述的是必须成立的行为：单 tick 流动性上限挡住 `liquidityNet` 回绕攻击（含一条按上限走完整个攻击序列、验证诚实 LP 仍能全额撤出的用例）；无返回值代币和返回 `false` 的代币都被正确处理；活跃流动性为 0 时闪电贷被拒绝；FlashArbitrage 部分成交时整笔回滚且合约不留余额；`Manager.swap` 拒绝精确输出。

### 后端单元测试（16 个包，101 个测试函数）

```bash
cd backend && go test -p 2 ./...
```

| 包 | 测试数 | 重点 |
|----|-------|------|
| `api` | 16 | 报价、交易前保护、验签、待签名交易构造、K 线 / 成交 / 套利 / SSE 端点；按 IP 限流、持续速率与突发、限制可关闭；找不到路由返回 404；Trace ID 传递；HTTP 与缓存指标 |
| `chain` | 13 | 在 go-ethereum 模拟后端上：区块头、事件解码、按区块范围批量拉日志、伪造 `PoolCreated` 被忽略、畸形日志跳过、交易状态、广播、EIP-1559 费用、断路器熔断、索引器跟链并处理 Reorg、路径与调用数据编码 |
| `chain/contractabi` | 2 | 内嵌 ABI 与编译产物一致 |
| `mev` | 11 | 三明治命中 / 不误报 / 经路由合约下单 / 按 logIndex 排序、Reorg 丢弃告警、价格冲击、滑点与 deadline 检查 |
| `oracle` | 10 | Chainlink：配置解析、五项有效性检查、token 顺序与 decimals 换算、按喂价 decimals 归一化、缓存与刷新、坏数据拒绝、三方校验与降级；TWAP 校验、三方交叉验证、池内价格喂价 |
| `kline` | 8 | 多周期聚合、成交量精确、Reorg 重算、回滚超出成交保留范围 / 超出检查点范围、内存上限、并发访问 |
| `math` | 6 | 已知值、往返一致性、`ComputeSwapStep` |
| `router` | 6 | 逐 tick 模拟、tick 查找与位图参照实现一致、精确输出与精确输入互逆、价格限制 |
| `arb` | 5 | 最优借入量、无机会、扣除 gas、机会生命周期、放贷池余额不足时换下一个 |
| `indexer` | 4 | 事件回放、Reorg 回滚并重新索引、回滚分叉上创建的池子 |
| `circuitbreaker` | 4 | 状态转换、恢复、回调 |
| `workerpool` | 4 | 功能测试 |
| `cache` | 3 | 读写与过期、条目数上限、Singleflight |
| `eip712` | 3 | 摘要与标准类型化数据实现一致、验签与防重放、非法字段 |
| `gas` | 3 | 链上费用、缓存、退回静态估算 |
| `ws` | 3 | 推送与类型过滤、慢客户端被断开且不阻塞、关闭时断开客户端 |

另有 11 个 Go 基准函数（`router` 3 个、`workerpool` 3 个、`kline` 2 个、`mev` 2 个、`indexer` 1 个），结果见 [Benchmark](benchmark.md) 和[压测](benchmark.md#压测)。

### 端到端集成测试

```bash
cd backend && go test -p 2 -tags integration -count=1 -timeout 15m ./integration
```

带 `integration` 构建标签，默认的 `go test ./...` 不会跑到它。测试**自己拉起一个 Hardhat 节点**（空闲端口），用编译产物部署 Factory / Manager / Quoter / FlashArbitrage、三种测试代币和一个可控的 Chainlink 聚合器（`TestAggregator`），建 4 个池子、铺 11 个头寸，再按 `main.go` 的装配方式把后端各模块接到这个节点上，起一个 HTTP 服务。运行前需要 `contracts/` 下已安装依赖（`npm install`）。

`TestEndToEnd` 的 12 个场景按顺序在同一条链上执行：

| 场景 | 验证内容 |
|------|---------|
| 索引器状态与链上一致 | 链下重建的每个池子的价格、tick、活跃流动性与链上 `slot0 / liquidity` 完全相等 |
| 链下报价与链上 Quoter 一致 | 60 条单跳 / 多跳报价逐位相等；做几笔真实 swap（含穿越 tick）之后再比一轮 |
| 多跳交易构造、签名、广播并确认 | A → C 选出两跳路径 A→B→C 而不是 1% 的直连池；链上抽检一致；EIP-712 验签后广播；同一签名重放被拒（401）；别人签的 `raw_tx` 冒充 owner 被拒（403）；状态从 `pending_confirmation` 走到 `confirmed`；到手数量与报价逐位一致 |
| 单跳 swap 与 `exactOutputSingle` | 到手 / 付出的数量与链下计算逐位一致 |
| K 线与成交记录 | 多跳交易的第二跳出现在成交记录里；最新成交价等于链上现价；1m / 1h K 线的笔数和成交量等于逐笔合计；SSE 收到推送 |
| TWAP 读数与手算一致及降级 | 在精确时间点做两笔 swap，链上读数等于按持续时间加权的手算平均 tick；观测不足时退到能覆盖的最长窗口；再不足则明确报不可用 |
| 现价偏离 TWAP 时拒绝交易 | 单区块大单把价格砸偏后，构造交易返回 403、报价带警告；价格买回后恢复 |
| 三明治攻击检测 | 关闭自动出块，把“攻击者买入 → 受害者同向买入 → 攻击者反向卖出”三笔交易按小费排进同一区块；产生 1 条告警且内容正确；被夹的池子推荐滑点收紧到 0.1% |
| 套利机会扫描与执行 | 制造价差后扫描器给出机会；零本金账户原样执行调用数据，实际获利与链下预测逐位相等；执行后机会消失 |
| 真实节点上的 Reorg 回滚 | `evm_snapshot` → 出一个带 swap 的区块 → `evm_revert` → 换一笔不同的 swap；索引器检测到 Reorg，状态回到与链上一致，被回滚的成交从成交记录和 K 线里消失 |
| Chainlink 喂价三方校验与降级 | 喂价正常时现价 / Chainlink / TWAP 三方都参与校验并放行；喂价比池价高 10% 时构造交易返回 403；喂价过期、轮次未完成、答案来自旧轮次、价格为负时降级为两方校验并在响应里说明原因；喂价恢复后回到三方 |
| 全部场景之后仍然一致 | 再比一轮状态和 60 条报价；`/metrics` 里链上抽检不一致次数为 0 |

一次实际运行的输出节选：

```
[初始状态] 对比 60 条报价：60 条两边数值逐位一致（其中 11 条穿越了已初始化的 tick，单条最多 4 个）
多跳 A→B→C：投入 5000000000000000000，到手 4996415352936063385，与链下报价逐位一致；gas 实际 199260 / 估算上限 267133
TWAP：tick1=-18 持续 100s，tick2=9 持续 50s，手算平均 tick=-9，链上读数 -9
操纵检测：现价 0.811815，150 秒 TWAP 1.000600，偏离 18.87%，交易被拒绝
套利：借入 34868164334402465845147，发起人获利 1060294620131858209393（与链下预测逐位一致），gas 367570；执行前价差 5.910%，执行后 0.648%
喂价偏离：池价 0.974480，Chainlink 1.071928，偏离 9.09%，交易被拒绝
[全部场景之后] 对比 60 条报价：60 条两边数值逐位一致（其中 13 条穿越了已初始化的 tick，单条最多 5 个）
```

**节点进程的回收**有三层保险（`harness_test.go`）：正常结束时 `TestMain` 关掉节点并等它退出；节点跑在独立进程组里，退出时整组 SIGKILL；测试进程被强杀时，包装脚本读着的管道 EOF，随即杀掉节点，不留孤儿进程。

