# Go 后端：核心业务

> 小节编号沿用原文：1–13 见 [contracts.md](contracts.md)，14–22 见 [backend.md](backend.md)，23–26 见 [market-mev.md](market-mev.md)，27–32 见 [infrastructure.md](infrastructure.md)；测试见 [testing.md](testing.md)，压测见 [benchmark.md](benchmark.md)，部署与 API 见 [getting-started.md](getting-started.md)。

### 14. 链下数学库（链上链下一致性）

**核心问题**：链下路由引擎需要模拟 Swap，预估输出数量。如果链下计算与链上不一致（哪怕差 1 wei），按链下报价算出来的 `amountOutMinimum` 就可能让交易在链上 revert。

**Go 的 `math/big` 天然支持任意精度整数**，与 Solidity 的 `uint256` 对应。`backend/math` 把合约数学库逐个搬到链下：

| Solidity | Go | 说明 |
|----------|----|------|
| `TickMath.getSqrtRatioAtTick` | `GetSqrtRatioAtTick` | 相同的 20 个魔数常量，逐位相乘后右移 128 位 |
| `TickMath.getTickAtSqrtRatio` | `GetTickAtSqrtRatio` | 浮点近似 + 用精确的正向函数修正到边界成立 |
| `SqrtPriceMath.getAmount0Delta / getAmount1Delta` | `GetAmount0Delta / GetAmount1Delta` | 带取整方向参数 |
| `SqrtPriceMath.getNextSqrtPriceFromInput / Output` | `GetNextSqrtPriceFromInput / Output` | 包括 `amount × sqrtP` 溢出 uint256 时换公式的分支 |
| `SwapMath.computeSwapStep` | `ComputeSwapStep` | 逐行对应 |
| `Math.mulDiv / mulDivRoundingUp` | `MulDiv / MulDivRoundingUp` | 大整数直接乘除，不需要手工处理 512 位 |

```go
// tickmath.go — 与 Solidity 完全一致的查表法
func GetSqrtRatioAtTick(tick int32) (*big.Int, error) {
    absTick := int64(tick)
    if absTick < 0 {
        absTick = -absTick
    }
    if absTick > int64(MaxTick) {
        return nil, errors.New("tick out of range")
    }
    absTickBig := big.NewInt(absTick)

    // 初始化 ratio（Q128.128 格式）：第 0 位为 1 用第一个魔数，否则是 1.0
    ratio := new(big.Int)
    if absTickBig.Bit(0) == 1 {
        ratio.Set(magicNumbers[0])
    } else {
        ratio.Set(baseRatio)
    }

    // 原理：|tick| = Σ(bit_i × 2^i)，所以 √1.0001^|tick| = Π(√1.0001^(2^i))^bit_i
    for i := 1; i < 20; i++ {
        if absTickBig.Bit(i) == 1 {
            ratio.Mul(ratio, magicNumbers[i])
            ratio.Rsh(ratio, 128)
        }
    }

    // 魔数是倒数形式，tick 为正时再取一次倒数
    if tick > 0 {
        ratio.Div(MaxUint256, ratio)
    }

    // Q128.128 → Q64.96：右移 32 位并向上取整（与链上相同）
    mod := new(big.Int).And(ratio, mask32)   // mask32 = 2^32 - 1
    result := new(big.Int).Rsh(ratio, 32)
    if mod.Sign() > 0 {
        result.Add(result, big.NewInt(1))
    }
    return result, nil
}
```

**取整方向是一致性的关键**。`ComputeSwapStep` 里输入一律向上取整、输出一律向下取整（池子永远不吃亏），输入不够走到目标价格时，剩余的输入全部算作手续费：

```go
// sqrtpricemath.go — ComputeSwapStep 结尾
if exactIn && !reachedTarget {
    // 没走到目标价格：剩下的输入全部算作手续费
    feeAmount = new(big.Int).Sub(absRemaining, amountIn)
} else {
    feeAmount = MulDivRoundingUp(amountIn, fee, feeComplement)
}
```

链上 `uint160` 的截断、`require` 失败这些边界也要搬过来：链上会 revert 的输入，链下返回 `ErrSwapMath`，不会算出一个链上不存在的结果。

**一致性验证**分三层：

1. `tickmath_test.go`：用 Uniswap V3 的已知值（tick 0、±1、±100、MinTick、MaxTick 的 `sqrtPriceX96`）逐位比对，并做 tick → price → tick 的往返测试
2. `router/quote_test.go`：`TestNextInitializedTickMatchesBitmap` 用随机 tick 集合对比排序数组实现与位图参照实现
3. 端到端集成测试：对真实节点上的池子，链下报价与链上 `UniGoQuoter` 的返回值逐位比对（见[测试体系](testing.md)）

**代码位置**：`backend/math/tickmath.go`, `backend/math/sqrtpricemath.go`, `backend/math/tickmath_test.go`

---

### 15. chain 包：基于 go-ethereum 的链交互层

**问题**：后端要读区块、拉日志、解码事件、调合约、广播交易。这些如果散落在各模块里各写一套 JSON-RPC，事件签名和 ABI 编码很容易和合约对不上。

`backend/chain` 把所有链交互收拢到一个 `Client`，底层是 go-ethereum 的 `ethclient`：

| 能力 | 方法 | 用到它的模块 |
|------|------|-------------|
| 区块头（hash / parentHash / timestamp） | `LatestBlock` `BlockByNumber` | 索引器 |
| 按区块范围批量拉日志并解码成事件 | `BlockEvents` | 索引器 |
| 合约只读调用 | `Slot0` `PoolLiquidity` `TWAP` `QuoteExactInput` | TWAP 校验、链上报价抽检 |
| Chainlink 聚合器、ERC20 余额 | `ChainlinkDecimals` `ChainlinkLatestRound` `TokenBalance` | 价格校验、套利扫描 |
| 调用数据编码 | `EncodePath` `EncodeSwap` `EncodeExactInput` `EncodeExactOutputSingle` `EncodeArbitrage` | 待签名交易、套利扫描 |
| 交易 | `SendRawTransaction` `InspectRawTx` `TxStatus` `EstimateGas` | 提交交易、状态查询 |
| EIP-1559 费用 | `Fees` | Gas 成本评估 |

`Client` 只依赖一个由 go-ethereum 标准接口拼成的 `Backend` 接口，真实节点的 `*ethclient.Client` 和 go-ethereum 的模拟后端（`ethclient/simulated`）都满足它，所以 chain 包的单元测试不需要起节点。

**ABI 由编译产物生成并内嵌**

事件签名、函数选择器、结构体参数的编码全部来自合约 ABI，不手写：

```go
// chain/contractabi/contractabi.go
//go:generate go run gen.go

var (
    //go:embed UniGoPool.json
    poolJSON string
    // ... UniGoFactory / UniGoManager / UniGoQuoter / FlashArbitrage / AggregatorV3Interface / IERC20

    Pool    = mustParse(poolJSON)
    Factory = mustParse(factoryJSON)
    // ...
)
```

`gen.go` 从 Hardhat 编译产物（`contracts/artifacts`）里只取 `abi` 字段写成 JSON，`go:embed` 把它编进二进制。合约接口改了之后重新生成即可：

```bash
cd contracts && npx hardhat compile
cd ../backend && go generate ./chain/...
```

`contractabi_test.go` 的 `TestEmbeddedABIMatchesArtifacts` 会把内嵌的 ABI 与编译产物比对，合约改了而 ABI 没重新生成时测试直接失败。

**事件解码**

```go
// chain/events.go — 关心的事件签名取自 ABI，用作 FilterLogs 的 topic0 过滤
var eventTopics = func() []common.Hash {
    var topics []common.Hash
    for _, name := range []string{"Initialize", "Mint", "Burn", "Swap", "Collect", "Flash"} {
        topics = append(topics, contractabi.Pool.Events[name].ID)
    }
    return append(topics, contractabi.Factory.Events["PoolCreated"].ID)
}()
```

日志的 `topics[0]` 是事件签名哈希，indexed 参数在 `topics[1:]`，其余参数 ABI 编码在 `data` 里。解码时有三个防御点：

- **核对日志的区块哈希**：日志按区块号范围批量拉取，拿到的是节点当前主链上的日志，所以逐条核对 `blockHash` 与索引器手里的区块头一致；对不上说明这期间链重组了，返回 `ErrReorged` 让调用方整段重读
- **只信任 Factory 发出的 `PoolCreated`**：否则任何合约都能发一条同签名的日志伪造池子
- **畸形日志跳过不中断**：签名相同但内容解不出来的日志记一条警告后跳过，不能卡住索引

**所有 RPC 调用都经过断路器**（第 27 节）。这里有一个细节：只有节点不可达才计入失败次数，“查不到”和节点明确返回的 JSON-RPC 错误说明节点是活的：

```go
// chain/client.go
func call[T any](c *Client, fn func() (T, error)) (T, error) {
    var (
        value    T
        answered error // 节点给出的否定应答
    )
    _, err := c.cb.Execute(func() (interface{}, error) {
        var err error
        value, err = fn()
        var rpcErr rpc.Error
        if errors.Is(err, ethereum.NotFound) || errors.As(err, &rpcErr) {
            answered = err
            return nil, nil // 不计入断路器失败
        }
        return nil, err
    })
    if err == nil {
        err = answered
    }
    return value, err
}
```

否则用户提交几笔会 revert 的交易，就能把索引器一起熔断。

**代码位置**：`backend/chain/client.go`, `backend/chain/contracts.go`, `backend/chain/events.go`, `backend/chain/contractabi/`

---

### 16. 事件索引器：链数据源接口与 Reorg 回滚

**问题**：区块链的最终性不是即时的。一个区块被打包后仍然可能被撤销（Reorg），基于该区块做的所有链下状态更新都要跟着撤销，否则链下报价用的就是一条不存在的链上的状态。

**链数据源接口**

索引器只依赖一个三方法的接口，不关心区块从哪来：

```go
// indexer/indexer.go
type ChainSource interface {
    LatestBlock(ctx context.Context) (uint64, error)
    BlockByNumber(ctx context.Context, number uint64) (*BlockInfo, error)   // hash / parentHash / timestamp
    // 一段连续区块内已解码的事件：结果与 blocks 一一对应，每个区块内按 logIndex 升序
    BlockEvents(ctx context.Context, blocks []*BlockInfo) ([][]interface{}, error)
}
```

- 配置了 `RPC_URL` 和 `FACTORY_ADDRESS` 且节点连得上：数据源是 `chain.Client`，区块哈希、父哈希、事件全部来自真实节点
- 否则：数据源是内置的模拟源（`sim_source.go`），只出父哈希衔接的空区块，保证后端不配节点也能启动

**按区块范围批量拉取**

每个区块发一次 `eth_getLogs` 的话，追 N 个区块就要 2N 次 RPC 往返（区块头 + 日志）。索引器把待追的区块按 `MaxBlockRange`（默认 500，多数节点服务商对单次 `eth_getLogs` 的范围有限制）分段，每段：

1. 逐个取区块头，并检查段内每个区块的 `parentHash` 与前一个接得上
2. 第一个区块的 `parentHash` 与本地记录的上一个区块比对（Reorg 检测，见下）
3. **一次** `eth_getLogs` 拉取整段的事件，按区块分好
4. 按区块顺序应用事件、记录哈希链

区块头和日志是分两步读的，中间链可能重组。数据源逐条核对日志所属区块的哈希与第 1 步拿到的区块头一致，不一致就报错，这一段整体作废、下一轮重读，不会把两条分叉上的数据拼在一起：

```go
// chain/events.go — BlockEvents
block := blocks[lg.BlockNumber-first]
if lg.BlockHash.Hex() != block.Hash {
    return nil, fmt.Errorf("%w: 区块 %d", ErrReorged, lg.BlockNumber)
}
```

对本地 Hardhat 节点从创世块追到链头（680 个区块、3036 个事件，带全部订阅者）中位耗时 193 ms，约 3,517 区块/秒（见[压测](benchmark.md#压测)）。

**事件驱动的状态重建**

`PoolStateManager` 在内存里维护每个池子的 `sqrtPriceX96 / tick / liquidity` 和所有已初始化 tick 的 `liquidityNet`，完全由事件回放得到：

| 事件 | 对链下状态的影响 |
|------|-----------------|
| `PoolCreated`（仅 Factory 发出的） | 登记新池子 |
| `Initialize` | 设定初始价格和 tick |
| `Mint` / `Burn` | 更新上下界 tick 的 `liquidityGross / liquidityNet`；区间覆盖现价时增减活跃流动性 |
| `Swap` | 用事件里的成交后价格、tick、活跃流动性覆盖当前值，并推送给订阅者 |
| `Collect` / `Flash` | 不改变价格和流动性，只计数 |

**Reorg 检测：父块哈希校验**

索引器保留最近 `2 × CONFIRMATION_BLOCKS` 个区块的哈希。处理新区块时，先比对它的 `parentHash` 与本地记录的上一个区块的哈希：

```
本地已索引：
  Block 100: hash=0xAAA
  Block 101: hash=0xBBB
  Block 102: hash=0xCCC

节点切到了另一条分叉，区块 102 被替换，新链继续出块：
  Block 103': parentHash=0xDDD   ← 与本地记录的 102（0xCCC）对不上
       ↓
  检测到 Reorg → 向前逐块比对本地哈希与链上哈希，找到第一个不一致的区块（分叉点 = 102）
       ↓
  撤销 >= 102 的全部状态变更 → 从 102 开始重新索引
```

**回滚实现：撤销日志**

只把“最后更新区块号”改回去是不够的：被回滚的 Mint 留下的流动性、被回滚的 Swap 推过的价格都还在。索引器在应用每个事件的同时记下一条撤销动作：

```go
// indexer/indexer.go — handleEvent
case *MintEvent:
    pool.ApplyMint(e)
    idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() {
        pool.ApplyBurn(&BurnEvent{TickLower: e.TickLower, TickUpper: e.TickUpper, Amount: e.Amount, BlockNumber: e.BlockNumber})
    }})
case *SwapEvent:
    before := pool.Snapshot()           // 成交前的价格 / tick / 活跃流动性
    pool.ApplySwap(e)
    idx.undoLog = append(idx.undoLog, undoEntry{e.BlockNumber, func() {
        pool.ApplySwap(&SwapEvent{SqrtPriceX96: before.SqrtPriceX96, Liquidity: before.Liquidity, Tick: before.Tick, BlockNumber: e.BlockNumber})
    }})
```

| 事件 | 撤销动作 |
|------|---------|
| `PoolCreated` | 移除池子 |
| `Initialize` | 价格清零（回到未初始化） |
| `Mint` | 应用一次等量的 Burn |
| `Burn` | 应用一次等量的 Mint |
| `Swap` | 恢复成交前的价格、tick、活跃流动性 |

发现 Reorg 后**逆序**执行分叉点之后的撤销动作（逆序很重要：Mint 的撤销要不要动活跃流动性，取决于当时的 tick，必须先把后面的 Swap 撤掉）：

```go
// indexer/indexer.go — handleReorg
keep := len(idx.undoLog)
for keep > 0 && idx.undoLog[keep-1].block >= forkBlock {
    keep--
    idx.undoLog[keep].undo()
}
idx.undoLog = idx.undoLog[:keep]

for _, l := range idx.listeners {
    l.OnReorg(forkBlock)   // K 线、MEV 告警、套利机会各自丢弃分叉后的数据
}
```

撤销日志只保留回滚窗口（`2 × CONFIRMATION_BLOCKS` 个区块）内的条目，更老的变更不可能再被回滚，随处理进度丢弃。

**Swap 事件订阅**

事件处理和回滚在同一个协程里顺序执行，保证状态变更的顺序与链上一致。其它模块通过 `SwapListener` 接口挂在这条流上：

```go
type SwapListener interface {
    OnSwap(event *SwapEvent)     // 每处理完一笔已知池子的 Swap 调用一次
    OnReorg(forkBlock uint64)    // 区块号 >= forkBlock 的数据已被回滚，随后会重新推送
}
```

`main.go` 里注册了四个订阅者：预言机的池内价格喂价、K 线聚合器、三明治检测、套利扫描。

**验证**：`indexer_test.go` 用可编程的假数据源构造分叉；`chain_test.go` 在 go-ethereum 模拟后端上做真实的链重组，并验证跨区块范围的批量拉取（`TestBlockEventsRange`）；集成测试在 Hardhat 节点上用 `evm_snapshot / evm_revert` 制造 Reorg，断言回滚后链下状态与链上 `slot0 / liquidity` 完全一致。

**代码位置**：`backend/indexer/indexer.go`, `backend/indexer/pool_state.go`, `backend/indexer/sim_source.go`, `backend/indexer/events.go`

---

### 17. 链下 swap 模拟与 Smart Order Router

#### 逐 tick 精确的链下模拟

**问题**：调用链上 Quoter 报价要走一次 RPC 和一次 EVM 执行，路由引擎比较几十条候选路径时不可能每条都问链上。链下模拟必须快，而且结果要和链上**逐位一致**，否则算出的滑点保护是错的。

`router.SimulateSwap` 在索引器重建的池子快照上复现 `UniGoPool.swap` 的主循环，参数含义与链上相同（`zeroForOne`、带符号的 `amountSpecified`、`sqrtPriceLimitX96`）：

```go
// router/quote.go
for remaining.Sign() != 0 && sqrtPrice.Cmp(sqrtPriceLimitX96) != 0 {
    tickNext, liquidityNet := nextInitializedTickWithinOneWord(pool.Ticks, tick, pool.TickSpacing, zeroForOne)
    // ... tickNext 限制在 [MinTick, MaxTick]，算出 sqrtPriceNext

    // 本步的目标价格：下一个 tick，或者价格限制（哪个先到算哪个）
    sqrtPrice, amountIn, amountOut, feeAmount, err = umath.ComputeSwapStep(sqrtPrice, target, liquidity, remaining, pool.Fee)

    if sqrtPrice.Cmp(sqrtPriceNext) == 0 {
        if liquidityNet != nil {            // 走到了已初始化的 tick：穿越它，调整活跃流动性
            if zeroForOne {
                liquidity = new(big.Int).Sub(liquidity, liquidityNet)
            } else {
                liquidity = new(big.Int).Add(liquidity, liquidityNet)
            }
            crossed++
        }
        // tick 更新规则与链上相同：zeroForOne 方向减 1
    }
}
```

**为什么连“在哪里切步”都要和链上一样**

链上用位图查找下一个 tick，每次只在一个 word（256 个 tickSpacing）内找，找不到就停在 word 边界再继续。每一步的输入向上取整、输出向下取整，所以同一笔 swap 切成 2 步和切成 3 步，最终数量可能差 1 wei。链下虽然用排序数组而不是位图，但 `nextInitializedTickWithinOneWord` 会在同样的 word 边界停下：

```go
// 向右找：从下一个压缩 tick 开始，且不超过它所在 word 的终点
start := compressed + 1
bitPos := start & 255
wordEnd := (start + 255 - bitPos) * tickSpacing
i := sort.Search(len(ticks), func(i int) bool { return ticks[i].Tick >= start*tickSpacing })
if i < len(ticks) && ticks[i].Tick <= wordEnd {
    return ticks[i].Tick, ticks[i].LiquidityNet
}
return wordEnd, nil   // 本 word 内没有已初始化的 tick：停在边界
```

在此之上封装了两个报价函数，语义分别与合约对齐：

| 函数 | 等价的链上行为 | 成交不完时 |
|------|---------------|-----------|
| `QuoteExactInputSingle` | `Manager.exactInput` / `Quoter.quoteExactInput` 的一跳 | 返回 `ErrInsufficientLiquidity`（链上 revert `PF`） |
| `QuoteExactOutputSingle` | `Manager.exactOutputSingle` | 同上 |

**与链上 Quoter 逐位一致**：集成测试在 4 个池子（含同币对不同费率、多个价格区间的头寸）上取 60 条单跳和多跳报价，分别在初始状态、若干笔真实 swap 之后、全部场景跑完之后各比一轮，每轮 60 条两边数值全部相等，其中 11 ~ 13 条穿越了已初始化的 tick（单条最多 5 个）。生产路径上也留了抽检：`POST /api/v1/swap/build` 在配置了 `QUOTER_ADDRESS` 时会用同一路径问一次链上 Quoter，结果写进响应的 `quote_consistent`，偏差计入 `quote_onchain_mismatch_total` 和 `quote_onchain_deviation_bps` 指标。

#### 路由搜索与二级降级

**问题**：极端行情下大量 Swap 请求并发触发多跳路径搜索，CPU 飙升。如果所有请求都坚持寻找最优路径，系统会雪崩。

**核心设计：Fail-soft 而非 Fail-fast**。超时后不报错，而是自动降级到更简单的路径：

```
请求进入
    │
    ▼
┌───────────────────────────────────────┐
│ 背压控制：信号量 chan struct{} (200)   │
│ 获取失败？→ 直接降级，不排队           │
└───────────┬───────────────────────────┘
            │ 获取成功
            ▼
┌───────────────────────────────────────┐
│ Level 1：多跳搜索                      │
│ context.WithTimeout(200ms)            │
│  ① DFS 枚举候选路径                    │
│     最多 3 跳、不重复经过同一 token     │
│     同币对不同费率的池子算不同路径      │
│     最多 64 条候选                     │
│  ② Worker Pool 并发评估                │
│     每条路径逐池逐 tick 模拟产出        │
│  ③ 取最终产出最高的一条                │
│                                       │
│ 超时？→ 进入 Level 2                   │
└───────────┬───────────────────────────┘
            │ 搜索成功
            ▼
┌───────────────────────────────────────┐
│ Gas 预估                               │
│ 每跳 150000 + 每穿越一个 tick 25000    │
│ ticksCrossed > 50？→ 高 Gas 警告日志   │
└───────────────────────────────────────┘

Level 2 降级路径：
┌───────────────────────────────────────┐
│ 只在 tokenIn/tokenOut 的直连池里选      │
│ （可能有多个费率档，取产出最高的）       │
│ 标记 IsDegraded = true                │
└───────────────────────────────────────┘
```

枚举只看图结构，很快；评估是 CPU 密集的大数运算，交给 Goroutine 池（第 28 节）并发执行。池的队列满时在当前协程里直接算，不丢候选：

```go
// router/router.go — searchRoutes
for i, c := range candidates {
    evaluate := func() {
        defer wg.Done()
        if ctx.Err() == nil {
            routes[i] = evaluatePath(c, tokenIn, amountIn)
        }
    }
    wg.Add(1)
    if !r.evalPool.Submit(evaluate) {
        evaluate()
    }
}
wg.Wait()
```

降级只发生在两种情况：并发搜索数已达上限，或搜索超时。搜索正常跑完但没有可成交的路径时不降级——直连池本来就在候选里，降级不会有不同的结果——直接返回错误，API 层映射为 404。

Go 基准（`router/bench_test.go`）量了这两层的开销：单池模拟一笔不跨 tick 的 swap 约 4.1 µs，穿越 20 个 tick 约 40 µs；64 条三跳候选路径串行评估 1,524 µs，交给 Worker Pool 并发评估 768 µs（1.98×）。完整数据见[压测](benchmark.md#压测)。

精确输出走 `FindExactOutputRoute`：在直连池里找投入最少的那个（与合约的 `exactOutputSingle` 对应，只支持单池）。

**代码位置**：`backend/router/quote.go`, `backend/router/router.go`

---

### 18. 缓存 Singleflight 防击穿

**问题**：缓存失效瞬间，大量并发请求同时穿透缓存，各自重复执行同一次昂贵的加载（路由搜索、对节点的 TWAP 查询），造成瞬时过载。

**三种缓存异常的区分**：

| 异常类型 | 原因 | 表现 | 解决方案 |
|---------|------|------|---------|
| 缓存穿透 | 查询不存在的 Key | 每次请求都打到后端 | 布隆过滤器 / 空值缓存 |
| 缓存击穿 | 热点 Key 过期 | 大量并发请求同时重建 | **Singleflight** |
| 缓存雪崩 | 大量 Key 同时过期 | 全面击穿 | 随机 TTL / 预热 |

**Singleflight 请求合并的实现**：

```go
// cache.go — 核心代码
func (c *Cache) GetOrLoad(key string, loader func() (interface{}, error)) (interface{}, error) {
    // 1. 先查缓存（命中直接返回）
    if val, ok := c.Get(key); ok {
        return val, nil
    }

    // 2. Singleflight：检查是否有正在进行的加载
    c.flightsMu.Lock()
    if f, ok := c.flights[key]; ok {
        c.flightsMu.Unlock()
        f.wg.Wait()         // 等待先行请求完成
        return f.val, f.err  // 直接复用结果（不再调用 loader）
    }

    // 3. 本请求成为 "leader"，创建新的 flight
    f := &flight{}
    f.wg.Add(1)
    c.flights[key] = f
    c.flightsMu.Unlock()

    // 4. 只有 leader 执行实际加载
    f.val, f.err = loader()
    if f.err == nil {
        c.Set(key, f.val)  // 加载成功，写入缓存
    }
    f.wg.Done()  // 5. 通知所有等待者

    // 6. 清理 flight（允许后续请求重新加载）
    c.flightsMu.Lock()
    delete(c.flights, key)
    c.flightsMu.Unlock()

    return f.val, f.err
}
```

**具体场景示例**：

```
时间线：
  T=0ms   请求 A 查询 "ETH/USDC" 报价，缓存 miss → 创建 flight，开始加载
  T=1ms   请求 B 查询 "ETH/USDC" 报价，缓存 miss → 发现 flight 存在，等待
  T=2ms   请求 C 查询 "ETH/USDC" 报价，缓存 miss → 发现 flight 存在，等待
  ...
  T=50ms  请求 A 加载完成 → 写入缓存 → 通知 B 和 C
  T=50ms  请求 B 和 C 同时拿到结果返回

没有 Singleflight：3 个请求 = 3 次 RPC 调用
有 Singleflight：3 个请求 = 1 次 RPC 调用
```

**按区块失效**

报价的缓存 key 带上索引器的最新区块号。池子状态只在新区块到来时变化，同一区块内重复报价直接命中缓存，出了新区块自动失效，不会返回过期报价：

```go
// api/server.go — findRoute
cacheKey := fmt.Sprintf("quote:%s:%s:%s:%d", tokenIn, tokenOut, amountIn.String(), s.idxer.GetStats().LastBlockNumber)
result, err := s.cache.GetOrLoad(cacheKey, func() (interface{}, error) {
    return s.router.FindBestRoute(ctx, tokenIn, tokenOut, amountIn)
})
```

TWAP 读数也用同样的方式缓存（key 为 `twap:{池子}:{区块号}`），一个区块内对同一个池子只问节点一次。

**条目数上限**

缓存 key 里带着请求参数（报价金额）。不设上限的话，一直变换金额的请求会让缓存在 TTL（30 秒）到期之前无限增长。缓存有条目数上限（默认 10000），超过后按写入顺序淘汰最早的条目：

```go
// cache/cache.go — SetWithTTL
if _, exists := c.data[key]; !exists {
    c.order = append(c.order, key)        // 各 key 首次写入的顺序，队头最早
}
c.data[key] = entry

for len(c.data) > c.maxEntries && len(c.order) > 0 {
    delete(c.data, c.order[0])
    c.order = c.order[1:]
}
```

压测里“每个请求一个新金额”的场景下，加上限后后端常驻内存从 762.4 MB 降到 69.1 MB（见[压测](benchmark.md#压测)）。

存储是 map + 读写锁，后台协程按 TTL 周期清理过期条目；命中和未命中次数计入 `cache_hits_total` / `cache_misses_total`。

**代码位置**：`backend/cache/cache.go`, `backend/cache/cache_test.go`, `backend/api/server.go`

---

### 19. 价格安全：现价 / TWAP / Chainlink 三方交叉校验

**核心原则：不盲信任何单一价格源。**

| 价格源 | 特点 | 弱点 |
|--------|------|------|
| 池内现价（`sqrtPriceX96`） | 实时 | 一笔大单（或闪电贷）就能在单个区块内拉偏 |
| 池内 TWAP（第 10 节） | 要整个窗口都顶住才会动，操纵成本高 | 对真实的快速行情反应慢；和现价来自同一个池子 |
| Chainlink 喂价 | 独立于本池子 | 有心跳间隔，可能过期或停更 |

三者两两比较，各管一类风险：

```
                 ┌──────────────┐
                 │  Chainlink   │
                 └──┬────────┬──┘
        偏离 > 2%   │        │   偏离 > 2%
        拒绝交易    │        │   保护模式（不是瞬时波动）
              ┌─────▼──┐  ┌──▼─────┐
              │ 池内现价 │◄─►│池内 TWAP│
              └────────┘  └────────┘
                  偏离 > 5%：疑似单区块操纵
                  拒绝交易
```

每次报价、构造交易、提交交易，都对路由经过的每个池子做一遍校验（`checkPool`）：先读 Chainlink，再读 TWAP，两者都记入预言机后一起做三方交叉验证。

#### 读取链上 Chainlink 喂价

喂价直接读链上聚合器合约（`AggregatorV3Interface` 的 `latestRoundData()` 和 `decimals()`），按币对配置：

```
CHAINLINK_FEEDS=base:quote:聚合器地址:baseDecimals:quoteDecimals[:心跳秒数]     多个喂价用逗号分隔
```

**有效性检查**。`latestRoundData()` 永远会返回一组数，但这组数不一定能用：

```go
// oracle/chainlink.go
func ValidateRound(round *chain.ChainlinkRound, heartbeat time.Duration) error {
    switch {
    case round.Answer == nil || round.Answer.Sign() <= 0:
        return errors.New("answer 不是正数")
    case round.UpdatedAt == 0:
        return errors.New("轮次未完成（updatedAt = 0）")
    case round.AnsweredInRound.Cmp(round.RoundID) < 0:
        return fmt.Errorf("答案来自旧轮次（answeredInRound %s < roundId %s）", round.AnsweredInRound, round.RoundID)
    case round.UpdatedAt > round.BlockTime:
        return errors.New("updatedAt 晚于最新区块时间")
    case round.BlockTime-round.UpdatedAt > uint64(heartbeat/time.Second):
        return fmt.Errorf("喂价已过期：%d 秒未更新（心跳阈值 %d 秒）", ...)
    }
    return nil
}
```

过期判断用的是**链上最新区块的时间**，不是本机时钟：本机时钟和链上时间可能有偏差，测试链的时间还可以被任意拨动。心跳阈值按喂价配置，缺省 1 小时。

**口径换算**（最容易错的地方）。喂价是“1 个 base 值多少 quote”，按人类单位计，再乘以 `10^feedDecimals`；池子价格是 `token1 最小单位 / token0 最小单位`，未按 decimals 调整，而且哪个币是 token0 由地址大小决定：

```
P = answer / 10^feedDecimals                          例：ETH/USD answer=200000000000, decimals=8 → P = 2000

base 是 token0：poolPrice = P × 10^(quoteDecimals - baseDecimals)
base 是 token1：poolPrice = (1/P) × 10^(baseDecimals - quoteDecimals)

例：WETH(18 位) / USDC(6 位)，P = 2000
  WETH 是 token0：1 wei WETH 值 2000 × 10^(6-18) = 2e-9 个 USDC 最小单位
  USDC 是 token0：1 个 USDC 最小单位值 (1/2000) × 10^(18-6) = 5e8 wei WETH
```

```go
func (f FeedConfig) PoolPrice(humanPrice float64, token0 string) float64 {
    if token0 == f.Base {
        return humanPrice * math.Pow10(int(f.QuoteDecimals)-int(f.BaseDecimals))
    }
    return 1 / humanPrice * math.Pow10(int(f.BaseDecimals)-int(f.QuoteDecimals))
}
```

**缓存与刷新**。每个喂价的读数缓存 `CHAINLINK_REFRESH_SECONDS`（默认 15 秒），过期后由下一次查询触发刷新，同一个喂价同时只有一个刷新请求在途；`decimals` 不会变，只读一次；RPC 经过 chain 包的断路器。节点查询失败时不缓存、也不沿用旧读数——旧读数是否还在心跳之内已经无法确认。

**不可用时降级为两方**。任何一项有效性检查不通过、或节点查询失败，这个喂价就是 `unavailable`：预言机把这个币对的 Chainlink 价格清掉，交叉验证降级为“现价 vs TWAP”两方，响应里说明原因，绝不拿坏数据去比较。

| Chainlink 校验结果 | `GET /quote` | `POST /swap/build`、`POST /swap` |
|-------------------|-------------|--------------------------------|
| `ok` | 正常 | 正常 |
| `deviated`（现价偏离喂价 > 2%） | 返回报价并带警告 | **403 拒绝** |
| `unavailable`（过期、轮次未完成、旧轮次、非正数、节点查询失败） | 带警告，说明原因 | 放行并带警告，校验降级为两方 |
| `none`（该币对没有配置喂价） | 不参与校验 | 不参与校验 |

#### 现价 vs TWAP：操纵拒绝

已连接节点时，`chain.Client.TWAP` 调用池子的 `observe([window, 0])`（窗口默认 600 秒），与索引器里的现价比对：

```go
// oracle/oracle.go — CheckTWAP
check := TWAPCheck{Status: TWAPOK, Spot: spot, TWAP: twap, Deviation: math.Abs(spot-twap) / twap}
if check.Deviation > o.config.MaxTWAPDeviation {   // 默认 5%
    check.Status = TWAPDeviated
}
```

| TWAP 校验结果 | `GET /quote` | `POST /swap/build`、`POST /swap` |
|--------------|-------------|--------------------------------|
| `ok` | 正常 | 正常 |
| `deviated`（偏离 > 5%） | 返回报价并带警告 | **403 拒绝** |
| `unavailable`（观测不足或节点不可用） | 带警告 | 放行并带警告（无法判断，不能当作“正常”） |

**观测不足时的降级**：池子太新或观测容量小时，600 秒前的数据已被覆盖，链上 `observe` 会 revert `OLD`。这时 `TWAP` 读出最老一条观测的时间戳，退到它能覆盖的最长窗口并标记 `degraded`；连最短窗口（默认 60 秒）都覆盖不了才返回 `unavailable`。

#### 校验明细 `price_checks`

`/quote`、`/swap/build` 的响应里带 `price_checks`（路径上每个池子一条），`/twap?pool=` 返回单个池子的同一结构。`sources` 列出这次实际参与校验的价格源，三个都在是三方校验。下面是本地部署后、池子刚创建时的一次真实返回——TWAP 观测还不足，所以是“现价 + Chainlink”两方（省略了值为 0 的字段）：

```json
{
  "pool": "0x93bea4f2139418874a17b84a9b6928c8573e0330",
  "spot": 0.0005,
  "sources": ["spot", "chainlink"],
  "status": "unavailable",
  "detail": "预言机观测不足",
  "chainlink_status": "ok",
  "chainlink": 0.0005
}
```

（这个池子里 USDC 是 token0，所以现价是 1/2000；喂价 ETH/USD = 2000 换算后同样是 0.0005。）

#### 保护模式

| 事件 | 操作 |
|------|------|
| 池内现价与 Chainlink 偏离 > 2% | 激活保护模式（同时该笔交易被拒绝） |
| 池内 TWAP 与 Chainlink 偏离 > 2% | 激活保护模式 |
| 池内现价与 TWAP 偏离 > 5% | 激活保护模式（同时该笔交易被拒绝） |
| 全部恢复正常且冷却时间（5 分钟）已过 | 解除保护模式 |

保护模式下不一刀切拒绝交易（避免变成 DoS），而是要求滑点设置不低于 0.5%，不满足的请求被拒绝，满足的放行并附带风险提示。当前处于保护模式的交易对出现在 `GET /api/v1/status` 的 `protected_pairs` 里。

池内现价由两条路径喂给预言机：索引器每处理一笔 Swap 推送一次（`PoolPriceFeed`），以及每次校验时连同参考价一起写入。偏离和不可用的次数计入 `oracle_chainlink_deviation_total`、`oracle_chainlink_unavailable_total`、`oracle_twap_deviation_total`、`oracle_twap_unavailable_total`。

**验证**：`chainlink_test.go` 覆盖配置解析、五项有效性检查、两种 token 顺序和不同 decimals 的换算、缓存与刷新、三方降级；集成测试用一个可控的聚合器合约（`TestAggregator`）在真实节点上走完“喂价正常 → 三方校验放行；喂价比池价高 10% → 403；过期 / 轮次未完成 / 旧轮次 / 负价 → 降级放行并说明原因；喂价恢复 → 回到三方”。

**代码位置**：`backend/oracle/chainlink.go`, `backend/oracle/oracle.go`, `backend/chain/contracts.go`（`TWAP` `ChainlinkLatestRound`）, `backend/api/trade.go`（`checkPriceSafety` `checkPool`）

---

### 20. 待签名交易构造与交易状态查询

**问题**：后端算出了最优路径，但用户怎么把它变成一笔链上交易？后端**不托管私钥**，不能替用户签名。

**流程**：

```
1. POST /api/v1/swap/build
     后端：搜索路由 → 价格安全检查 → MEV 检查 → 链上报价抽检
           → 把路由编码成 Manager 的调用数据
     返回：{ to, data, value, chain_id, gas_limit, gas, min_amount_out, deadline, price_checks, ... }

2. 用户钱包对 { to, data, value, gas_limit } 签名，得到 raw_tx

3. POST /api/v1/swap   （携带 EIP-712 签名 + raw_tx）
     后端：EIP-712 验签 → 再做一遍价格安全检查和 MEV 检查
           → 确认 raw_tx 的签名者是 owner、目标是 Manager → 广播
     返回：{ tx_hash, status: "pending" }

4. GET /api/v1/tx/{hash}   轮询直到 confirmed
```

**路由到合约方法的映射**：

| 请求 | 路由 | 调用的合约方法 | 写进交易的保护 |
|------|------|---------------|---------------|
| 给 `amount_in` | 单跳 | `Manager.swap` | `amountOutMinimum` + `deadline` |
| 给 `amount_in` | 多跳 | `Manager.exactInput`（紧凑路径，见第 9 节） | `amountOutMinimum` + `deadline` |
| 给 `amount_out` | 单池 | `Manager.exactOutputSingle` | `amountInMaximum` + `deadline` |

`min_amount_out = 报价 × (1 - 滑点)` 向下取整，`max_amount_in = 报价 × (1 + 滑点)` 向上取整。编码直接用内嵌 ABI 的 `Pack`，结构体字段与合约的 `SwapParams / ExactInputParams / ExactOutputSingleParams` 一一对应：

```go
// chain/contracts.go
func EncodeExactInput(path []byte, recipient string, amountIn, amountOutMinimum *big.Int, deadline int64) ([]byte, error) {
    return contractabi.Manager.Pack("exactInput", exactInputParams{
        Path:             path,
        Recipient:        common.HexToAddress(recipient),
        AmountIn:         amountIn,
        AmountOutMinimum: amountOutMinimum,
        Deadline:         big.NewInt(deadline),
    })
}
```

**构造时的三道保险**：

1. **不走缓存**：每次都重新搜索路由
2. **链上报价抽检**：配置了 Quoter 时，用同一路径、同一输入问一次链上；不一致时响应里 `quote_consistent=false`，并且滑点保护按较低的一方计算
3. **gas 上限**：请求带了 `from` 就让节点 `eth_estimateGas` 模拟整笔交易，加 20% 余量（`gas_limit_source=node`）；否则用路由引擎的静态估算（`router`）

**广播前的检查**：`raw_tx` 必须由 EIP-712 签名的 `owner` 签出、发给 Manager 合约，后端只做转发：

```go
// api/server.go — handleSwap
info, err := chain.InspectRawTx(req.RawTx)   // 解析交易并恢复签名者
if info.From != strings.ToLower(req.Owner) {
    writeError(w, http.StatusForbidden, "raw_tx is not signed by owner")
    return
}
if s.config.ManagerAddress != "" && info.To != strings.ToLower(s.config.ManagerAddress) {
    writeError(w, http.StatusForbidden, "raw_tx is not addressed to the Manager contract")
    return
}
```

不带 `raw_tx` 时接口只返回检查结果（`status: "validated"`），由客户端自行提交。

**状态码**区分“请求的问题”和“服务的问题”：

| 情况 | 状态码 |
|------|-------|
| 找不到可成交的路径（币对不存在、流动性不足） | 404 |
| 参数不合法；MEV 交易前检查不通过 | 400 |
| EIP-712 验签失败 | 401 |
| 价格安全检查拒绝；`raw_tx` 签名者或目标不对 | 403 |
| 节点明确拒绝这笔交易（nonce 不对、余额不足等） | 400 |
| 连不上节点，广播失败 | 502 |
| 未连接节点或未配置 Manager | 503 |

**交易状态：延迟确认**

交易被打包不等于最终确认，它所在的区块仍可能被 Reorg 掉。`TxStatus` 按回执和确认数给出四种状态，确认数阈值与索引器的 `CONFIRMATION_BLOCKS`（默认 12）一致：

```
            节点交易池里有        已打包，确认数 < K       确认数 >= K
提交 ──→ ┌─────────┐  打包  ┌──────────────────────┐      ┌───────────┐
         │ pending │ ─────→ │ pending_confirmation │ ───→ │ confirmed │
         └─────────┘        └──────────┬───────────┘      └───────────┘
                                       │ 回执 status = 0
                                       ▼
                                  ┌──────────┐
                                  │ reverted │
                                  └──────────┘
```

节点完全不认识的哈希返回 404，格式不合法返回 400。

**代码位置**：`backend/api/trade.go`（`handleBuildSwap`）, `backend/api/server.go`（`handleSwap` `handleTx`）, `backend/chain/contracts.go`, `backend/chain/client.go`

---

### 21. EIP-712 签名与三层防重放

**为什么需要 EIP-712？**

普通的 `eth_sign` 签名只展示一串十六进制哈希，用户无法知道自己在签什么。EIP-712 定义了结构化数据签名标准，钱包可以展示可读的签名内容。

**三层防重放机制**：

```
┌──────────────────────────────────────────────────────────────┐
│ Layer 1: Deadline（时间锁）                                   │
│                                                              │
│   签名包含 deadline 时间戳（默认 5 分钟后过期）                  │
│   if time.Now().Unix() > permit.Deadline → 拒绝               │
│                                                              │
│   防御场景：用户离线签名被第三方截获后保存，数天后恶意提交         │
├──────────────────────────────────────────────────────────────┤
│ Layer 2: Nonce（序号锁）                                      │
│                                                              │
│   每个地址维护单调递增的 nonce（0, 1, 2, 3, ...）               │
│   签名使用后 nonce 自增，同一签名无法第二次使用                   │
│   if permit.Nonce != expectedNonce → 拒绝                     │
│                                                              │
│   防御场景：同一笔签名被多次提交（重放攻击）                      │
├──────────────────────────────────────────────────────────────┤
│ Layer 3: Domain Separator（域隔离）                            │
│                                                              │
│   签名哈希包含 ChainID 和 VerifyingContract 地址               │
│   hash = keccak256("\x19\x01" || domainHash || messageHash)  │
│                                                              │
│   防御场景：                                                   │
│   - ETH mainnet 的签名被提交到 Polygon（跨链重放）              │
│   - 用于合约 A 的签名被提交到合约 B（跨合约重放）                │
└──────────────────────────────────────────────────────────────┘
```

**摘要计算**

摘要按 EIP-712 规范逐字段编码，与钱包 `eth_signTypedData_v4` 算出的结果相同：

```go
// eip712/eip712.go
var permitTypeHash = crypto.Keccak256([]byte(
    "SwapPermit(address owner,address tokenIn,address tokenOut,uint256 amountIn,uint256 minAmountOut,uint256 nonce,uint256 deadline)"))

// digest = keccak256(0x19 0x01 ‖ domainSeparator ‖ hashStruct(permit))
// hashStruct(s) = keccak256(typeHash ‖ 各字段按 32 字节编码)
domainSeparator := crypto.Keccak256(
    domainTypeHash,
    crypto.Keccak256([]byte(v.domain.Name)),      // "UniGo-DEX"
    crypto.Keccak256([]byte(v.domain.Version)),   // "1"
    uintWord(new(big.Int).SetUint64(v.domain.ChainID)),
    contract,                                      // Manager 合约地址
)
```

域里的 `chainId` 在连上节点后取节点的链 ID，`verifyingContract` 是 `MANAGER_ADDRESS`。`eip712_test.go` 的 `TestHashMatchesStandardTypedData` 把摘要与 go-ethereum 的标准类型化数据实现对照，保证钱包签出来的签名能验过。

**验证流程**：

```go
func (v *Verifier) Verify(permit *SwapPermit, sig *Signature) error {
    // Layer 1: Deadline
    if time.Now().Unix() > permit.Deadline {
        return errors.New("签名已过期（deadline exceeded）")
    }

    // Layer 3: 恢复签名者（域分隔符不同的签名恢复不出同一个地址）
    hash, err := v.Hash(permit)
    // ...
    recovered, err := recoverAddress(hash, sig)   // secp256k1 ecrecover
    // ...
    if recovered != owner {
        return fmt.Errorf("签名验证失败：期望签名者 %s，恢复出 %s", owner, recovered)
    }

    // Layer 2: Nonce —— 通过后消费，同一个签名不能再用第二次
    v.mu.Lock()
    defer v.mu.Unlock()
    if expected := v.nonces[owner]; permit.Nonce != expected {
        return fmt.Errorf("nonce 不匹配：期望 %d，收到 %d（可能为重放攻击）", expected, permit.Nonce)
    }
    v.nonces[owner]++
    return nil
}
```

**拒绝可延展签名**：同一条消息的 `(r, s)` 和 `(r, n-s)` 都能通过 ecrecover。`recoverAddress` 只接受低 s 值（`crypto.ValidateSignatureValues(..., true)`），保证一个签名只有一种写法。

客户端签名前从 `GET /api/v1/nonce/{address}` 取当前 nonce。验签失败返回 401 并计入 `swap_signature_rejected_total`。

**代码位置**：`backend/eip712/eip712.go`, `backend/eip712/eip712_test.go`

---

### 22. EIP-1559 Gas 成本评估

**问题**：报价只告诉用户能换到多少，不告诉这笔交易要花多少 gas 费。小额交易的 gas 可能比价差还大；套利机会也要扣掉 gas 才知道赚不赚。

EIP-1559 之后每单位 gas 的实际价格是：

```
gasPrice     = baseFee + priorityFee
maxFeePerGas = 2 × baseFee + priorityFee
```

- `baseFee`：协议按上一个区块的拥堵程度算出，写在区块头里，会被销毁
- `priorityFee`：给出块者的小费，取节点建议值（`eth_maxPriorityFeePerGas`）
- `maxFeePerGas`：`baseFee` 每个区块最多涨 12.5%，取 2 倍可以保证连续 6 个满块之内交易仍能被打包

```go
// gas/gas.go
func (e *Estimator) Estimate(ctx context.Context, gasLimit uint64) Estimate {
    baseFee, priorityFee, source := e.fees(ctx)

    gasPrice := new(big.Int).Add(baseFee, priorityFee)
    maxFee := new(big.Int).Add(new(big.Int).Lsh(baseFee, 1), priorityFee)
    limit := new(big.Int).SetUint64(gasLimit)

    return Estimate{
        GasLimit: gasLimit, BaseFee: baseFee.String(), PriorityFee: priorityFee.String(),
        GasPrice: gasPrice.String(), MaxFeePerGas: maxFee.String(),
        Cost:    new(big.Int).Mul(limit, gasPrice).String(),   // 预期成本
        MaxCost: new(big.Int).Mul(limit, maxFee).String(),     // 最坏成本
        Source:  source,                                        // chain / static
    }
}
```

**费用来源与降级**：

| 情况 | 行为 | `source` |
|------|------|----------|
| 已连接节点 | 取最新区块头的 `baseFee` + 节点建议小费，缓存 5 秒 | `chain` |
| 未配置节点或查询失败 | 退回静态值（20 gwei + 1.5 gwei），报价接口不会因此失败 | `static` |

`gasLimit` 来自路由引擎的估算或节点的 `eth_estimateGas`（见第 20 节）。评估结果出现在报价和待签名交易响应的 `gas` 字段里，套利扫描用 `CostWei` 把 gas 成本折算进净利润（第 25 节）。

**代码位置**：`backend/gas/gas.go`

