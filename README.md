# UniGo-DEX

基于 Uniswap V3 集中流动性设计的去中心化交易所全栈实现（Solidity + Go）。

本项目不是一个"调 SDK 跑 Demo"的教程，而是从底层数学库到生产级系统设计的完整实现。涵盖链上合约（10 个数学库 + 4 个核心合约）、链下 Go 后端（12 个模块），以及真实系统中的工程挑战与解决方案。

---

## 目录

- [项目简介](#项目简介)
- [系统架构](#系统架构)
- [技术亮点总览](#技术亮点总览)
- **智能合约层**
  - [1. 集中流动性数学模型](#1-集中流动性数学模型)
  - [2. Q64.96 定点数与 512 位算术](#2-q6496-定点数与-512-位算术)
  - [3. Tick 位图与 O(1) 查找](#3-tick-位图与-o1-查找)
  - [4. 回调模式与安全性](#4-回调模式与安全性)
  - [5. CREATE2 确定性部署](#5-create2-确定性部署)
  - [6. Quoter 的 revert-catch 模拟执行](#6-quoter-的-revert-catch-模拟执行)
- **Go 后端 — 核心业务**
  - [7. 链下数学库（链上链下一致性）](#7-链下数学库链上链下一致性)
  - [8. Smart Order Router（二级降级）](#8-smart-order-router二级降级)
  - [9. 事件索引器与 Reorg 回滚](#9-事件索引器与-reorg-回滚)
  - [10. 缓存 Singleflight 防击穿](#10-缓存-singleflight-防击穿)
  - [11. Chainlink 预言机验证与保护模式](#11-chainlink-预言机验证与保护模式)
  - [12. EIP-712 签名与三层防重放](#12-eip-712-签名与三层防重放)
- **Go 后端 — 工程基础设施**
  - [13. 断路器（Circuit Breaker）](#13-断路器circuit-breaker)
  - [14. Goroutine 池与性能优化](#14-goroutine-池与性能优化)
  - [15. 结构化日志与链路追踪](#15-结构化日志与链路追踪)
  - [16. Prometheus 风格可观测性指标](#16-prometheus-风格可观测性指标)
  - [17. WebSocket/SSE 实时推送](#17-websocketsse-实时推送)
  - [18. API 限流与背压控制](#18-api-限流与背压控制)
  - [19. 优雅关闭与生命周期管理](#19-优雅关闭与生命周期管理)
- [Benchmark 性能对比](#benchmark-性能对比)
- [快速开始](#快速开始)
- [项目结构](#项目结构)
- [参考资料](#参考资料)

---

## 项目简介

UniGo-DEX 是一个教学级别的 DEX 全栈实现，参考 Uniswap V3 的核心机制。

### 为什么做这个项目？

1. **深度理解 AMM 数学**：不是调 SDK，而是从 `mulDiv` 全精度乘除到 `TickMath` 查表法完整实现
2. **链上链下协同**：Go 后端精确复现 Solidity 数学库，使用 `math/big` 实现零 Gas 链下报价，tick 值与链上 bit-exact 一致
3. **生产级系统设计**：考虑了 Reorg 回滚、缓存雪崩、路由降级、断路器、背压控制等真实生产环境中的问题
4. **Go 工程实践**：Goroutine 池化与 Benchmark 对比、slog 结构化日志、Prometheus 指标、SSE 实时推送、Token Bucket 限流

### 技术栈

| 层级 | 技术 |
|------|------|
| 智能合约 | Solidity 0.8.20, Hardhat, OpenZeppelin |
| 后端语言 | Go 1.21+, 纯标准库（零外部框架依赖） |
| 数学精度 | Q64.96 定点数, 512 位中间精度, math/big |
| 可观测性 | slog 结构化日志, Prometheus 文本格式指标 |
| 实时通信 | Server-Sent Events (SSE) |
| 密码学 | keccak256, EIP-712, ECDSA |

---

## 系统架构

```
                          ┌─────────────────┐
                          │   用户 / 前端     │
                          └────────┬────────┘
                                   │ HTTP / SSE
            ┌──────────────────────▼──────────────────────┐
            │                 API Server                   │
            │  ┌──────────┐ ┌──────────┐ ┌──────────────┐ │
            │  │ 限流中间件 │ │ 背压控制  │ │ CORS / Log  │ │
            │  │TokenBucket│ │Semaphore │ │ TraceID注入  │ │
            │  └──────────┘ └──────────┘ └──────────────┘ │
            └─────┬──────────────┬──────────────┬─────────┘
                  │              │              │
         ┌────────▼───┐  ┌──────▼──────┐  ┌───▼────────┐
         │  Cache      │  │  Router     │  │  Oracle    │
         │ Singleflight│  │ BFS + 降级   │  │ Chainlink  │
         │ + Warmup    │  │ Worker Pool │  │ 交叉验证    │
         └──────┬──────┘  └──────┬──────┘  └────────────┘
                │                │
     ┌──────────▼─────────┐     │     ┌─────────────────┐
     │   Circuit Breaker  │     │     │   Worker Pool    │
     │   RPC 熔断保护      │     │     │  Goroutine 池化  │
     └──────────┬─────────┘     │     └─────────────────┘
                │               │
     ┌──────────▼───────────────▼──────────────────────┐
     │              PoolStateManager                    │
     │  （链下状态镜像 — 事件驱动状态重建）                │
     └──────────────────────┬──────────────────────────┘
                            │ 事件回放
     ┌──────────────────────▼──────────────────────────┐
     │              Event Indexer                       │
     │   区块轮询 → 事件解析 → Reorg 检测 + 状态回滚      │
     └──────────────────────┬──────────────────────────┘
                            │ JSON-RPC / WebSocket
     ┌──────────────────────▼──────────────────────────┐
     │                Ethereum Node                     │
     │  ┌────────────┐ ┌──────────────┐ ┌────────────┐ │
     │  │ UniGoPool   │ │ UniGoFactory │ │ UniGoQuoter│ │
     │  └────────────┘ └──────────────┘ └────────────┘ │
     └─────────────────────────────────────────────────┘

     横切关注点：
     ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐
     │ Metrics  │ │ Logging  │ │ WS/SSE   │ │ EIP-712  │
     │ P50/P99  │ │ TraceID  │ │ 价格推送  │ │ 签名验证  │
     └──────────┘ └──────────┘ └──────────┘ └──────────┘
```

---

## 技术亮点总览

| 分类 | 技术点 | 核心机制 | 代码位置 |
|------|--------|----------|----------|
| 合约 | Q64.96 定点数 + 512 位精度 | mulmod + 牛顿法模逆 | `contracts/lib/Math.sol` |
| 合约 | Tick 位图 O(1) 查找 | 256-bit word + MSB/LSB | `contracts/lib/TickBitmap.sol` |
| 合约 | 查表法 tick↔price 转换 | 20 个预计算魔数常量 | `contracts/lib/TickMath.sol` |
| 合约 | 回调模式安全转账 | mint/swap callback + 余额校验 | `contracts/UniGoPool.sol` |
| 合约 | CREATE2 确定性部署 | salt = keccak256(token0, token1, fee) | `contracts/UniGoFactory.sol` |
| 合约 | revert-catch 模拟执行 | 故意 revert 携带结果数据 | `contracts/UniGoQuoter.sol` |
| 后端 | 链下数学精确复现 | math/big + 相同魔数常量 | `backend/math/` |
| 后端 | BFS 路由 + 200ms 超时降级 | context.WithTimeout + fallback | `backend/router/router.go` |
| 后端 | Reorg 检测 + 状态回滚 | parentHash 校验 + RollbackToBlock | `backend/indexer/indexer.go` |
| 后端 | Singleflight 防缓存击穿 | sync.WaitGroup 请求合并 | `backend/cache/cache.go` |
| 后端 | Chainlink 验证 + 保护模式 | 过期检测 + 交叉偏离检测 | `backend/oracle/oracle.go` |
| 后端 | EIP-712 三层防重放 | Deadline + Nonce + DomainSeparator | `backend/eip712/eip712.go` |
| 后端 | 断路器（三态状态机） | Closed→Open→HalfOpen + 探测恢复 | `backend/circuitbreaker/` |
| 后端 | Goroutine 池化 | 固定 Worker + 有界 Channel 复用 | `backend/workerpool/pool.go` |
| 后端 | 结构化日志 + 链路追踪 | slog + context TraceID 传播 | `backend/logging/logging.go` |
| 后端 | Prometheus 风格指标 | Counter/Gauge/Histogram + P99 | `backend/metrics/metrics.go` |
| 后端 | SSE 实时价格推送 | Hub 发布-订阅 + 慢客户端驱逐 | `backend/ws/ws.go` |
| 后端 | Token Bucket + 并发信号量 | 每 IP 限流 + 全局 503 背压 | `backend/api/middleware.go` |

---

## 智能合约层

### 1. 集中流动性数学模型


**核心公式**（来自 Uniswap V3 白皮书）：

```
恒等式：x · y = L²

其中：
  x = L / √P        （token0 虚拟储备）
  y = L · √P        （token1 虚拟储备）
  L = 流动性（liquidity）
  P = 当前价格 = token1 / token0

集中流动性 — 在 [Pa, Pb] 区间内提供流动性：
  Δtoken0 = L × (1/√Pa - 1/√Pb)
  Δtoken1 = L × (√Pb - √Pa)
```

**与 Uniswap V2 的关键区别**：

| 维度 | Uniswap V2 | Uniswap V3 (UniGo) |
|------|-----------|---------------------|
| 流动性分布 | 均匀分布在 `[0, ∞)` | 集中在 `[Pa, Pb]` 区间 |
| 资金效率 | ~0.5%（大部分区间无交易） | 可达 V2 的 4000x |
| 价格表示 | reserve0/reserve1 | sqrtPriceX96 (Q64.96) |
| 状态跟踪 | 单一 (x, y) | tick + 位图 + position 映射 |

**Swap 的逐 tick 循环过程**：

```
价格从 P_current 移动到 P_target：
  1. 查找当前 tick 到 P_target 之间的下一个已初始化 tick
  2. 计算在 [P_current, P_next_tick] 这一步能消耗多少输入
  3. 如果输入全部消耗完 → 结束
  4. 如果到达 tick 边界 → cross tick，更新 liquidity → 回到步骤 1
  5. 每穿越一个已初始化的 tick 要多做一次 Tick.cross（翻转手续费外侧累计值、调整活跃流动性），
     实测多消耗约 3.9 万 gas（见 Benchmark 一节）
```

这个循环是整个 AMM 最核心的逻辑，在 `UniGoPool.sol:swap()` 中实现，链下在 `backend/router/quote.go:SimulateSwap()` 中完全复刻。

**代码位置**：`contracts/contracts/lib/SqrtPriceMath.sol`, `contracts/contracts/UniGoPool.sol`

---

### 2. Q64.96 定点数与 512 位算术

Solidity 没有浮点数。AMM 的价格计算需要极高精度（价格变动 0.01% = 1 个 tick），因此使用 **Q64.96 定点数**格式：

```
sqrtPriceX96 = √price × 2^96

                 64 位                          96 位
├─────── 整数部分 ────────┤├────────── 小数部分 ──────────────┤
总共 160 位（uint160），但计算中需要 uint256
```

**核心挑战**：计算 `(a × b) / c` 时，中间结果 `a × b` 可能溢出 256 位。

例如：`amount × sqrtPriceX96` 其中 amount 是 uint256，sqrtPriceX96 是 uint160，乘积可能达到 416 位。

**解决方案：`mulDiv` 使用 512 位中间精度**

```solidity
// Math.sol — 512 位无损乘法
function mulDiv(uint256 a, uint256 b, uint256 denominator) internal pure returns (uint256) {
    // 第 1 步：512 位乘法（使用 assembly 的 mulmod）
    uint256 prod0; // 乘积的低 256 位
    uint256 prod1; // 乘积的高 256 位
    assembly {
        let mm := mulmod(a, b, not(0))  // a*b mod 2^256
        prod0 := mul(a, b)              // 低 256 位
        prod1 := sub(sub(mm, prod0), lt(mm, prod0))  // 高 256 位
    }

    // 第 2 步：如果 prod1 == 0，直接 256 位除法
    if (prod1 == 0) return prod0 / denominator;

    // 第 3 步：512 / 256 除法 — 使用模逆 + 牛顿迭代
    // 3a. 从 denominator 中提取 2 的幂次因子
    uint256 twos = denominator & (~denominator + 1); // 最低设置位
    // 3b. 除以 twos（右移），移除公共因子
    // 3c. 计算 denominator 对 2^256 的模逆
    uint256 inv = (3 * denominator) ^ 2; // 初始近似
    // 3d. 牛顿法迭代 6 次（收敛精度：2 → 4 → 8 → 16 → 32 → 64 → 128 → 256 位）
    unchecked {
        inv *= 2 - denominator * inv;  // 每次迭代精度翻倍
        inv *= 2 - denominator * inv;
        inv *= 2 - denominator * inv;
        inv *= 2 - denominator * inv;
        inv *= 2 - denominator * inv;
        inv *= 2 - denominator * inv;
    }
    // 3e. 结果 = 调整后的被除数 × 模逆
    return prod0 * inv;
}
```

**关键设计决策**：`unchecked` 块包裹模运算（取反、牛顿法迭代中的 `2 - denominator * inv`）。这些溢出是有意为之的模运算，Solidity 0.8+ 默认的溢出检查会让它们 panic。这是数学上正确的行为，不是 bug。

**代码位置**：`contracts/contracts/lib/Math.sol`

---

### 3. Tick 位图与 O(1) 查找

价格被离散化为 tick：`price = 1.0001^tick`（每个 tick = 0.01% 价格变化）。

tick 空间范围 = [-887272, 887272]，包含约 177 万个可能的 tick。但只有 LP 提供了流动性的 tick 才是"已初始化"的。Swap 时需要快速找到**下一个有流动性的 tick**。

**TickBitmap 方案**：

```
tick 空间被分割为 256-bit 的 word：
  wordPos = tick / 256   （哪个 word）
  bitPos  = tick % 256   （word 内哪一位）

每个 word 是一个 uint256，每一位表示一个 tick 是否已初始化。

查找下一个已初始化 tick 的算法：
  1. 根据当前 tick 计算 wordPos 和 bitPos
  2. 创建掩码：mask = (1 << bitPos) - 1  // 当前位右侧所有位
  3. masked = word & mask
  4. 如果 masked != 0 → 用 MSB (Most Significant Bit) 找到最近的 tick
  5. 如果 masked == 0 → 当前 word 内没有，移到下一个 word

                  wordPos=0                     wordPos=1
    ┌───────────────────────────┐ ┌───────────────────────────┐
    │ 0 0 1 0 0 0 0 1 0 0 1 ...│ │ 0 0 0 0 1 0 0 0 0 0 0 ...│
    └───────┬───────────────────┘ └───────────────────────────┘
            │
            tick=5 已初始化（该位 = 1）
```

**Gas 消耗对比**：

| 方法 | 最坏情况 Gas | 说明 |
|------|-------------|------|
| 暴力遍历 | O(N) × SLOAD | N = tick 间距，可能上万 |
| 位图 + MSB | O(1) × SLOAD | 最多查 1-2 个 word |

**MSB/LSB 位运算**（`BitMath.sol`）：

```solidity
// 查找最高有效位（Most Significant Bit）
// 使用二分法：先检查高 128 位，再高 64 位...逐步缩小范围
function mostSignificantBit(uint256 x) internal pure returns (uint8 r) {
    if (x >= 0x100000000000000000000000000000000) { x >>= 128; r += 128; }
    if (x >= 0x10000000000000000)                 { x >>= 64;  r += 64;  }
    if (x >= 0x100000000)                         { x >>= 32;  r += 32;  }
    if (x >= 0x10000)                             { x >>= 16;  r += 16;  }
    if (x >= 0x100)                               { x >>= 8;   r += 8;   }
    if (x >= 0x10)                                { x >>= 4;   r += 4;   }
    if (x >= 0x4)                                 { x >>= 2;   r += 2;   }
    if (x >= 0x2)                                 { r += 1;              }
}
```

**代码位置**：`contracts/contracts/lib/TickBitmap.sol`, `contracts/contracts/lib/BitMath.sol`

---

### 4. 回调模式与安全性

Pool 不使用传统的 `approve + transferFrom` 两步模式，而是采用**回调模式**。这是 Uniswap V3 的核心设计模式之一。

**完整调用流程**：

```
用户调用 Manager.mint(pool, tickLower, tickUpper, amount)
  │
  ├──→ Manager 调用 Pool.mint(tickLower, tickUpper, amount, callbackData)
  │      │
  │      ├──→ Pool 计算需要的 token 数量 (amount0Owed, amount1Owed)
  │      ├──→ Pool 记录余额快照: balance0Before = token0.balanceOf(pool)
  │      ├──→ Pool 回调 Manager: IUniGoPoolCallbacks.uniGoMintCallback(amount0, amount1, data)
  │      │      │
  │      │      └──→ Manager 执行 token0.transferFrom(user, pool, amount0)
  │      │           Manager 执行 token1.transferFrom(user, pool, amount1)
  │      │
  │      └──→ Pool 验证: token0.balanceOf(pool) >= balance0Before + amount0Owed
  │           如果验证失败 → revert (保证原子性)
```

**与“Pool 内部直接 transferFrom”的对比**：

| 维度 | Pool 内部 transferFrom | 回调模式 |
|------|----------------------|---------|
| 授权对象 | 用户必须直接授权每一个 Pool | Pool 只看余额增量，用户只需授权 Manager 一次 |
| 资金来源 | 只能来自 Pool 的调用者 | 回调里可从任意来源筹集（用户、合约自有余额、上一跳的产出） |
| 多跳兑换 | 每一跳都要从用户处划转 | 中间跳由 Manager 用上一跳收到的 token 支付（第 9 节） |
| 闪电贷支持 | 不支持 | 天然支持：先转出、回调里归还（第 8 节） |

**安全保证**：Pool 在回调前记录余额快照，回调后验证余额增量 >= 预期。即使回调中执行了任意代码，只要最终余额不足，整个交易 revert。

除余额校验外还有两层保护：Pool 所有会改变状态的入口（`mint / burn / collect / swap / flash`）都带 `lock` 重入锁，回调里再进入同一个 Pool 会以 `LOK` revert；Pool 和 Manager 的 token 转账一律走 SafeERC20（第 12 节）。

**代码位置**：`contracts/contracts/UniGoPool.sol` (mint/swap), `contracts/contracts/interfaces/IUniGoPoolCallbacks.sol`

---

### 5. CREATE2 确定性部署

Factory 使用 `CREATE2` 而非 `CREATE` 部署 Pool 合约：

```solidity
// UniGoFactory.sol
pool = address(
    new UniGoPool{salt: keccak256(abi.encode(token0_, token1_, fee_))}(
        address(this), token0_, token1_, fee_, tickSpacing_
    )
);
```

**地址计算公式**：

```
poolAddress = keccak256(
    0xff,                                    // 固定前缀
    factoryAddress,                          // 工厂合约地址
    keccak256(abi.encode(token0, token1, fee)), // salt
    keccak256(creationCode ‖ abi.encode(构造参数)) // init code 哈希（含构造参数）
)[12:]  // 取后 20 字节
```

**意义**：

1. **链下可计算**：前端/路由引擎无需查链，直接算出 Pool 地址
2. **唯一性保证**：同一 (token0, token1, fee) 组合只能创建一个 Pool
3. **跨链一致性**：相同参数在不同链上生成相同地址（如果 Factory 地址一致）

**费率层级设计**：

| 费率 | tickSpacing | 适用场景 |
|------|------------|---------|
| 500 (0.05%) | 10 | 稳定币对（USDC/USDT） |
| 3000 (0.3%) | 60 | 主流代币对（ETH/USDC） |
| 10000 (1%) | 200 | 长尾/高波动代币 |

**代码位置**：`contracts/contracts/UniGoFactory.sol`

---

### 6. Quoter 的 revert-catch 模拟执行

**问题**：用户提交 Swap 前需要知道预期输出（报价），但 Quoter 不能真的执行 Swap（否则会改变池子状态）。

**巧妙的解决方案**：在 Swap 回调中故意 revert，让 EVM 回滚所有状态变更，但从 revert 的数据中提取出计算结果。

```solidity
// UniGoQuoter.sol
/// 自定义 revert 错误，用于携带报价结果
error QuoteResult(int256 amount0Delta, int256 amount1Delta);

function quote(QuoteParams memory params)
    public
    returns (int256 amount0Delta, int256 amount1Delta)
{
    try IUniGoPool(params.pool).swap(
        address(this),          // recipient = Quoter 自己（不会实际接收）
        params.zeroForOne,
        params.amountSpecified,
        params.sqrtPriceLimitX96,
        bytes("")
    ) {} catch (bytes memory reason) {
        // Pool.swap() 会调用 uniGoSwapCallback，而回调一定 revert，所以一定进入 catch
        return _parseQuoteResult(reason);
    }
}

// Quoter 的 swap 回调 — 不转入 token，而是 revert 携带计算结果
function uniGoSwapCallback(int256 amount0Delta, int256 amount1Delta, bytes calldata)
    external pure override
{
    // Pool 完整执行了 swap 计算（跨 tick 循环、流动性更新...）
    // 在这里 revert，所有状态变更被回滚，计算结果编码在 revert 数据中带出来
    revert QuoteResult(amount0Delta, amount1Delta);
}
```

`_parseQuoteResult` 先比对 revert 数据的 4 字节选择器：是 `QuoteResult` 就解出两个数量，不是（比如 Pool 自己因为 `SPL` revert 了）就把原始 revert 数据原样转发，调用方看到的是真实的错误原因。

**为什么不直接用 `staticcall`？**

`staticcall` 禁止任何状态修改，但 `swap()` 内部需要临时修改状态（更新 tick、价格、流动性）来完成计算。`revert-catch` 模式允许完整执行但最终回滚，报价复用的就是 Pool 自己的 swap 逻辑，结果与实际成交一致。

**多跳报价**

`quoteExactInput(path, amountIn)` 使用与 `Manager.exactInput` 相同的紧凑路径编码（第 9 节），逐跳报价，上一跳的输出作为下一跳的输入；流动性不足、输入吃不完时与 Manager 一样以 `PF` revert。链下通过 `eth_call` 调用它，后端用它对链下报价做一致性抽检（第 17 节）。

**代码位置**：`contracts/contracts/UniGoQuoter.sol`

---

### 7. 手续费累计与按 position 领取

**问题**：一个池子里有成千上万个 position，区间各不相同。每笔 swap 的手续费只该分给“区间包含当前价格”的 position，而且要按流动性比例分。如果每笔 swap 都遍历所有 position 记账，gas 无法接受。

**解决方案：只维护累计值，领取时做差**。和 TWAP（第 10 节）是同一个思路。

**第 1 层：全局累计值**

每一步 swap 的手续费按当时的活跃流动性均摊，累加进“每单位流动性累计手续费”（Q128.128 定点数）：

```solidity
// UniGoPool.sol — swap 主循环
if (state.liquidity > 0) {
    unchecked {
        state.feeGrowthGlobalX128 += Math.mulDiv(step.feeAmount, Position.Q128, state.liquidity);
    }
}
```

手续费只收输入 token，所以 `zeroForOne` 方向累加 `feeGrowthGlobal0X128`，反向累加 `feeGrowthGlobal1X128`。

**第 2 层：tick 记录“外侧”的累计值**

每个已初始化的 tick 存一个 `feeGrowthOutside`：相对当前价格，tick 另一侧累计了多少手续费。价格每穿过这个 tick 一次，“外侧”和“内侧”互换，所以做一次翻转：

```solidity
// Tick.sol — cross
info.feeGrowthOutside0X128 = feeGrowthGlobal0X128 - info.feeGrowthOutside0X128;
info.feeGrowthOutside1X128 = feeGrowthGlobal1X128 - info.feeGrowthOutside1X128;
```

有了上下界两个 tick 的外侧值，任意区间内累计的手续费可以 O(1) 算出：

```
                    tickLower              tickUpper
  ──────────────────────┼──────────────────────┼──────────────────────→ 价格
      feeGrowthBelow    │   feeGrowthInside    │    feeGrowthAbove

  feeGrowthInside = feeGrowthGlobal - feeGrowthBelow - feeGrowthAbove
```

`feeGrowthBelow / Above` 取外侧值还是 `global - 外侧值`，取决于当前价格在这个 tick 的哪一侧（`Tick.getFeeGrowthInside`）。

**第 3 层：position 结算**

position 记住上次结算时的 `feeGrowthInside`，下次结算时做差再乘以自己的流动性：

```solidity
// Position.sol — update
tokensOwed0 = uint128(Math.mulDiv(
    feeGrowthInside0X128 - self.feeGrowthInside0LastX128, liquidityBefore, Q128
));
// ...
self.feeGrowthInside0LastX128 = feeGrowthInside0X128;
self.tokensOwed0 += tokensOwed0;
```

这些减法都放在 `unchecked` 块里：累计值可以溢出回绕，只要做差时也按模 2^256 运算，差值仍然正确。

**领取流程**：

| 调用 | 作用 |
|------|------|
| `mint` / `burn` | 流动性变化前先按旧流动性结算手续费到 `tokensOwed` |
| `burn(tickLower, tickUpper, 0)` | 不移除流动性，只结算手续费 |
| `burn(…, amount)` | 移除的本金也记入 `tokensOwed`，不立即转账 |
| `collect(recipient, …, amount0Requested, amount1Requested)` | 从 `tokensOwed` 提取，实际提取量不超过可领取数量，只能提取调用者自己的 position |

**测试覆盖**（`UniGoPool.test.js` 的“跨 tick 的手续费分配”）：同在区间内时按流动性比例均分；价格越过窄区间边界后窄区间不再分到手续费；反向穿回后重新分到；价格从未进入的区间分不到任何手续费；跨 tick 成交后各 position 仍能全额撤出。

**代码位置**：`contracts/contracts/UniGoPool.sol`（`_updatePosition` `burn` `collect`）, `contracts/contracts/lib/Tick.sol`, `contracts/contracts/lib/Position.sol`

---

### 8. 闪电贷（Flash）

**问题**：池子里躺着大量闲置 token。能不能无抵押借给任何人用，又保证一定还得回来？

**答案**：EVM 交易的原子性。借出和归还发生在同一笔交易里，还不上就整笔回滚，等于没借过。

```solidity
// UniGoPool.sol
function flash(address recipient, uint256 amount0, uint256 amount1, bytes calldata data)
    external override lock
{
    uint128 _liquidity = liquidity;
    // 没有活跃流动性时手续费无人可分，会永久锁在池子里，直接拒绝
    require(_liquidity > 0, "L");

    // 1. 按池子费率计算手续费（向上取整）
    uint256 fee0 = Math.mulDivRoundingUp(amount0, fee, 1e6);
    uint256 fee1 = Math.mulDivRoundingUp(amount1, fee, 1e6);
    uint256 balance0Before = balance0();
    uint256 balance1Before = balance1();

    // 2. 先把 token 转给借款人
    if (amount0 > 0) IERC20(token0).safeTransfer(recipient, amount0);
    if (amount1 > 0) IERC20(token1).safeTransfer(recipient, amount1);

    // 3. 回调：借款人在这里用钱，并归还本金 + 手续费
    IUniGoPoolCallbacks(msg.sender).uniGoFlashCallback(fee0, fee1, data);

    // 4. 验证余额不低于“借出前余额 + 手续费”，否则整笔交易回滚
    uint256 balance0After = balance0();
    uint256 balance1After = balance1();
    require(balance0Before + fee0 <= balance0After, "F0");
    require(balance1Before + fee1 <= balance1After, "F1");

    // 5. 实收手续费（调用者可以多付）按活跃流动性均摊，归 LP 所有
    uint256 paid0 = balance0After - balance0Before;
    uint256 paid1 = balance1After - balance1Before;
    unchecked {
        if (paid0 > 0) feeGrowthGlobal0X128 += Math.mulDiv(paid0, Position.Q128, _liquidity);
        if (paid1 > 0) feeGrowthGlobal1X128 += Math.mulDiv(paid1, Position.Q128, _liquidity);
    }

    emit Flash(msg.sender, recipient, amount0, amount1, paid0, paid1);
}
```

**设计要点**：

| 要点 | 说明 |
|------|------|
| 费率 | 与池子的 swap 费率相同（如 0.3% 池借 1000 付 3），向上取整，池子不吃亏 |
| 手续费归属 | 走第 7 节同一套 `feeGrowthGlobal` 累计，只分给区间包含当前价格的 position |
| 重入锁 | `flash` 期间 Pool 处于锁定状态，回调里不能再对同一个 Pool `swap / mint / flash`（这决定了第 11 节套利合约的结构） |
| 零流动性保护 | 活跃流动性为 0 时拒绝放贷，避免手续费无主锁死 |
| 只验余额 | Pool 不关心借款人拿钱做了什么，只在回调后检查余额 |

**测试覆盖**：正常借还并触发 `Flash` 事件、手续费向上取整、不还款 revert、回调里重入被锁拦截、手续费计入 LP 可领取费用并能提取、只分给区间包含当前价格的 position、活跃流动性为 0 时被拒绝。

**代码位置**：`contracts/contracts/UniGoPool.sol`（`flash`）, `contracts/contracts/interfaces/IUniGoPoolCallbacks.sol`, `contracts/contracts/test/TestFlashBorrower.sol`

---

### 9. Manager：期限与滑点保护、多跳路径、精确输出

Pool 只提供最底层的 `mint / swap`，不带任何用户保护。用户通过 `UniGoManager` 交互，它解决四个问题。

#### 期限保护（deadline）

**问题**：交易在内存池里可能滞留很久。几小时后才被打包时，价格早已不是用户下单时的价格。

```solidity
// UniGoManager.sol — 所有入口都带这个修饰符
modifier checkDeadline(uint256 deadline) {
    require(block.timestamp <= deadline, "EXP");
    _;
}
```

#### 滑点保护

**问题**：从报价到成交之间价格会变，也可能被人故意抢跑（三明治攻击，见第 24 节）。用户需要一个“最差也要拿到这么多”的底线。

| 入口 | 保护参数 | 不满足时 |
|------|---------|---------|
| `mint` | `amount0Min` / `amount1Min`：实际存入的两种 token 都不低于下限 | `S0` / `S1` |
| `swap` / `exactInput` | `amountOutMinimum`：最少到手的输出 | `SLP` |
| `exactOutputSingle` | `amountInMaximum`：最多付出的输入 | `TMI` |

`Manager.swap` 只接受精确输入（`amountSpecified > 0`）：精确输出在这个入口没有输入上限保护，必须走带 `amountInMaximum` 的 `exactOutputSingle`。

#### 多跳 `exactInput` 与紧凑路径编码

**问题**：A/C 没有直连池（或直连池很浅）时，要走 A → B → C。让用户发两笔交易既贵又不原子。

路径是一个紧凑编码的字节串，不用 `address[]` + `uint24[]` 两个动态数组：

```
 token(20 字节)  fee(3 字节)  token(20 字节)  fee(3 字节)  token(20 字节)
├──────────────┼───────────┼──────────────┼───────────┼──────────────┤
       A           3000           B           500            C

n 跳的路径长度 = 20 + 23 × n 字节；每个 (token, fee, token) 三元组确定一个 Pool
```

合约直接在 calldata 上切片解码，每跳完成后把路径头部削掉 23 字节：

```solidity
// UniGoManager.sol — exactInput
address payer = msg.sender;
amountOut = params.amountIn;                       // 每一跳的输出作为下一跳的输入

while (true) {
    bool isLastHop = path.length == POOL_PATH_SIZE;   // 43 字节 = 只剩一个 Pool

    amountOut = _exactInputHop(
        address(bytes20(path[:ADDR_SIZE])),               // tokenIn
        address(bytes20(path[HOP_SIZE:POOL_PATH_SIZE])),  // tokenOut
        uint24(bytes3(path[ADDR_SIZE:HOP_SIZE])),         // fee
        isLastHop ? params.recipient : address(this),     // 中间跳的输出先给 Manager
        amountOut,
        payer
    );

    if (isLastHop) break;

    payer = address(this);        // 后续各跳由 Manager 用上一跳收到的 token 付款
    path = path[HOP_SIZE:];
}

require(amountOut >= params.amountOutMinimum, "SLP");   // 滑点只检查最终输出
```

**为什么中间 token 要先经过 Manager**：Pool 在回调前后比较余额，提前转进去的 token 不算数，所以不能让上一个 Pool 直接把输出转给下一个 Pool。第一跳由用户付款，中间跳由 Manager 付款，最后一跳的输出直接给 `recipient`，全程用户只付 A、只收 C。

每一跳都要求输入全部成交（否则 `PF`）：流动性不足时剩余的 token 会滞留在 Manager 里，所以整笔作废。

#### 精确输出 `exactOutputSingle`

“我要正好拿到 100 个 C，最多愿意付 X 个 A”。对 Pool 传负的 `amountSpecified`，成交后做两个检查：

```solidity
// 流动性不足或撞到价格限制时拿不满，整笔作废
require(uint256(-(zeroForOne ? amount1 : amount0)) == params.amountOut, "PF");

amountIn = uint256(zeroForOne ? amount0 : amount1);
require(amountIn <= params.amountInMaximum, "TMI");
```

#### Pool 真伪校验

Manager 手里握着用户的授权。如果回调不校验来源，任何人都可以部署一个假 Pool，回调 Manager 把用户授权的 token 划走。所有入口和回调都通过 Factory 确认 Pool 是真实部署的：

```solidity
// 入口：用户传入的 Pool 地址必须能从 Factory 反查回来
require(IUniGoFactory(factory).getPool(data.token0, data.token1, data.fee) == pool, "POOL");

// 回调：调用者必须是回调数据里 (token0, token1, fee) 对应的那个 Pool
require(
    msg.sender == IUniGoFactory(factory).getPool(decoded.token0, decoded.token1, decoded.fee),
    "CB"
);
```

Pool 只会回调它的调用者，所以能通过校验的回调数据一定是 Manager 自己编码的，里面的 `payer` 可信。`UniGoManager.test.js` 的“Pool 真伪校验”用一个伪造的 Pool（`TestFakePool`）验证了它既不能作为 `swap / mint` 的目标，也不能直接调用回调划走已授权用户的 token。

**代码位置**：`contracts/contracts/UniGoManager.sol`, `contracts/test/UniGoManager.test.js`

---

### 10. TWAP 预言机（观测环形数组）

**问题**：池内现价（`slot0.sqrtPriceX96`）可以被一笔大单在单个区块内拉到任意位置，再在同一笔交易里拉回来（闪电贷让这件事几乎零成本）。任何直接读现价做决策的合约或后端都可能被操纵。

**解决方案：时间加权平均价（TWAP）**。攻击者要操纵 TWAP，必须把价格偏离维持很多个区块，期间会被套利者不断搬回，成本远高于操纵单个区块的现价。

**只存累计值**

Pool 不存“每个时刻的价格”，而是存 tick 对时间的累计值：

```
tickCumulative(t) = Σ tick × 持续秒数        （从池子初始化起）

任意窗口 [t1, t2] 的平均 tick = (tickCumulative(t2) - tickCumulative(t1)) / (t2 - t1)
TWAP 价格 = 1.0001^平均tick                  （tick 的算术平均 = 价格的几何平均）
```

每条观测（`Oracle.Observation`）打包在一个 storage slot 里：

| 字段 | 类型 | 含义 |
|------|------|------|
| `blockTimestamp` | uint32 | 观测写入时的区块时间戳 |
| `tickCumulative` | int56 | tick × 时间 的累计值 |
| `secondsPerLiquidityCumulativeX128` | uint160 | 时间 / 活跃流动性 的累计值 |
| `initialized` | bool | 该槽位是否已写入 |

**写入时机**：tick 或活跃流动性**即将变化之前**，用变化前的值写入，每个区块最多一次：

```solidity
// UniGoPool.sol — swap 结尾
if (state.tick != _slot0.tick) {
    // tick 即将变化，先用变化前的 tick 和流动性写入观测
    _writeObservation(_slot0, liquidity);
    (slot0.sqrtPriceX96, slot0.tick) = (state.sqrtPriceX96, state.tick);
}
```

“变化前的值”很关键：上一条观测到现在这段时间里生效的是旧 tick，新 tick 从这一刻才开始计时。同一区块内的第二次写入直接返回，所以区块内无论价格被拉多远，只要在区块结束前拉回来，就不会在累计值里留下痕迹。

**环形数组与扩容**

```
observations[65535]，初始容量 1

  index ─────────────┐                      写满后回到 0，覆盖最老的一条
                     ▼
  ┌─────┬─────┬─────┬─────┬─────┬─────┐
  │ o3  │ o4  │ o5  │ o0  │ o1  │ o2  │     最新 = index，最老 = index + 1
  └─────┴─────┴─────┴─────┴─────┴─────┘
```

容量越大，可查询的历史越久。任何人都可以调用 `increaseObservationCardinalityNext` 付 gas 扩容：

```solidity
// Oracle.sol — grow
// 只占位，initialized 保持 false，查询时会跳过
for (uint16 i = current; i < next; i++) self[i].blockTimestamp = 1;
```

提前把槽位写成非零值，是把“首次写入 storage”的 20000 gas 转移给扩容的人，之后 swap 写观测只需付改写的 gas。新容量在写指针走到数组末尾时才启用，保证环形顺序不被打乱。

**查询：二分 + 插值**

`observe(secondsAgos)` 对每个“多少秒之前”返回当时的累计值，分三种情况：

| 目标时刻 | 处理 |
|---------|------|
| 等于或晚于最新观测 | 用当前 tick 和流动性从最新观测**外推** |
| 落在两条观测之间 | 在环形数组上**二分查找**夹住它的两条相邻观测，再**线性插值** |
| 早于最老的观测 | revert `OLD` |

```solidity
// Oracle.sol — binarySearch
uint256 l = (index + 1) % cardinality; // 最老的观测
uint256 r = l + cardinality - 1;       // 最新的观测
while (true) {
    i = (l + r) / 2;
    beforeOrAt = self[i % cardinality];
    if (!beforeOrAt.initialized) { l = i + 1; continue; }   // 落在尚未写入的槽位上
    atOrAfter = self[(i + 1) % cardinality];

    bool targetAtOrAfter = lte(time, beforeOrAt.blockTimestamp, target);
    if (targetAtOrAfter && lte(time, target, atOrAfter.blockTimestamp)) break;
    if (!targetAtOrAfter) r = i - 1;
    else l = i + 1;
}
```

**uint32 时间戳回绕**：时间戳截断成 32 位存储，所有时间差的运算放在 `unchecked` 块里依赖溢出回绕；比较先后用 `lte(time, a, b)`，以当前时间为参照把回绕过的时间戳加回 2^32 再比。

**测试覆盖**（`Oracle.test.js`，12 个用例）：恒定价格下 TWAP 等于现价；价格变动后 TWAP 与按各段持续时间加权的手算值一致；同一区块时间戳内只写一次观测；查询早于最老观测时 revert；扩容只增不减；写满后覆盖最老观测、剩余历史仍可正确查询。后端读取 TWAP 并用它校验现价的部分见第 19 节。

**代码位置**：`contracts/contracts/lib/Oracle.sol`, `contracts/contracts/UniGoPool.sol`（`observe` `increaseObservationCardinalityNext` `_writeObservation`）, `contracts/test/Oracle.test.js`

---

### 11. 闪电贷套利合约（FlashArbitrage）

**问题**：同一个交易对可以有 0.05% / 0.3% / 1% 三个池子，各自独立定价。某个池子被一笔大单推偏后，两个池子之间就有了价差。谁来把它搬平？

`FlashArbitrage` 演示零本金套利，把第 8 节的闪电贷和 swap 组合成一笔原子交易：

```
以借入 token X 为例（另一种 token 记为 Y）：

  ┌────────────┐ 1. flash 借入 X    ┌──────────────────┐
  │ lender 池   │ ─────────────────→ │  FlashArbitrage  │
  │ （第三个池） │ ←───────────────── │                  │
  └────────────┘ 4. 归还 X + 手续费  └───┬──────────┬───┘
                                        │          │
                        2. 卖出全部 X    │          │ 3. 用全部 Y 买回 X
                                        ▼          ▼
                                 ┌──────────┐ ┌──────────┐
                                 │ sellPool │ │ buyPool  │
                                 │ X 较贵    │ │ X 较便宜  │
                                 └──────────┘ └──────────┘

  5. 剩余的 X 就是利润，转给发起人
```

**为什么放贷池必须是第三个池子**

Pool 的 `flash` 带 `lock` 重入锁：从转出 token 到验证还款之间，这个 Pool 的 `swap` 一律以 `LOK` revert。而套利的两次 swap 恰恰发生在闪电贷的回调里。所以放贷的池子不能是买卖两个池子中的任何一个，必须是另一个持有 X 的池子（例如 X/Z 池）：

```solidity
// FlashArbitrage.sol — arbitrage
require(params.lender != params.sellPool && params.lender != params.buyPool, "LP");
```

**回调里完成套利**：

```solidity
function uniGoFlashCallback(uint256 fee0, uint256 fee1, bytes calldata data) external override {
    require(msg.sender == _expectedCaller, "CB");
    ArbitrageParams memory params = abi.decode(data, (ArbitrageParams));

    // 1. 在较贵的池子把借来的 token 全部卖出
    uint256 otherAmount = _swapAll(params.sellPool, params.borrowToken, params.amount);

    // 2. 在较便宜的池子把换来的 token 全部买回借入的 token
    address otherToken = _otherToken(params.sellPool, params.borrowToken);
    _swapAll(params.buyPool, otherToken, otherAmount);

    // 3. 归还本金 + 手续费；利润必须为正且不低于 minProfit，否则整笔回滚
    uint256 owed = params.amount + (params.borrowToken == IUniGoPool(msg.sender).token0() ? fee0 : fee1);
    uint256 balance = IERC20(params.borrowToken).balanceOf(address(this));
    require(balance > owed && balance - owed >= params.minProfit, "NP");

    IERC20(params.borrowToken).safeTransfer(msg.sender, owed);
    _expectedCaller = address(0);   // 套利结束，不再接受任何回调
}
```

**安全设计**：

| 机制 | 作用 |
|------|------|
| `minProfit` + `NP` 检查 | 无利可图（价差不够覆盖两池手续费 + 闪电贷手续费、方向弄反、被人抢先）时整笔回滚，发起人只损失 gas |
| `_expectedCaller` | 每一步只接受“当前应该回调我的那个 Pool”的回调；套利过程之外为零地址，任何回调都被拒绝 |
| Factory 校验 | 三个池子都必须是 Factory 创建的，买卖两个池子必须是同一交易对 |
| 部分成交回滚 | `_swapAll` 要求输入全部成交（`PF`），否则剩余的 token 会滞留在合约里被后来的调用者拿走 |

套利的副作用是两个池子的价格向彼此收敛——这正是多池市场保持价格一致的机制。借多少最赚、要不要扣 gas，由后端的套利扫描器在链下算好（第 25 节）。

**测试覆盖**（`FlashArbitrage.test.js`，10 个用例）：发起人零本金净赚且利润与事件一致、两池价格收敛、放贷池只赚不亏且 token 总量守恒、利润低于 `minProfit` 时 revert、方向弄反时 revert、借入 token1 反向套利同样获利、价格一致时 revert 且状态不变、参数与权限校验。

**代码位置**：`contracts/contracts/FlashArbitrage.sol`, `contracts/test/FlashArbitrage.test.js`

---

### 12. 安全加固：tick 流动性上限与 SafeERC20

#### 单个 tick 的流动性上限

**问题**：`liquidityNet` 是 `int128`，全局活跃流动性是 `uint128`。如果单个 tick 上能堆积任意多的流动性，攻击者可以在远离现价的极端区间（本金几乎为零）反复 mint 接近 `int128` 上限的流动性，让 tick 的 `liquidityNet` 溢出回绕。此后价格进入该区间时，全局流动性不含这笔巨额流动性，手续费却按巨额流动性结算，攻击者就能把池子里其他 LP 的 token “领”走。

**防线一：按 tickSpacing 推导每个 tick 的上限**（与 Uniswap V3 相同的算法）

```solidity
// Tick.sol
function tickSpacingToMaxLiquidityPerTick(int24 tickSpacing) internal pure returns (uint128) {
    int24 minTick = (TickMath.MIN_TICK / tickSpacing) * tickSpacing;
    int24 maxTick = (TickMath.MAX_TICK / tickSpacing) * tickSpacing;
    uint24 numTicks = uint24((maxTick - minTick) / tickSpacing) + 1;
    return type(uint128).max / numTicks;
}
```

把 `uint128` 的容量平均分给所有可用的 tick：即使每个 tick 都被用满，全局活跃流动性也不会溢出 `uint128`，`liquidityNet` 也不可能溢出 `int128`。上限在 Pool 构造时算好存为 immutable，`Tick.update` 里检查：

```solidity
require(liquidityGrossAfter <= maxLiquidity, "LO");
```

| 费率 | tickSpacing | `maxLiquidityPerTick` |
|------|------------|----------------------|
| 0.05% | 10 | 1917569901783203986719870431555990 |
| 0.3% | 60 | 11505743598341114571880798222544994 |
| 1% | 200 | 38350317471085141830651933667504588 |

这三个值与 Uniswap V3 对应费率池的已知值一致（`Security.test.js`）。

**防线二：受检运算，溢出时 revert 而不是回绕**

```solidity
// Tick.sol — update：直接用 int128 做受检运算
info.liquidityNet = upper
    ? info.liquidityNet - liquidityDelta
    : info.liquidityNet + liquidityDelta;

// UniGoPool.sol — mint：流动性变化量以 int128 记账，超过上限会被截断成负数
require(amount <= uint128(type(int128).max), "AO");
```

测试用 `TestTickHarness` 绕开上限单独验证库本身：即使把上限放到 `uint128` 最大值，第二次累加也以 `Panic(0x11)` revert。

#### SafeERC20：兼容不规范的代币

**问题**：ERC20 标准规定 `transfer` 返回 `bool`，但现实中的代币并不都守规矩：

| 代币行为 | 裸调用 `IERC20.transfer` 的后果 |
|---------|-------------------------------|
| 没有返回值（USDT 式） | Solidity 解码返回值失败，交易 revert —— 这类代币根本用不了 |
| 失败时返回 `false` 而不 revert | 调用方不检查返回值就会继续执行 —— 账记了、币没转 |

Pool、Manager、FlashArbitrage 的所有转账都改用 OpenZeppelin 的 `SafeERC20`（`safeTransfer / safeTransferFrom`）：没有返回值视为成功，返回 `false` 视为失败并 revert。

`Security.test.js` 用两个专门的测试代币覆盖：`TestNoReturnERC20` 能正常 mint、双向 swap、burn、collect、flash；`TestFalseReturnERC20` 在转账失败时 collect 回滚（可领取数量不会被白白扣掉）、swap 的输出转账失败时回滚（用户不会付了钱却收不到币）、付款方余额不足时 mint 回滚。

**代码位置**：`contracts/contracts/lib/Tick.sol`, `contracts/contracts/UniGoPool.sol`, `contracts/test/Security.test.js`, `contracts/contracts/test/`

---

### 13. The Graph 子图与多周期 K 线

**问题**：前端要展示池子列表、历史成交、K 线、累计成交量。这些数据都在链上事件里，但直接用 RPC 扫事件既慢又不能做聚合查询。

`subgraph/` 是一个 The Graph 子图：把合约事件映射成可用 GraphQL 查询的实体。

**动态数据源**

Factory 是唯一的静态数据源。每创建一个新池，Factory 的 handler 就为它启动一个 Pool 模板实例，此后这个池子的事件才会被索引：

```typescript
// subgraph/src/factory.ts
export function handlePoolCreated(event: PoolCreated): void {
  // ... 更新 Factory.poolCount，创建 Pool 实体
  PoolTemplate.create(event.params.pool);
}
```

**事件到实体的映射**：

| 事件 | handler | 写入 |
|------|---------|------|
| `PoolCreated` | `handlePoolCreated` | `Factory`（池子数）、`Pool`（token、费率、tickSpacing） |
| `Initialize` | `handleInitialize` | `Pool` 的初始价格和 tick |
| `Swap` | `handleSwap` | `Swap` 实体；`Pool` 的价格、tick、活跃流动性、累计成交量；四个周期的 `Candle` |
| `Mint` / `Burn` | `handleMint` / `handleBurn` | `Mint` / `Burn` 实体；用 `try_slot0()` / `try_liquidity()` 读合约刷新 `Pool` 状态 |
| `Flash` | `handleFlash` | `Flash` 实体；`Pool` 的累计闪电贷手续费 |

`Swap / Mint / Burn / Flash` 是 immutable 实体（id = 交易哈希 + 日志序号），`Pool / Factory / Candle` 是可变实体。

**多周期 K 线**

每笔 Swap 同时更新 1m / 5m / 1h / 1d 四个周期的 K 线，id 为 `{池子地址}-{周期}-{起始时间}`：

```typescript
// subgraph/src/pool.ts — updateCandles
const INTERVAL_NAMES: string[] = ["1m", "5m", "1h", "1d"];
const INTERVAL_SECONDS: i32[] = [60, 300, 3600, 86400];

const periodStart = (timestamp / seconds) * seconds;   // 对齐到周期起点
let candle = Candle.load(id);
if (candle == null) {
  candle = new Candle(id);
  candle.open = openPrice;      // 开盘价 = 周期内第一笔成交前的价格
  candle.high = openPrice;
  candle.low = openPrice;
  // ...
}
if (closePrice.gt(candle.high)) candle.high = closePrice;
if (closePrice.lt(candle.low)) candle.low = closePrice;
candle.close = closePrice;      // 收盘价 = 周期内最后一笔成交后的价格
```

价格由 `sqrtPriceX96` 计算：`price = sqrtPriceX96² / 2^192`，即 1 个 token0 值多少 token1（原始单位的比值）。

**查询示例**：

```graphql
{
  candles(
    where: { pool: "0x…", interval: "1h" }
    orderBy: periodStart
    orderDirection: desc
    first: 24
  ) {
    periodStart open high low close volumeToken0 volumeToken1 txCount
  }
}
```

**与后端 K 线模块的分工**：后端的 `kline` 包（第 23 节）在内存里维护最近的 K 线并实时推送，延迟低；子图把全部历史持久化，支持任意条件的聚合查询。

> 使用说明：子图需要自行部署 graph-node 来运行。`subgraph.yaml` 默认指向本地 Hardhat 节点（network `localhost`）和 `scripts/deploy.js` 在全新节点上部署出的 Factory 地址，部署到其它网络时修改 `network / address / startBlock`。构建步骤见[快速开始](#快速开始)。

**代码位置**：`subgraph/schema.graphql`, `subgraph/subgraph.yaml`, `subgraph/src/factory.ts`, `subgraph/src/pool.ts`

---

## Go 后端 — 核心业务

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
3. 端到端集成测试：对真实节点上的池子，链下报价与链上 `UniGoQuoter` 的返回值逐位比对（见[测试体系](#测试体系)）

**代码位置**：`backend/math/tickmath.go`, `backend/math/sqrtpricemath.go`, `backend/math/tickmath_test.go`

---

### 15. chain 包：基于 go-ethereum 的链交互层

**问题**：后端要读区块、拉日志、解码事件、调合约、广播交易。这些如果散落在各模块里各写一套 JSON-RPC，事件签名和 ABI 编码很容易和合约对不上。

`backend/chain` 把所有链交互收拢到一个 `Client`，底层是 go-ethereum 的 `ethclient`：

| 能力 | 方法 | 用到它的模块 |
|------|------|-------------|
| 区块头（hash / parentHash / timestamp） | `LatestBlock` `BlockByNumber` | 索引器 |
| 按区块拉日志并解码成事件 | `BlockEvents` | 索引器 |
| 合约只读调用 | `Slot0` `PoolLiquidity` `TWAP` `QuoteExactInput` | TWAP 校验、链上报价抽检 |
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
    // ... UniGoFactory / UniGoManager / UniGoQuoter / FlashArbitrage

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

- **按区块哈希过滤**（`FilterQuery.BlockHash`）：区块已被 Reorg 掉时节点会报错，而不是返回另一条分叉上的日志
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
    BlockEvents(ctx context.Context, block *BlockInfo) ([]interface{}, error) // 已解码，按 logIndex 升序
}
```

- 配置了 `RPC_URL` 和 `FACTORY_ADDRESS` 且节点连得上：数据源是 `chain.Client`，区块哈希、父哈希、事件全部来自真实节点
- 否则：数据源是内置的模拟源（`sim_source.go`），只出父哈希衔接的空区块，保证后端不配节点也能启动

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

**验证**：`indexer_test.go` 用可编程的假数据源构造分叉；`chain_test.go` 在 go-ethereum 模拟后端上做真实的链重组；集成测试在 Hardhat 节点上用 `evm_snapshot / evm_revert` 制造 Reorg，断言回滚后链下状态与链上 `slot0 / liquidity` 完全一致。

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
│ 超时 / 无可用路径？→ 进入 Level 2      │
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

**缓存预热（防止冷启动雪崩）**：

```go
// 系统启动时预加载 Top N 交易对
// 使用 10x TTL，确保预热数据在系统稳定前不过期
func (c *Cache) Warmup(config WarmupConfig, loader func(...) (interface{}, error)) int {
    for _, pair := range config.TopPairs {
        val, err := loader(pair.Token0, pair.Token1, pair.Fee)
        if err != nil { continue }  // 预热失败不阻塞启动
        c.SetWithTTL(key, val, c.ttl*10)  // 10x TTL
    }
}
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

**代码位置**：`backend/cache/cache.go`, `backend/api/server.go`

---

### 19. 价格安全：现价 / TWAP / Chainlink 三方交叉校验

**核心原则：不盲信任何单一价格源。**

| 价格源 | 特点 | 弱点 |
|--------|------|------|
| 池内现价（`sqrtPriceX96`） | 实时 | 一笔大单（或闪电贷）就能在单个区块内拉偏 |
| 池内 TWAP（第 10 节） | 要整个窗口都顶住才会动，操纵成本高 | 对真实的快速行情反应慢 |
| Chainlink | 独立于本池子 | 有心跳间隔，可能过期 |

三者两两比较，各管一类风险：

```
                 ┌──────────────┐
                 │  Chainlink   │
                 └──┬────────┬──┘
        偏离 > 2%   │        │   偏离 > 2%
        保护模式    │        │   保护模式（不是瞬时波动）
              ┌─────▼──┐  ┌──▼─────┐
              │ 池内现价 │◄─►│池内 TWAP│
              └────────┘  └────────┘
                  偏离 > 5%：疑似单区块操纵
                  交易前检查直接拒绝
```

**现价 vs TWAP：操纵拒绝**

每次报价、构造交易、提交交易，都会对路由经过的每个池子读一次链上 TWAP（`chain.Client.TWAP` 调用池子的 `observe([window, 0])`，窗口默认 600 秒），与索引器里的现价比对：

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

**Chainlink 有效性与保护模式**

```go
// oracle/oracle.go
func (o *Oracle) UpdateChainlinkPrice(pair string, price float64, updatedAt time.Time) {
    // 关键：不只存价格，还校验 updatedAt
    isStale := time.Since(updatedAt) > o.config.StaleThreshold  // 默认 1 小时
    // ...
    if isStale {
        o.activateProtection(pair, "Chainlink 价格过期…")
    }
}
```

| 事件 | 操作 |
|------|------|
| Chainlink `updatedAt` 超过 1 小时 | 激活保护模式；过期价格不再参与比较，在它更新之前不解除 |
| 池内现价与 Chainlink 偏离 > 2% | 激活保护模式 |
| 池内 TWAP 与 Chainlink 偏离 > 2% | 激活保护模式 |
| 池内现价与 TWAP 偏离 > 5% | 激活保护模式（同时该笔交易被拒绝） |
| 全部恢复正常且冷却时间（5 分钟）已过 | 解除保护模式 |

保护模式下不一刀切拒绝交易（避免变成 DoS），而是要求滑点设置不低于 0.5%，不满足的请求被拒绝，满足的放行并附带风险提示。

池内现价由两条路径喂给预言机：索引器每处理一笔 Swap 推送一次（`PoolPriceFeed`），以及每次 TWAP 校验时连同 TWAP 一起写入。

> 使用说明：Chainlink 价格通过 `Oracle.UpdateChainlinkPrice` 注入，由接入方的喂价任务调用；未注入时交叉校验在池内现价与 TWAP 两者之间进行。

**代码位置**：`backend/oracle/oracle.go`, `backend/chain/contracts.go`（`TWAP`）, `backend/api/trade.go`（`checkPriceSafety`）

---

### 20. 待签名交易构造与交易状态查询

**问题**：后端算出了最优路径，但用户怎么把它变成一笔链上交易？后端**不托管私钥**，不能替用户签名。

**流程**：

```
1. POST /api/v1/swap/build
     后端：搜索路由 → 价格安全检查 → MEV 检查 → 链上报价抽检
           → 把路由编码成 Manager 的调用数据
     返回：{ to, data, value, chain_id, gas_limit, gas, min_amount_out, deadline, ... }

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

---

## Go 后端 — 行情与 MEV

### 23. K 线与成交记录（Reorg 重算）

**问题**：前端要画 K 线、展示最近成交。这些数据都来自 Swap 事件，而 Swap 事件所在的区块可能被 Reorg 掉，已经画出去的 K 线就包含了不存在的成交。

`kline.Aggregator` 订阅索引器的 Swap 事件流，按池子聚合出 **1m / 5m / 15m / 1h / 4h / 1d** 六个周期的 OHLCV，并保留最近的逐笔成交：

- **价格**：由成交后的 `sqrtPriceX96` 换算，`price = (sqrtPriceX96 / 2^96)²`
- **成交量**：`amount0 / amount1` 的绝对值
- **时间**：区块时间戳

每笔成交依次并入六个周期的最后一根 K 线（或开一根新的）：

```go
// kline/kline.go
func (a *Aggregator) merge(series []Candle, secs int64, c Candle) []Candle {
    c.OpenTime = bucket(c.OpenTime, secs)             // 对齐到周期起点
    if n := len(series); n > 0 && series[n-1].OpenTime == c.OpenTime {
        last := &series[n-1]
        last.High = max(last.High, c.High)
        last.Low = min(last.Low, c.Low)
        last.Close = c.Close
        last.Volume0 += c.Volume0
        last.Volume1 += c.Volume1
        last.Trades += c.Trades
        return series
    }
    // ... 追加新的一根，超过 MaxCandles 时淘汰最老的
}
```

**Reorg 重算**

区块时间戳随区块号单调递增，所以回滚区块 `>= forkBlock` 时可以精确地只重算受影响的部分：

```
成交（按时间）:  t1  t2  t3 │ t4  t5  t6        │ = 分叉点，t4 起被回滚
1m K 线:        [ m1 ][  m2    ][ m3 ]          t4 落在 m2 里

1. 丢弃成交 t4、t5、t6
2. 各周期：删除起点 >= m2 所在周期的 K 线
3. 1m：用 m2 这一分钟内幸存的成交（t3）重算 m2
4. 5m / 15m / 1h / 4h / 1d：受影响的那一根，由 1m K 线重新合并
```

随后索引器从分叉点重新索引，新链上的成交按正常流程再次进入聚合器。

**内存上限**：每个池子每个周期最多保留 1500 根 K 线、1000 笔成交，最多跟踪 1024 个池子。

查询接口：`GET /api/v1/klines?pool=&interval=&limit=`、`GET /api/v1/trades?pool=&limit=`；每笔成交和更新后的 1m K 线同时经 SSE 推送（第 26 节）。链上历史的持久化索引由子图承担（第 13 节）。

**代码位置**：`backend/kline/kline.go`, `backend/kline/kline_test.go`

---

### 24. 三明治攻击检测与交易前保护

**三明治攻击**：攻击者看到受害者的大额交易后，抢在前面同向买入推高价格，等受害者以更差的价格成交，再反向卖出获利。受害者能被夹多狠，取决于他愿意接受多差的成交价（`min_amount_out` 设得多松）。

```
同一区块、同一池子内，按 logIndex 排序：

  攻击者  买入 token1  ──→ 价格被推高        （抢跑）
  受害者  买入 token1  ──→ 以更差的价格成交   （同向）
  攻击者  卖出 token1  ──→ 价格回落，差价入袋 （反向平仓）
```

**检测**

`mev.Guard` 订阅 Swap 事件流，把当前区块内每个池子的 swap 按 `logIndex` 排好。每来一笔新的 swap，就把它当作“反向平仓”向前找配对的抢跑交易：

```go
// mev/mev.go — findFrontRun
// 配对条件：
//   - 抢跑交易与平仓交易来自同一地址（sender 和 recipient 都相同）、方向相反
//   - 两者之间至少有一笔别人的、与抢跑同向的交易（受害者）
for front = back - 1; front >= 0; front-- {
    f := swaps[front]
    if !sameActor(f, b) || zeroForOne(f) == zeroForOne(b) || f.TxHash == b.TxHash {
        continue
    }
    victims = nil
    for _, v := range swaps[front+1 : back] {
        if !sameActor(v, f) && zeroForOne(v) == zeroForOne(f) && v.TxHash != f.TxHash && v.TxHash != b.TxHash {
            victims = append(victims, v.TxHash)
        }
    }
    if len(victims) > 0 {
        return front, victims
    }
}
```

攻击者按 `(sender, recipient)` 一起识别：经路由合约下单时 `sender` 都是路由合约，要靠 `recipient` 才能区分是谁。命中后记录告警（攻击者、抢跑交易、平仓交易、受害交易列表）、计入 `mev_sandwich_detected_total`、经 SSE 推送；Reorg 时丢弃分叉点之后的告警。

**交易前保护**

| 检查 | 规则 | 默认值 |
|------|------|--------|
| 价格冲击 | `impact = 1 - 报价产出 / 按现价（扣手续费）应得产出`，过大则拒绝 | > 5% 拒绝 |
| deadline | 已过期，或距当前时间过长（长 deadline 给了攻击者择时的空间） | 最长 30 分钟 |
| `min_amount_out` | 低于 `报价 × (1 - 最大滑点)` 视为过松，拒绝 | 最大滑点 50% |
| 推荐滑点 | 用户未指定时用默认值；路径上有池子近期被夹过时收紧并提示 | 默认 0.5%，10 分钟内被夹过收紧到 0.1% |

这些检查在 `POST /api/v1/swap/build` 和 `POST /api/v1/swap` 里执行，报价接口返回 `price_impact`、推荐的 `slippage` 和 `min_amount_out`。告警列表通过 `GET /api/v1/mev/alerts` 查询。

**代码位置**：`backend/mev/mev.go`, `backend/mev/mev_test.go`

---

### 25. 套利机会扫描（三分搜索最优借入量）

**问题**：同一个币对可以有多个费率档的池子，它们的价格由各自的成交决定，不会自动对齐。出现价差时可以用第 11 节的 `FlashArbitrage` 合约零本金套利。但**借多少**？

借得越多，卖出池的价格被自己压得越低、买入池的价格被自己抬得越高，边际利润递减，借过头反而亏：

```
利润
 │        ╭───╮
 │      ╭─╯   ╰─╮
 │    ╭─╯       ╰──╮
 │  ╭─╯            ╰───╮
 └──┴───────────────────┴──→ 借入数量
    1       最优         上界（两池价格被搬平）
```

利润对借入数量是**单峰**的，用三分搜索找峰值：每次把区间三等分，比较两个分点的利润，丢掉利润较低一侧的三分之一。

```go
// arb/arb.go — OptimalAmount
// 搜索上界：把卖出池的价格一路压到买入池现价需要卖多少。超过这个量两池价格已经倒挂，一定更亏
toParity, err := router.SimulateSwap(sell, zeroForOne, huge, buy.SqrtPriceX96)

three := big.NewInt(3)
for new(big.Int).Sub(hi, lo).Cmp(three) > 0 {
    third := new(big.Int).Sub(hi, lo)
    third.Div(third, three)
    m1 := new(big.Int).Add(lo, third)
    m2 := new(big.Int).Sub(hi, third)
    if less(profit(m1), profit(m2)) {
        lo = m1
    } else {
        hi = m2
    }
}
// 区间缩到 3 以内后逐个比较，取利润最高的
```

**利润函数直接用第 17 节的逐 tick 模拟**，包含两池的手续费、tick 穿越和取整，闪电贷手续费按链上规则向上取整，所以链下算出的利润就是链上执行能拿到的利润：

```go
func Profit(sell, buy indexer.PoolSnapshot, borrow string, lenderFee uint32, amount *big.Int) *big.Int {
    otherAmount, _, err := router.QuoteExactInputSingle(sell, borrow, amount)   // 卖出借入的 token
    // ...
    back, _, err := router.QuoteExactInputSingle(buy, other, otherAmount)        // 再买回来
    // ...
    flashFee := umath.MulDivRoundingUp(amount, big.NewInt(int64(lenderFee)), big.NewInt(1e6))
    profit := new(big.Int).Sub(back, amount)
    return profit.Sub(profit, flashFee)
}
```

**扫描流程**（每笔 Swap 触发，只比较成交的那个池子和同币对的其它池子）：

1. **定方向**：价格高的池子里 token0 更贵。借 token0 就在高价池卖、低价池买回；借 token1 反过来
2. **选放贷池**：含有借入 token、既不是卖出池也不是买入池、活跃流动性最大的池子（必须是第三个池子的原因见第 11 节）
3. **快速排除**：借入量趋近于 0 时的边际利润都不为正（`价格比 × (1-卖出池费率) × (1-买入池费率) <= 1 + 闪电贷费率`），不必搜索
4. **三分搜索**最优借入量和毛利润
5. **扣 gas**：用第 22 节的 `CostWei` 算出一笔套利的 gas 成本，按 WETH 直连池的现价折算成借入 token；净利润不为正则放弃
6. **编码**：生成 `FlashArbitrage.arbitrage` 的调用数据，`minProfit` 设为 gas 成本——链上实际利润达不到就整笔 revert

机会通过 `GET /api/v1/arbitrage/opportunities` 查询（净利润高的在前）并经 SSE 推送；池子再次成交后重新评估，无利可图时移除，Reorg 时全部作废。本模块只产出机会和调用数据，不代签、不自动发交易。

集成测试里的套利场景：在真实节点上制造价差 → 扫描器给出机会 → 用它的调用数据原样发起交易 → 发起人账户实际增加的余额与链下预测的毛利润逐位相等，执行后同一机会的残余利润不到原来的 1%。

> 使用说明：`WETH_ADDRESS` 用于把 gas 成本折算成借入 token，未配置时响应里 `gas_priced=false`；`FLASH_ARB_ADDRESS` 用于填充响应里的 `to`。

**代码位置**：`backend/arb/arb.go`, `backend/arb/arb_test.go`

---

### 26. SSE 实时推送

**为什么需要实时推送？**

| 方式 | 延迟 | 带宽 | 说明 |
|------|------|------|------|
| HTTP 轮询 | 轮询间隔/2 | 高（大量无效请求） | 100 用户 × 每秒 1 次 = 100 QPS 无效负载 |
| SSE 推送 | 毫秒级 | 低（仅推送变化） | 单向，服务端→客户端 |
| WebSocket | 毫秒级 | 低 | 双向，更复杂 |

本实现使用 SSE（Server-Sent Events），因为价格推送是单向的（服务端→客户端），SSE 更简单且浏览器原生支持。

**推送的事件类型**：

| 事件 | 触发时机 | 内容 |
|------|---------|------|
| `trade` | 每笔 Swap | 成交记录（池子、价格、数量、方向、交易哈希） |
| `kline` | 每笔 Swap | 更新后的 1m K 线 |
| `mev_alert` | 检测到三明治 | 告警详情 |
| `arbitrage` | 发现套利机会 | 机会与调用数据 |

**Hub 发布-订阅模式**：

```
  Indexer（Swap 事件，索引协程内同步回调）
      │
      ├─→ kline.OnTrade ──────→ hub.Broadcast("trade" / "kline")
      ├─→ mev.OnAlert ────────→ hub.Broadcast("mev_alert")
      └─→ arb.OnOpportunity ──→ hub.Broadcast("arbitrage")
                                        │ 非阻塞扇出
              ┌─────────────────────────┼─────────────────────────┐
         ┌────▼─────┐              ┌────▼─────┐              ┌────▼─────┐
         │ Client A │              │ Client B │              │ Client C │
         │ 队列 256  │              │ 队列 256  │              │ 队列 256  │
         └──────────┘              └──────────┘              └──────────┘
```

**慢客户端不能拖住索引器**

`Broadcast` 是在索引协程里被同步调用的，它一旦阻塞，整个索引就停了。所以扇出时对每个客户端的有界队列做**非阻塞写**，队列满了说明客户端消费不过来，直接断开它：

```go
// ws/ws.go
func (h *Hub) Broadcast(kind string, payload interface{}) {
    data, err := json.Marshal(payload)
    // ...
    h.mu.Lock()
    defer h.mu.Unlock()
    for c := range h.clients {
        if c.kinds != nil && !c.kinds[kind] {
            continue                      // 客户端没订阅这类事件
        }
        select {
        case c.send <- msg:
        default:
            h.drop(c)                     // 队列已满：断开慢客户端
            metrics.StreamDroppedClients.Inc()
        }
    }
}
```

被断开的客户端重连后从最新数据继续，而不是让它的积压反过来卡住所有人。同时在线的客户端上限是 1024，超过时新的订阅返回 503。

**SSE 协议细节**：

```go
// SSEHandler — 每条消息带事件类型
fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.kind, msg.data)
```

- `?types=trade,kline` 只订阅指定类型，不传则订阅全部
- 每 15 秒发一行 `: heartbeat` 注释保活
- 长连接用 `http.ResponseController` 取消服务器的写超时；日志中间件包装的 `ResponseWriter` 透传 `Flush`，否则流式响应发不出去
- 服务关闭时 Hub 主动断开所有客户端，否则长连接会拖住 HTTP 服务器的优雅关闭

**客户端使用**：

```javascript
// 浏览器原生 EventSource，自动重连
const es = new EventSource("/api/v1/stream?types=trade,kline");
es.addEventListener("trade", (event) => {
    const trade = JSON.parse(event.data);
    console.log(`${trade.pool}: ${trade.price}`);
});
es.addEventListener("kline", (event) => {
    const { pool, interval, candle } = JSON.parse(event.data);
});
```

**代码位置**：`backend/ws/ws.go`, `backend/ws/ws_test.go`

---

## Go 后端 — 工程基础设施

### 27. 断路器（Circuit Breaker）

**问题**：后端的索引、TWAP 读取、链上报价抽检、Gas 费用查询、交易广播都要调用以太坊 RPC 节点。当节点故障时：


```
正常情况：
  请求 → RPC 节点 → 响应 (10ms)

外部服务故障，无断路器：
  请求 → RPC 节点 → 等待超时... (30s) → 失败
  请求 → RPC 节点 → 等待超时... (30s) → 失败
  请求 → RPC 节点 → 等待超时... (30s) → 失败
  ↓
  所有 goroutine 阻塞等待 → 请求堆积 → 内存飙升 → OOM

外部服务故障，有断路器：
  前 5 次失败 → 断路器打开 → 后续请求直接返回错误 (0ms)
  30 秒后 → 断路器半开 → 放行 1 个探测请求
  探测成功 → 断路器关闭 → 恢复正常
  探测失败 → 断路器重新打开 → 继续保护
```

**三态状态机**：

```
         ┌────────────────┐
         │    Closed      │ ← 正常状态
         │ 放行所有请求     │
         │ 统计连续失败次数 │
         └───────┬────────┘
                 │ 连续失败 ≥ MaxFailures (默认 5)
                 ▼
         ┌────────────────┐
         │     Open       │ ← 熔断状态
         │ 拒绝所有请求    │
         │ (快速失败 0ms)  │
         └───────┬────────┘
                 │ 冷却时间到 (默认 30s)
                 ▼
         ┌────────────────┐    探测成功    ┌──────────┐
         │   HalfOpen     │ ─────────────→ │ Closed   │
         │ 放行 1 个探测   │               └──────────┘
         │ 成功则恢复      │    探测失败    ┌──────────┐
         │ 失败则重新熔断  │ ─────────────→ │  Open    │
         └────────────────┘               └──────────┘
```

**实现细节**：

```go
// circuitbreaker.go
func (cb *CircuitBreaker) Execute(fn func() (interface{}, error)) (interface{}, error) {
    cb.mu.Lock()

    switch cb.state {
    case StateOpen:
        if time.Since(cb.lastFailureTime) > cb.config.Timeout {
            // 冷却时间到，转入 HalfOpen 进行探测
            cb.setState(StateHalfOpen)
            cb.halfOpenCount = 0
        } else {
            cb.mu.Unlock()
            return nil, ErrCircuitOpen  // 快速失败，不执行 fn
        }

    case StateHalfOpen:
        if cb.halfOpenCount >= cb.config.HalfOpenMaxRequests {
            cb.mu.Unlock()
            return nil, ErrCircuitOpen  // 探测配额用完
        }
        cb.halfOpenCount++
    }

    cb.mu.Unlock()

    // 执行实际操作（只在 Closed 或 HalfOpen 时执行）
    result, err := fn()

    cb.mu.Lock()
    defer cb.mu.Unlock()
    if err != nil {
        cb.onFailure()  // 累加失败计数，可能触发 Closed→Open
    } else {
        cb.onSuccess()  // 重置计数，HalfOpen→Closed
    }
    return result, err
}
```

**与 RPC 客户端的集成**：

`chain.Client` 的每一次 RPC 调用都经过同一个断路器（第 15 节的 `call`）。断路器状态变化时写日志并更新 `circuitbreaker_open` 指标：

```go
// chain/client.go — NewClient
cfg := circuitbreaker.DefaultConfig()
cfg.OnStateChange = func(from, to circuitbreaker.State) {
    slog.Warn("RPC 断路器状态变化", "from", from.String(), "to", to.String())
    if to == circuitbreaker.StateOpen {
        metrics.CircuitBreakerOpen.Set(1)
    } else {
        metrics.CircuitBreakerOpen.Set(0)
    }
}
```

节点熔断期间各模块各自降级：索引器本轮轮询失败、下一轮重试；Gas 评估退回静态值；TWAP 校验返回 `unavailable` 并提示用户。

**代码位置**：`backend/circuitbreaker/circuitbreaker.go`, `backend/circuitbreaker/circuitbreaker_test.go`, `backend/chain/client.go`

---

### 28. Goroutine 池与性能优化

**问题**：Go 的 goroutine 虽然轻量（初始栈仅 2KB），但在高并发场景下无限制地 `go func()` 仍然有代价：

| 问题 | 原因 | 影响 |
|------|------|------|
| 调度开销 | runtime 调度器管理百万级 G-M-P | CPU 浪费在调度而非计算 |
| 内存压力 | 每个 goroutine 栈会动态增长 | 万级 goroutine × 几 KB = 可观内存 |
| GC 压力 | goroutine 引用的对象延迟回收 | GC STW 时间随 goroutine 数增加 |

**池化方案**：

```
朴素方案：go func()
  ┌─────────┐ ┌─────────┐ ┌─────────┐     ┌─────────┐
  │ Task 1  │ │ Task 2  │ │ Task 3  │ ... │ Task N  │
  │ goroutine│ │ goroutine│ │ goroutine│     │ goroutine│
  └─────────┘ └─────────┘ └─────────┘     └─────────┘
  → N 个任务 = N 个 goroutine（创建/销毁开销）

池化方案：Worker Pool
  ┌────────────────────────────────────────────┐
  │            taskQueue (有界 Channel)          │
  │  [ Task 1 | Task 2 | Task 3 | ... ]        │
  └──────┬──────────┬──────────┬───────────────┘
         │          │          │
    ┌────▼───┐ ┌───▼────┐ ┌──▼─────┐
    │Worker 1│ │Worker 2│ │Worker N│  ← 固定数量，长驻复用
    │(复用)   │ │(复用)   │ │(复用)   │
    └────────┘ └────────┘ └────────┘
  → N 个任务 = 固定 W 个 goroutine（零创建/销毁开销）
```

**核心实现**：

```go
// pool.go — Worker 循环
// 优先从 taskQueue 取任务执行，channel 关闭后自动退出
func (p *Pool) worker(id int) {
    defer p.wg.Done()
    for task := range p.taskQueue {  // channel 关闭时自动退出循环
        task()
        p.completed.Add(1)
    }
}

// 非阻塞提交（队列满时返回 false，调用方决定降级策略）
func (p *Pool) Submit(task Task) bool {
    p.submitted.Add(1)
    select {
    case p.taskQueue <- task:
        return true
    default:
        p.rejected.Add(1)
        return false  // 队列已满，调用方可以：go func() / 降级 / 返回错误
    }
}

// 优雅关闭：先清空队列再退出 Worker
func (p *Pool) Shutdown() {
    close(p.taskQueue)  // 1. 关闭 channel → 停止接受新任务
    p.wg.Wait()         // 2. 等待 Worker 消费完剩余任务
    p.cancel()          // 3. 清理 context
}
```

**泛型支持（Go 1.18+）**：

```go
// 提交任务并等待结果（泛型版本）
// 队列已满、任务被拒绝时在调用方协程里直接执行，否则等结果的一方会永远收不到值
func SubmitAndWaitResult[T any](p *Pool, fn func() T) <-chan T {
    ch := make(chan T, 1)
    if !p.Submit(func() { ch <- fn() }) {
        ch <- fn()
    }
    return ch
}

// 使用示例
result := <-SubmitAndWaitResult(pool, func() int {
    return computeExpensiveResult()
})
```

**在本项目中的使用**：路由引擎用一个 `WorkerCount = runtime.NumCPU()`、队列长度 256 的池并发评估候选路径（第 17 节）。

**Benchmark 实测数据**见 [Benchmark 性能对比](#benchmark-性能对比)：10000 个任务时池化方案的内存分配减少 86%、分配次数减半。

**代码位置**：`backend/workerpool/pool.go`, `backend/workerpool/pool_test.go`

---

### 29. 结构化日志与链路追踪

**为什么用 slog 而不是 `log.Printf`？**

| 维度 | log.Printf | slog (Go 1.21+) |
|------|-----------|-----------------|
| 输出格式 | 自由文本，难以机器解析 | JSON / Text，可被 ELK/Loki 直接采集 |
| 日志级别 | 无 | Debug / Info / Warn / Error |
| 结构化字段 | 手动拼接字符串 | 原生键值对 `slog.String("key", val)` |
| 上下文传递 | 不支持 | 通过 context 携带 Trace ID |
| 性能 | 每次调用都格式化 | Handler 接口支持零分配优化 |

**Trace ID 链路追踪**：

一个用户请求可能经过多个模块（API → Router → Indexer → Cache）。通过 context 传递 Trace ID，可以在日志中串联整个链路：

```go
// logging.go — TraceID 注入
func WithTraceID(ctx context.Context) context.Context {
    id := generateTraceID()  // 12 字节随机 hex
    return context.WithValue(ctx, traceIDKey, id)
}

// L(ctx) 获取携带 trace_id 的 Logger
func L(ctx context.Context) *slog.Logger {
    logger := slog.Default()
    if traceID := GetTraceID(ctx); traceID != "" {
        logger = logger.With("trace_id", traceID)
    }
    return logger
}
```

**使用示例**：

```go
// 初始化：级别 + 输出格式（json / text）
logging.Init("info", "json")

// 请求入口处生成 TraceID
ctx = logging.WithTraceID(ctx)

// 业务代码中使用
logging.L(ctx).Info("处理 swap 请求",
    "token_in", "WETH",
    "token_out", "USDC",
    "amount", "1000000000000000000",
)
```

**JSON 日志输出**：

```json
{
  "time": "2024-01-01T00:00:00.123Z",
  "level": "INFO",
  "msg": "处理 swap 请求",
  "trace_id": "a1b2c3d4e5f67890abcd1234",
  "token_in": "WETH",
  "token_out": "USDC",
  "amount": "1000000000000000000"
}
```

在 Grafana / Kibana 中搜索 `trace_id` 即可看到这个请求经过的所有模块的日志。

chain / gas / mev / arb / ws 这些模块直接用 slog 的键值对风格输出关键事件，例如断路器状态变化、三明治告警、发现套利机会：

```go
slog.Warn("检测到疑似三明治攻击",
    "pool", alert.Pool, "block", alert.BlockNumber, "attacker", alert.Attacker,
    "front_tx", alert.FrontTx, "back_tx", alert.BackTx, "victims", len(victims))
```

**代码位置**：`backend/logging/logging.go`

---

### 30. Prometheus 风格可观测性指标

**日志告诉你"发生了什么"，指标告诉你"系统状态如何"。**

本实现使用纯标准库实现 Prometheus 文本格式的指标暴露，无需引入 `prometheus/client_golang`。

**三种指标类型**：

```go
// Counter — 只增不减的累计值（适合：请求总数、错误总数）
type Counter struct {
    value atomic.Int64  // 使用 atomic 保证并发安全，零锁开销
}
counter.Inc()     // +1
counter.Add(10)   // +10

// Gauge — 可增可减的瞬时值（适合：当前连接数、队列深度）
type Gauge struct {
    value atomic.Int64
}
gauge.Set(42)     // 设置为 42
gauge.Inc()       // +1
gauge.Dec()       // -1

// Histogram — 值的分布统计（适合：请求延迟）
type Histogram struct {
    buckets []float64      // 桶边界：[1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000]
    counts  []atomic.Int64 // 每个桶的计数
    values  []float64      // 保留最近 10000 个值，用于精确百分位计算
}
histogram.Observe(42.5)                          // 记录一个值
histogram.ObserveDuration(startTime)             // 记录持续时间
histogram.Percentile(0.99)                       // 获取 P99
```

**Histogram 百分位计算原理**：

```
P99 = 99% 的请求在 X ms 内完成

计算方法：
1. 保留最近 N 个观测值（默认 10000）
2. 排序
3. 取第 ceil(0.99 × N) 个值

例如 1000 个观测值：
  排序后 → [0.5, 0.8, 1.2, ..., 42.5, 50.1, 120.3]
  P99 = 第 990 个值 = 50.1ms
  含义："99% 的请求在 50.1ms 内完成"
```

**业务指标**（预注册到全局 Registry，各模块直接使用）：

| 指标 | 类型 | 含义 |
|------|------|------|
| `router_requests_total` | Counter | 路由请求总数 |
| `router_degraded_total` | Counter | 降级到直连路径的次数 |
| `router_latency_ms` | Histogram | 路由搜索延迟 |
| `workerpool_queue_used` | Gauge | 路径评估池的队列占用 |
| `indexer_blocks_processed` | Counter | 已处理的区块数 |
| `indexer_reorgs_total` | Counter | 检测到的 Reorg 次数 |
| `circuitbreaker_open` | Gauge | RPC 断路器是否处于熔断状态 |
| `quote_onchain_checks_total` | Counter | 链上报价抽检次数 |
| `quote_onchain_mismatch_total` | Counter | 抽检中链下与链上不一致的次数 |
| `quote_onchain_deviation_bps` | Histogram | 抽检偏差（基点） |
| `oracle_twap_checks_total` | Counter | TWAP 校验次数 |
| `oracle_twap_deviation_total` | Counter | 现价偏离 TWAP 超阈值的次数 |
| `oracle_twap_unavailable_total` | Counter | TWAP 不可用的次数 |
| `mev_sandwich_detected_total` | Counter | 检测到的三明治攻击数 |
| `mev_swaps_rejected_total` | Counter | 被交易前保护拒绝的请求数 |
| `swap_signature_rejected_total` | Counter | EIP-712 验签失败次数 |
| `arb_opportunities_found_total` | Counter | 累计发现的套利机会 |
| `arb_opportunities_active` | Gauge | 当前有效的套利机会数 |
| `stream_clients` | Gauge | SSE 在线客户端数 |
| `stream_dropped_clients_total` | Counter | 因消费过慢被断开的客户端数 |

**`/metrics` 端点输出**（Prometheus 文本格式）：

```
# TYPE router_requests_total counter
router_requests_total 1200

# TYPE router_latency_ms histogram
router_latency_ms_bucket{le="5"} 100
router_latency_ms_bucket{le="10"} 250
router_latency_ms_bucket{le="25"} 800
router_latency_ms_bucket{le="50"} 1100
router_latency_ms_bucket{le="100"} 1180
router_latency_ms_bucket{le="+Inf"} 1200
router_latency_ms_count 1200
router_latency_ms_avg 18.50
router_latency_ms_p50 12.00
router_latency_ms_p95 45.00
router_latency_ms_p99 95.00

# TYPE quote_onchain_mismatch_total counter
quote_onchain_mismatch_total 0
```

Prometheus 定期 scrape 此端点，Grafana 展示面板。集成测试的最后一步也读这个端点：断言 `quote_onchain_mismatch_total` 为 0、`mev_sandwich_detected_total` 为 1，套利和 TWAP 偏离计数大于 0。

**代码位置**：`backend/metrics/metrics.go`

---

### 31. API 限流与背压控制

**双层保护**：

```
请求进入
    │
    ▼
┌───────────────────────────────────┐
│ Layer 1: 每 IP 限流 (Token Bucket)│
│                                   │
│ 每个 IP 有一个令牌桶               │
│ 容量 = 100                        │
│ 补充速率 = 每秒 1 个               │
│ 每次请求消耗 1 个令牌              │
│                                   │
│ 桶空？→ 429 Too Many Requests     │
│        + Retry-After: 1 头        │
└───────────┬───────────────────────┘
            │ 通过
            ▼
┌───────────────────────────────────┐
│ Layer 2: 全局并发限制 (Semaphore)  │
│                                   │
│ 有界 Channel (capacity=500)       │
│ 非阻塞获取                        │
│                                   │
│ 满了？→ 503 Service Unavailable   │
│        + Retry-After: 1 头        │
└───────────┬───────────────────────┘
            │ 通过
            ▼
      正常处理请求
```

完整的中间件链是 `CORS → Logging → RateLimit → ConcurrencyLimit → Handler`。

**按 IP 而不是按连接限流**：`r.RemoteAddr` 是 `IP:端口`，每个连接的端口都不同，直接拿它当 key 等于没限流。中间件先用 `net.SplitHostPort` 去掉端口（`middleware_test.go` 的 `TestRateLimitIsPerIPNotPerConnection` 覆盖了这一点）。

**Token Bucket 算法详解**：

```go
// 每个 IP 有独立的令牌桶
type tokenBucket struct {
    tokens   int        // 当前令牌数
    lastTime time.Time  // 上次补充时间
}

func (rl *RateLimiter) Allow(ip string) bool {
    bucket := rl.buckets[ip]

    // 按时间补充令牌
    elapsed := time.Since(bucket.lastTime)
    refilled := int(elapsed / rl.refillRate)
    bucket.tokens = min(bucket.tokens + refilled, rl.maxTokens)

    if bucket.tokens <= 0 {
        return false  // 桶空，拒绝
    }
    bucket.tokens--
    return true
}

// 过期桶清理（防止内存泄漏）
// 每 5 分钟清理 10 分钟未活跃的桶
go rl.cleanup()
```

**为什么 503 而不是排队？**

排队意味着请求在内存中等待，极端情况下等待队列可以无限增长导致 OOM。直接 503 + `Retry-After` 让客户端自行重试，是更安全的策略。

**代码位置**：`backend/api/middleware.go`

---

### 32. 优雅关闭与生命周期管理

**启动顺序（依赖图）**：

```
1. PoolStateManager（无依赖）
        ↓
2. 连接链节点 chain.Dial（连不上或未配置则用模拟源）
        ↓
3. Oracle / Gas Estimator / K 线 / MEV Guard / 套利扫描 / SSE Hub
   （订阅 Swap 事件流的模块，回调必须在索引器启动前接好）
        ↓
4. Indexer（注册订阅者后启动轮询）
        ↓
5. Cache
        ↓
6. Router（依赖 PoolStateManager）
        ↓
7. EIP-712 Verifier + API Server（依赖所有上游）
```

`chain.Dial` 在连接后做一次自检：按最新区块头本地复算区块哈希，再用这个哈希向节点查回区块。个别节点返回的区块头字段不全时，本地复算的哈希会和节点自己的对不上，父哈希校验会一直误报 Reorg，所以启动时先确认一次。

**关闭顺序**：

```go
// main.go — 优雅关闭
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
<-sigCh  // 阻塞等待退出信号

// 1. 取消根 context：停止索引器，并让 Hub 断开所有 SSE 连接
//    否则长连接会拖住 HTTP 服务器的关闭
cancel()

// 2. 关闭 API Server：停止接受新请求，等待处理中的请求完成，最多 10 秒
shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
defer shutdownCancel()
server.Shutdown(shutdownCtx)
```

**关键原则**：

1. **先断开长连接**：SSE 连接永远不会自己结束，`server.Shutdown` 会一直等它们，所以先让 Hub 主动断开
2. **等待处理中的请求完成**：`server.Shutdown(ctx)` 会等待活跃连接处理完毕
3. **超时保护**：10 秒后强制退出，防止某些连接永不关闭
4. **所有后台协程都监听同一个根 context**：索引器轮询、Hub 通过 `ctx.Done()` 退出

**代码位置**：`backend/main.go`

---

## 测试体系

测试分三层：合约测试验证链上逻辑本身；后端单元测试验证各模块；端到端集成测试把两者接在一个真实节点上，验证“链下算的和链上跑的是同一个结果”。

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
| `Gas.test.js` | 6 | Gas 基准（见 [Benchmark](#benchmark-性能对比)） |
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

### 后端单元测试（15 个包，80 个测试函数）

```bash
cd backend && go test -p 2 ./...
```

| 包 | 测试数 | 重点 |
|----|-------|------|
| `chain` | 12 | 在 go-ethereum 模拟后端上：区块头、事件解码、伪造 `PoolCreated` 被忽略、畸形日志跳过、交易状态、广播、EIP-1559 费用、断路器熔断、索引器跟链并处理 Reorg、路径与调用数据编码 |
| `chain/contractabi` | 2 | 内嵌 ABI 与编译产物一致 |
| `mev` | 11 | 三明治命中 / 不误报 / 经路由合约下单 / 按 logIndex 排序、Reorg 丢弃告警、价格冲击、滑点与 deadline 检查 |
| `api` | 10 | 报价、交易前保护、验签、待签名交易构造、K 线 / 成交 / 套利 / SSE 端点、按 IP 限流 |
| `math` | 6 | 已知值、往返一致性、`ComputeSwapStep` |
| `router` | 6 | 逐 tick 模拟、tick 查找与位图参照实现一致、精确输出与精确输入互逆、价格限制 |
| `kline` | 5 | 多周期聚合、Reorg 重算、内存上限、并发访问 |
| `indexer` | 4 | 事件回放、Reorg 回滚并重新索引、回滚分叉上创建的池子 |
| `arb` | 4 | 最优借入量、无机会、扣除 gas、机会生命周期 |
| `circuitbreaker` | 4 | 状态转换、恢复、回调 |
| `workerpool` | 4 | 功能测试（另有 3 个 Benchmark） |
| `eip712` | 3 | 摘要与标准类型化数据实现一致、验签与防重放、非法字段 |
| `gas` | 3 | 链上费用、缓存、退回静态估算 |
| `oracle` | 3 | TWAP 校验、三方交叉验证、池内价格喂价 |
| `ws` | 3 | 推送与类型过滤、慢客户端被断开且不阻塞、关闭时断开客户端 |

### 端到端集成测试

```bash
cd backend && go test -p 2 -tags integration -count=1 -timeout 15m ./integration
```

带 `integration` 构建标签，默认的 `go test ./...` 不会跑到它。测试**自己拉起一个 Hardhat 节点**（空闲端口），用编译产物部署 Factory / Manager / Quoter / FlashArbitrage 和三种测试代币，建 4 个池子、铺 11 个头寸，再按 `main.go` 的装配方式把后端各模块接到这个节点上，起一个 HTTP 服务。运行前需要 `contracts/` 下已安装依赖（`npm install`）。

`TestEndToEnd` 的 11 个场景按顺序在同一条链上执行：

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
| 全部场景之后仍然一致 | 再比一轮状态和 60 条报价；`/metrics` 里链上抽检不一致次数为 0 |

一次实际运行的输出节选：

```
[初始状态] 对比 60 条报价：60 条两边数值逐位一致（其中 11 条穿越了已初始化的 tick，单条最多 4 个）
多跳 A→B→C：投入 5000000000000000000，到手 4996415352936063385，与链下报价逐位一致；gas 实际 199260 / 估算上限 267133
TWAP：tick1=-18 持续 100s，tick2=9 持续 50s，手算平均 tick=-9，链上读数 -9
操纵检测：现价 0.811815，150 秒 TWAP 1.000600，偏离 18.87%，交易被拒绝
套利：借入 34868164334402465845147，发起人获利 1060294620131858209393（与链下预测逐位一致），gas 367570；执行前价差 5.910%，执行后 0.648%
[全部场景之后] 对比 60 条报价：60 条两边数值逐位一致（其中 13 条穿越了已初始化的 tick，单条最多 5 个）
```

**节点进程的回收**有三层保险（`harness_test.go`）：正常结束时 `TestMain` 关掉节点并等它退出；节点跑在独立进程组里，退出时整组 SIGKILL；测试进程被强杀时，包装脚本读着的管道 EOF，随即杀掉节点，不留孤儿进程。

---

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

## 快速开始

### 环境要求

- Node.js >= 18
- Go >= 1.24

### 编译与测试

```bash
# 克隆项目
git clone <repo-url>
cd UniGo-DEX

# === 智能合约 ===
cd contracts
npm install
npx hardhat compile             # 编译合约（项目自身 27 个 .sol 文件）
npx hardhat test                # 109 个合约测试
REPORT_GAS=1 npx hardhat test   # 同时打印 Gas 基准和 gas-reporter 汇总

# === Go 后端 ===
cd ../backend
go build ./...                  # 编译全部 19 个包
go test -p 2 ./...              # 单元测试（15 个包，80 个测试函数）

# 端到端集成测试（自起 Hardhat 节点，需要 contracts/ 已 npm install）
go test -p 2 -tags integration -count=1 -timeout 15m ./integration

# Goroutine 池 benchmark
go test ./workerpool/ -run '^$' -bench=. -benchmem -count=3
```

也可以用 Makefile：

| 命令 | 作用 |
|------|------|
| `make install` | 安装合约和后端依赖 |
| `make compile` | 编译合约 |
| `make test` / `make test-go` / `make test-all` | 合约测试 / Go 单元测试 / 两者 |
| `make test-integration` | 端到端集成测试 |
| `make gas` | 合约测试并打印 Gas 报告 |
| `make abi` | 编译合约并重新生成后端内嵌的 ABI |
| `make node` / `make deploy` | 启动本地 Hardhat 节点 / 部署到本地节点 |
| `make build-backend` / `make run-backend` | 编译 / 运行后端 |

### 本地部署并启动后端

```bash
# 终端 1：启动本地 Hardhat 节点（http://127.0.0.1:8545，chainId 31337）
cd contracts && npx hardhat node

# 终端 2：部署 Factory / Manager / Quoter、两个测试代币（WETH、USDC），
#         创建并初始化 WETH/USDC 0.3% 池
cd contracts && npx hardhat run scripts/deploy.js --network localhost
```

部署脚本会打印各合约地址。在全新启动的节点上地址是固定的（以脚本输出为准）：

```
Factory:   0x5FbDB2315678afecb367f032d93F642f64180aa3
Manager:   0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512
Quoter:    0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0
WETH:      0xCf7Ed3AccA5a467e9e704C703E8D87F634fB0Fc9
USDC:      0xDc64a140Aa3E981100a9becA4E685f962f0cF6C9
Pool:      0x6c7b79509c47DA244DEf7c87a02816B6c39DB61d
```

```bash
# 终端 3：把地址通过环境变量交给后端并启动
cd backend
RPC_URL=http://127.0.0.1:8545 \
FACTORY_ADDRESS=0x5FbDB2315678afecb367f032d93F642f64180aa3 \
MANAGER_ADDRESS=0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512 \
QUOTER_ADDRESS=0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0 \
WETH_ADDRESS=0xCf7Ed3AccA5a467e9e704C703E8D87F634fB0Fc9 \
go run main.go                  # API 运行在 :8080

# 验证：索引器已从链上事件重建出池子
curl localhost:8080/health
curl localhost:8080/api/v1/pools
# [{"address":"0x6c7b…b61d","token0":"0xcf7e…","token1":"0xdc64…","fee":3000,"tick_spacing":60,"price":2000,"tick":76012,"liquidity":"0"}]
curl localhost:8080/api/v1/status
curl "localhost:8080/api/v1/twap?pool=0x6c7b79509c47da244def7c87a02816b6c39db61d"
curl -N localhost:8080/api/v1/stream       # 订阅实时推送
```

> 使用说明：部署脚本只创建并初始化池子，不添加流动性。报价和交易构造需要池子里有流动性，可通过 `Manager.mint` 添加（调用前先 `approve` Manager）。套利扫描需要另行部署 `FlashArbitrage`（构造参数为 Factory 地址）并配置 `FLASH_ARB_ADDRESS`。不配置任何环境变量直接 `go run main.go` 时，索引器使用模拟源，服务同样可以启动。

### 构建子图

```bash
cd subgraph
npm install
npm run abis       # 从 contracts/artifacts 提取 Factory 和 Pool 的 ABI（需先编译合约）
npm run codegen    # 由 schema 和 ABI 生成 AssemblyScript 类型
npm run build      # 编译成 WASM
```

构建产物部署到自行运行的 graph-node 上即可查询（见第 13 节）。

### 环境变量

**后端**（`backend/main.go`）：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `RPC_URL` | 空 | 以太坊节点 RPC 地址。未配置或连不上时索引器使用模拟源 |
| `FACTORY_ADDRESS` | 空 | Factory 合约地址，用于发现池子（只采信它发出的 `PoolCreated`）。未配置时索引器使用模拟源 |
| `START_BLOCK` | `0` | 开始索引的区块号 |
| `CONFIRMATION_BLOCKS` | `12` | 交易最终确认所需的区块数；Reorg 回滚窗口为它的 2 倍 |
| `CHAIN_ID` | `31337` | 未连接节点时 EIP-712 域使用的链 ID；连上节点后以节点返回的为准 |
| `MANAGER_ADDRESS` | 空 | UniGoManager 地址：待签名交易的目标，也是 EIP-712 的 `verifyingContract`。未配置时 `/api/v1/swap/build` 返回 503 |
| `QUOTER_ADDRESS` | 空 | UniGoQuoter 地址。配置后对链下报价做链上一致性抽检 |
| `FLASH_ARB_ADDRESS` | 空 | FlashArbitrage 地址，填充套利机会响应里的 `to` |
| `WETH_ADDRESS` | 空 | WETH 地址，用于把 gas 成本折算成借入 token |
| `TWAP_WINDOW` | `600` | TWAP 窗口（秒） |
| `PORT` | `8080` | HTTP 监听端口 |

**合约测试**：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `REPORT_GAS` | 未设置 | 设置后打印 Gas 基准表并启用 hardhat-gas-reporter |
| `INVARIANT_SEED` | `42` | 不变量随机测试的种子 |
| `INVARIANT_STEPS` | `300` | 不变量随机测试的步数 |

### API 端点

以 `backend/api/server.go` 注册的路由为准。数量一律用十进制字符串传递，避免 JSON 精度丢失。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/quote?token_in=&token_out=&amount_in=[&slippage=]` | 报价：最优路径、产出、价格冲击、推荐滑点与 `min_amount_out`、Gas 成本、价格安全警告 |
| POST | `/api/v1/swap/build` | 把路由编码成 Manager 的调用数据，返回待签名交易。body：`token_in` `token_out` `recipient`，`amount_in` 与 `amount_out` 二选一，可选 `slippage` `deadline` `from` |
| POST | `/api/v1/swap` | 提交交易：EIP-712 验签 + 价格安全检查 + MEV 检查；带 `raw_tx` 时广播并返回交易哈希 |
| GET | `/api/v1/nonce/{address}` | EIP-712 签名用的 nonce |
| GET | `/api/v1/tx/{hash}` | 交易状态：`pending` / `pending_confirmation` / `confirmed` / `reverted` |
| GET | `/api/v1/pools` | 所有池子的价格、tick、活跃流动性 |
| GET | `/api/v1/klines?pool=[&interval=1m][&limit=100]` | K 线，周期 `1m` `5m` `15m` `1h` `4h` `1d`，`limit` 最大 1000 |
| GET | `/api/v1/trades?pool=[&limit=50]` | 最近成交，最新的在前 |
| GET | `/api/v1/twap?pool=` | 池子的 TWAP、现价与偏离（`ok` / `deviated` / `unavailable`） |
| GET | `/api/v1/mev/alerts[?limit=100]` | 三明治攻击告警，最新的在前 |
| GET | `/api/v1/arbitrage/opportunities` | 当前套利机会与调用数据，净利润高的在前 |
| GET | `/api/v1/stream[?types=trade,kline,mev_alert,arbitrage]` | SSE 实时推送 |
| GET | `/api/v1/status` | 系统状态（索引器进度、Reorg 次数、路由统计） |
| GET | `/metrics` | Prometheus 文本格式指标（含 P50 / P95 / P99） |
| GET | `/health` | 健康检查 |

`/api/v1/tx/{hash}` 和 `/api/v1/twap` 需要已连接节点，否则返回 503。

---

## 项目结构

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
│   │   └── test/                           # 仅测试使用的合约（8 个）
│   │       ├── TestERC20.sol               # 标准测试代币
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
│   │   └── deploy.js                       # 部署脚本
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
│   │   ├── contracts.go                    # slot0 / TWAP / Quoter 读调用 + 调用数据编码
│   │   ├── events.go                       # 事件解码（签名取自内嵌 ABI）
│   │   ├── contractabi/                    # 内嵌 ABI
│   │   │   ├── contractabi.go              # go:embed + 解析
│   │   │   ├── gen.go                      # go generate：从 Hardhat 编译产物提取 ABI
│   │   │   └── *.json                      # Pool / Factory / Manager / Quoter / FlashArbitrage
│   │   └── *_test.go                       # 在 go-ethereum 模拟后端上测试
│   │
│   ├── indexer/                            # 事件索引器
│   │   ├── indexer.go                      # ChainSource 接口 + 轮询 + Reorg 检测 + 撤销日志回滚
│   │   ├── pool_state.go                   # PoolState + PoolStateManager + Snapshot
│   │   ├── events.go                       # 事件结构体
│   │   └── sim_source.go                   # 未配置节点时的模拟数据源
│   │
│   ├── router/                             # 智能路由引擎
│   │   ├── quote.go                        # 逐 tick 精确的链下 swap 模拟（SimulateSwap）
│   │   └── router.go                       # 路径枚举 + 并发评估 + 200ms 超时 + 二级降级
│   │
│   ├── oracle/                             # 价格安全
│   │   └── oracle.go                       # 现价 / TWAP / Chainlink 三方交叉校验 + 保护模式
│   │
│   ├── eip712/                             # 签名验证
│   │   └── eip712.go                       # EIP-712 摘要 + ecrecover + 三层防重放
│   │
│   ├── gas/                                # Gas 成本评估
│   │   └── gas.go                          # EIP-1559：baseFee + priorityFee，取不到时退回静态值
│   │
│   ├── kline/                              # 行情
│   │   └── kline.go                        # 六周期 K 线 + 逐笔成交 + Reorg 重算
│   │
│   ├── mev/                                # MEV 防御
│   │   └── mev.go                          # 三明治检测 + 价格冲击 / 滑点 / deadline 检查
│   │
│   ├── arb/                                # 套利
│   │   └── arb.go                          # 跨池套利扫描 + 三分搜索最优借入量
│   │
│   ├── ws/                                 # 实时推送
│   │   └── ws.go                           # Hub 发布-订阅 + SSE + 慢客户端断开
│   │
│   ├── cache/                              # 缓存层
│   │   └── cache.go                        # Singleflight 请求合并 + TTL + Warmup 预热
│   │
│   ├── circuitbreaker/                     # 断路器
│   │   └── circuitbreaker.go               # 三态状态机（Closed→Open→HalfOpen）
│   │
│   ├── workerpool/                         # Goroutine 池
│   │   ├── pool.go                         # 固定 Worker + 有界 Channel + 泛型结果
│   │   └── pool_test.go                    # 功能测试 + Benchmark 对比
│   │
│   ├── logging/                            # 结构化日志
│   │   └── logging.go                      # slog + JSON/Text + TraceID
│   │
│   ├── metrics/                            # 可观测性指标
│   │   └── metrics.go                      # Counter/Gauge/Histogram + P99 + /metrics 端点
│   │
│   ├── api/                                # REST API
│   │   ├── server.go                       # 路由注册 + 报价 / 提交 / 状态 / K 线 / 告警等端点
│   │   ├── trade.go                        # 价格安全检查、待签名交易构造、TWAP 与套利查询
│   │   ├── middleware.go                   # Token Bucket 限流 + 信号量并发控制 + 日志 + CORS
│   │   └── types.go                        # 请求/响应类型定义
│   │
│   ├── integration/                        # 端到端集成测试（integration 构建标签）
│   │   ├── harness_test.go                 # 拉起 Hardhat 节点、部署合约、发交易
│   │   └── e2e_test.go                     # 11 个全链路场景
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
├── README.md                               # 本文件
├── Makefile                                # make compile / test / test-integration / gas / abi / deploy ...
└── .gitignore
```

代码规模（`wc -l`）：合约 3157 行 Solidity（不含测试合约），合约测试 2768 行；后端 7533 行 Go，后端测试 4910 行（其中集成测试 1452 行）。

---

## 参考资料

- [Uniswap V3 白皮书](https://uniswap.org/whitepaper-v3.pdf)
- [Uniswap V3 开发教程 (Jeiwan)](https://uniswapv3book.com/)
- [Uniswap V3 Core 源码](https://github.com/Uniswap/v3-core)
- [EIP-712: Typed structured data hashing and signing](https://eips.ethereum.org/EIPS/eip-712)
- [EIP-1559: Fee market change](https://eips.ethereum.org/EIPS/eip-1559)
- [go-ethereum 文档](https://geth.ethereum.org/docs)
- [The Graph 文档](https://thegraph.com/docs/)
- [Chainlink Price Feeds](https://docs.chain.link/data-feeds)
- [Go slog 官方文档](https://pkg.go.dev/log/slog)
- [Circuit Breaker Pattern (Martin Fowler)](https://martinfowler.com/bliki/CircuitBreaker.html)
- [Token Bucket Algorithm](https://en.wikipedia.org/wiki/Token_bucket)

## 许可证

GPL-2.0-or-later
