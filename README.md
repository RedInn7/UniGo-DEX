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
  5. 每穿越一个 tick，Gas 消耗增加约 ~25000
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
  │      ├──→ Pool 回调 Manager: IUniGoMintCallback.uniGoMintCallback(amount0, amount1, data)
  │      │      │
  │      │      └──→ Manager 执行 token0.transferFrom(user, pool, amount0)
  │      │           Manager 执行 token1.transferFrom(user, pool, amount1)
  │      │
  │      └──→ Pool 验证: token0.balanceOf(pool) >= balance0Before + amount0Owed
  │           如果验证失败 → revert (保证原子性)
```

**与 approve + transferFrom 的对比**：

| 维度 | approve 模式 | 回调模式 |
|------|-------------|---------|
| 交易次数 | 2 笔（approve + transferFrom） | 1 笔 |
| Gas 开销 | 多一次 approve 的 SSTORE（~20000 gas） | 节省 |
| 安全授权 | 需要无限授权（风险） | 单次精确授权 |
| 灵活性 | 资金只能来自调用者 | 可从任意来源筹集资金 |
| 闪电贷支持 | 不支持 | 天然支持（先用后还） |

**安全保证**：Pool 在回调前记录余额快照，回调后验证余额增量 >= 预期。即使回调中执行了任意代码，只要最终余额不足，整个交易 revert。

**代码位置**：`contracts/contracts/UniGoPool.sol` (mint/swap), `contracts/contracts/interfaces/IUniGoPoolCallbacks.sol`

---

### 5. CREATE2 确定性部署

Factory 使用 `CREATE2` 而非 `CREATE` 部署 Pool 合约：

```solidity
// UniGoFactory.sol
pool = new UniGoPool{salt: keccak256(abi.encode(token0, token1, fee))}(
    token0, token1, fee, tickSpacing
);
```

**地址计算公式**：

```
poolAddress = keccak256(
    0xff,                                    // 固定前缀
    factoryAddress,                          // 工厂合约地址
    keccak256(abi.encode(token0, token1, fee)), // salt
    keccak256(UniGoPool.bytecode)             // 合约字节码哈希
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
function quote(address pool, bool zeroForOne, int256 amountSpecified)
    external returns (uint256 amountOut)
{
    try IUniGoPool(pool).swap(
        address(this),          // recipient = Quoter 自己
        zeroForOne,
        amountSpecified,
        sqrtPriceLimitX96,
        abi.encode(pool)        // callback data
    ) {} catch (bytes memory reason) {
        // Pool.swap() 会调用 uniGoSwapCallback
        // 而我们的 callback 会故意 revert
        // 所以一定会进入 catch
        return _parseRevertReason(reason);
    }
}

// Quoter 的 swap 回调 — 故意 revert
function uniGoSwapCallback(int256 amount0Delta, int256 amount1Delta, bytes calldata)
    external pure
{
    // Pool 完整执行了 swap 计算（跨 tick 循环、流动性更新...）
    // 但因为我们在这里 revert，所有状态变更被回滚
    // 同时将计算结果编码在 revert 数据中带出来
    assembly {
        let ptr := mload(0x40)
        mstore(ptr, amount0Delta)
        mstore(add(ptr, 0x20), amount1Delta)
        revert(ptr, 64)
    }
}
```

**为什么不直接用 `staticcall`？**

`staticcall` 禁止任何状态修改，但 `swap()` 内部需要临时修改状态（更新 tick、价格、流动性）来完成计算。`revert-catch` 模式允许完整执行但最终回滚，是唯一能获得精确报价的方法。

**代码位置**：`contracts/contracts/UniGoQuoter.sol`

---

## Go 后端 — 核心业务

### 7. 链下数学库（链上链下一致性）

**核心问题**：链下路由引擎需要模拟 Swap，预估输出数量和 Gas。如果链下计算与链上不一致（哪怕差 1 wei），就会导致交易失败或滑点超限。

**Go 的 `math/big` 天然支持任意精度整数**，与 Solidity 的 `uint256` 对应：

```go
// tickmath.go — 与 Solidity 完全一致的查表法
//
// Solidity:  if (absTick & 0x1 != 0) ratio = (ratio * 0xfffcb933...) >> 128
// Go:        if absTickBig.Bit(0) == 1 { ratio.Mul(ratio, magicNumbers[0]); ratio.Rsh(ratio, 128) }
//
// 使用相同的 20 个预计算魔数常量（Q128.128 格式）

var magicNumbers = [20]*big.Int{
    hexToBigInt("fffcb933bd6fad37aa2d162d1a594001"),  // 2^128 / √1.0001
    hexToBigInt("fff97272373d413259a46990580e213a"),  // 2^128 / √1.0001²
    hexToBigInt("fff2e50f5f656932ef12357cf3c7fdcc"),  // 2^128 / √1.0001⁴
    // ... 共 20 个，覆盖 2^0 到 2^19 的 tick 增量
}

func GetSqrtRatioAtTick(tick int32) (*big.Int, error) {
    if tick < MinTick || tick > MaxTick {
        return nil, fmt.Errorf("tick %d 超出范围 [%d, %d]", tick, MinTick, MaxTick)
    }

    absTick := int32(math.Abs(float64(tick)))
    absTickBig := big.NewInt(int64(absTick))

    // 从 2^128 开始（Q128.128 格式的 1.0）
    ratio := new(big.Int).Set(magicNumbers[0]) // 初始化

    // 逐位检查 tick 值，乘以对应的魔数
    // 原理：tick = Σ(bit_i × 2^i)，所以 1.0001^tick = Π(1.0001^(2^i))^bit_i
    for i := 0; i < 20; i++ {
        if absTickBig.Bit(i) == 1 {
            ratio.Mul(ratio, magicNumbers[i])
            ratio.Rsh(ratio, 128)
        }
    }

    // 如果 tick 为正，取倒数
    if tick > 0 {
        maxUint256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
        ratio.Div(maxUint256, ratio)
    }

    // 从 Q128.128 转换为 Q64.96
    ratio.Rsh(ratio, 32)
    return ratio, nil
}
```

**一致性验证**（通过测试保证）：

```go
// tickmath_test.go
func TestKnownValues(t *testing.T) {
    // tick=0 → sqrtPriceX96 = 2^96 = 79228162514264337593543950336
    result, _ := GetSqrtRatioAtTick(0)
    expected := new(big.Int).Lsh(big.NewInt(1), 96)
    assert(result.Cmp(expected) == 0) // bit-exact 匹配

    // 往返测试：tick → sqrtPrice → tick，确保无损
    for _, tick := range []int32{-100, 0, 100, 887272, -887272} {
        sqrtPrice, _ := GetSqrtRatioAtTick(tick)
        recoveredTick := GetTickAtSqrtRatio(sqrtPrice)
        assert(recoveredTick == tick)
    }
}
```

**代码位置**：`backend/math/tickmath.go`, `backend/math/sqrtpricemath.go`, `backend/math/tickmath_test.go`

---

### 8. Smart Order Router（二级降级）

**问题**：极端行情下（如 ETH 瞬时下跌 30%），大量 Swap 请求并发触发复杂路径搜索，CPU 飙升。如果所有请求都坚持寻找最优路径，系统会雪崩。

**核心设计：Fail-soft 而非 Fail-fast**

不同于传统的"超时就报错"策略，UniGo 的路由引擎在超时后**自动降级到更简单的路径**，保证可用性：

```
请求进入
    │
    ▼
┌───────────────────────────────────┐
│ 背压控制：Worker Pool 信号量       │
│ chan struct{} (capacity=200)      │
│                                   │
│ 获取失败？→ 直接降级，不排队       │
└───────────┬───────────────────────┘
            │ 获取成功
            ▼
┌───────────────────────────────────┐
│ Level 1：BFS 多跳搜索              │
│ context.WithTimeout(200ms)       │
│ 最大跳数: 3                       │
│ sync.Pool 复用搜索状态             │
│                                   │
│ 超时 / 搜索失败？→ 进入 Level 2   │
└───────────┬───────────────────────┘
            │ 搜索成功
            ▼
┌───────────────────────────────────┐
│ Gas 预估检查                       │
│ ticksCrossed > 50？→ 高 Gas 警告  │
│ "建议拆分为多笔较小的交易"          │
└───────────────────────────────────┘

Level 2 降级路径：
┌───────────────────────────────────┐
│ 只查找 tokenIn → tokenOut 的直连池│
│ 不做多跳搜索                      │
│ 牺牲最优性，保证可用性              │
│ 标记 IsDegraded = true            │
└───────────────────────────────────┘
```

**背压机制的三层设计**：

```go
// 第 1 层：Worker Pool 信号量（router.go）
// 限制并发路由搜索数，防止 CPU 过载
workerSem: make(chan struct{}, 200)

select {
case r.workerSem <- struct{}{}:  // 非阻塞获取
    defer func() { <-r.workerSem }()
default:
    // 满了 → 直接降级，不排队等待
    return r.degradedRoute(tokenIn, tokenOut, amountIn, "worker_pool_full")
}

// 第 2 层：sync.Pool 对象复用（router.go）
// BFS 搜索过程中的 visited map 和 queue 通过 sync.Pool 复用
// 避免每次搜索都分配大量临时对象，降低 GC 压力
searchStatePool: sync.Pool{
    New: func() interface{} {
        return &searchState{
            visited: make(map[string]bool),
            queue:   make([]searchNode, 0, 32),
        }
    },
}

// 使用后重置并归还
state := r.searchStatePool.Get().(*searchState)
defer func() {
    for k := range state.visited { delete(state.visited, k) }
    state.queue = state.queue[:0]
    r.searchStatePool.Put(state)
}()

// 第 3 层：HTTP 并发限制（middleware.go）
// 超出限制直接 503，不让请求进入路由引擎
```

**代码位置**：`backend/router/router.go`

---

### 9. 事件索引器与 Reorg 回滚

**问题**：区块链的最终性不是即时的。以太坊上，一个区块被打包后仍然可能被撤销（Reorg），导致基于该区块的所有状态更新失效。

**Reorg 检测：父块哈希校验**

索引器维护最近 N 个区块的哈希链。每收到新区块时，验证其 `parentHash` 是否与已知的前一个区块 hash 一致：

```
正常情况：
  Block 100: hash=0xAAA
  Block 101: hash=0xBBB, parentHash=0xAAA  ✓ 匹配
  Block 102: hash=0xCCC, parentHash=0xBBB  ✓ 匹配

Reorg 发生：
  Block 100: hash=0xAAA
  Block 101: hash=0xBBB, parentHash=0xAAA  ✓ 匹配
  Block 102: hash=0xCCC, parentHash=0xBBB  ✓ 已处理
  Block 102: hash=0xDDD, parentHash=0xBBB  ← 新区块！parentHash 匹配 101 但 hash 不同
       ↓
  检测到 Reorg → 回滚区块 102 的所有状态变更
```

**延迟确认状态机**：

```
事件首次出现
    │
    ▼
┌──────────┐     等待 K 个区块    ┌──────────────────┐     K 个确认    ┌───────────┐
│ Pending  │ ──────────────────→ │ PendingConfirmed │ ─────────────→ │ Confirmed │
│ (待确认)  │                    │  (软确认)         │                │ (最终确认)  │
└──────────┘                    └────────┬─────────┘                └───────────┘
                                         │ Reorg 检测到
                                         ▼
                                    ┌────────────┐
                                    │ RolledBack │
                                    │ (已回滚)    │
                                    └────────────┘
```

K = 12（以太坊约 12 个区块确认，约 2.5 分钟）。

**回滚实现**：

```go
// indexer.go — Reorg 处理
func (idx *Indexer) handleReorg(forkBlock uint64) {
    // 1. 回滚 PoolStateManager 中受影响的池子状态
    rolledBack := idx.state.RollbackToBlock(forkBlock - 1)

    // 2. 清理分叉区块的历史记录
    for num := range idx.blockHistory {
        if num >= forkBlock {
            delete(idx.blockHistory, num)
        }
    }
    // 3. 从分叉点重新索引（由下一轮 pollBlocks 自动触发）
}

// pool_state.go — 状态回滚
func (m *PoolStateManager) RollbackToBlock(targetBlock uint64) int {
    rolled := 0
    for _, pool := range m.pools {
        if pool.LastBlockNumber > targetBlock {
            pool.LastBlockNumber = targetBlock  // 标记需要重新同步
            rolled++
        }
    }
    return rolled
}
```

**代码位置**：`backend/indexer/indexer.go`, `backend/indexer/pool_state.go`

---

### 10. 缓存 Singleflight 防击穿

**问题**：缓存失效瞬间（Key 过期或 Redis 重启），大量并发请求同时穿透缓存直达后端数据源（The Graph / 以太坊节点），造成瞬时过载。

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

**代码位置**：`backend/cache/cache.go`

---

### 11. Chainlink 预言机验证与保护模式

**核心原则：不盲信任何单一价格源。**

池内价格（AMM sqrtPriceX96）是实时的但可被操纵（闪电贷攻击）；Chainlink 价格是可信的但有滞后（心跳间隔 1-24 小时）。两者互相验证：

```
                ┌─────────────────┐
                │   Chainlink     │
                │  price=2000.50  │
                │  updatedAt=T-30m│
                └────────┬────────┘
                         │
                    ┌────▼────┐      偏离 < 2%？
                    │ 交叉验证 │ ────── YES → 正常放行
                    └────┬────┘ ────── NO  → 保护模式
                         │
                ┌────────▼────────┐
                │   Pool Price    │
                │  price=2010.30  │
                │  (实时计算)      │
                └─────────────────┘
```

**有效性检查三步走**：

```go
// 第 1 步：Chainlink 过期检测
func (o *Oracle) UpdateChainlinkPrice(pair string, price float64, updatedAt time.Time) {
    // 关键：不只读 price，还校验 updatedAt
    isStale := time.Since(updatedAt) > o.config.StaleThreshold  // 默认 1 小时
    if isStale {
        o.activateProtection(pair, "Chainlink 价格过期")
    }
}

// 第 2 步：交叉验证
func (o *Oracle) crossValidate(pair string) {
    deviation := math.Abs(poolPrice - chainlinkPrice) / chainlinkPrice
    if deviation > 0.02 {  // 偏离 > 2%
        o.activateProtection(pair, fmt.Sprintf("偏离 %.2f%%", deviation*100))
    }
}

// 第 3 步：保护模式下的行为
func (o *Oracle) ValidateSwap(pair string, requestedSlippage float64) (...) {
    if inProtection {
        // 不直接拒绝交易（避免 DoS）
        // 而是强制用户设置至少 0.5% 的滑点保护
        if requestedSlippage < 0.005 {
            return false, 0.005, "请设置至少 0.5% 的滑点保护"
        }
        return true, requestedSlippage, "注意：保护模式已激活"
    }
}
```

**保护模式的激活与解除**：

| 事件 | 操作 |
|------|------|
| Chainlink `updatedAt` 超过 1 小时 | 激活保护模式 |
| 池内价格与 Chainlink 偏离 > 2% | 激活保护模式 |
| 偏离恢复到 < 2% 且冷却时间（5分钟）已过 | 解除保护模式 |

**代码位置**：`backend/oracle/oracle.go`

---

### 12. EIP-712 签名与三层防重放

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

**验证流程**：

```go
func (v *Verifier) Verify(permit *SwapPermit, sig *Signature) error {
    // Layer 1: Deadline
    if time.Now().Unix() > permit.Deadline {
        return errors.New("签名已过期")
    }

    // Layer 2: Nonce
    expectedNonce := v.nonces[permit.Owner]
    if permit.Nonce != expectedNonce {
        return fmt.Errorf("nonce 不匹配：期望 %d，收到 %d", expectedNonce, permit.Nonce)
    }

    // Layer 3: 签名验证（含 Domain Separator）
    hash := v.hashTypedData(permit)  // keccak256("\x19\x01" || domain || message)
    recoveredAddr := recoverAddress(hash, sig)
    if recoveredAddr != permit.Owner {
        return fmt.Errorf("签名验证失败")
    }

    // 三层全部通过 → 消费 nonce，防止重放
    v.nonces[permit.Owner] = expectedNonce + 1
    return nil
}
```

**代码位置**：`backend/eip712/eip712.go`

---

## Go 后端 — 工程基础设施

### 13. 断路器（Circuit Breaker）

**问题**：后端频繁调用外部服务（以太坊 RPC 节点、The Graph、Chainlink 预言机）。当外部服务故障时：

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

```go
// 使用示例
rpcCB := NewRPCCircuitBreaker("http://localhost:8545", DefaultConfig())

blockNum, err := rpcCB.Call("eth_blockNumber")
if errors.Is(err, ErrCircuitOpen) {
    // RPC 节点故障，自动熔断
    // 可以：使用缓存数据 / 切换备用节点 / 返回降级响应
}
```

**代码位置**：`backend/circuitbreaker/circuitbreaker.go`, `backend/circuitbreaker/circuitbreaker_test.go`

---

### 14. Goroutine 池与性能优化

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
func SubmitAndWaitResult[T any](p *Pool, fn func() T) <-chan T {
    ch := make(chan T, 1)
    p.Submit(func() {
        ch <- fn()
    })
    return ch
}

// 使用示例
result := <-SubmitAndWaitResult(pool, func() int {
    return computeExpensiveResult()
})
```

**Benchmark 实测数据**（Apple M2, Go 1.21）：

```
goos: darwin
goarch: arm64
cpu: Apple M2

BenchmarkNaiveGoroutine-8        1867     664692 ns/op    16026 B/op    1001 allocs/op
BenchmarkPooledGoroutine-8       4195     462957 ns/op    16020 B/op    1001 allocs/op

BenchmarkGoroutineMemory/Naive_10000_tasks-8     62    22623458 ns/op   1217396 B/op   20445 allocs/op
BenchmarkGoroutineMemory/Pool_10000_tasks-8      82    13745727 ns/op    163196 B/op   10011 allocs/op
```

**性能对比分析**：

| 指标 | 朴素 go func() | Goroutine 池 | 提升 |
|------|---------------|-------------|------|
| 1000 任务耗时 | 664μs | 462μs | **30% 更快** |
| 10000 任务耗时 | 22.6ms | 13.7ms | **39% 更快** |
| 10000 任务内存 | 1.19 MB | 0.16 MB | **86% 更少** |
| 10000 任务 allocs | 20445 | 10011 | **51% 更少** |

内存优化效果显著：10000 个任务时，池化方案的内存分配只有朴素方案的 **1/7**，alloc 次数减半。这意味着更少的 GC 压力和更稳定的延迟。

**代码位置**：`backend/workerpool/pool.go`, `backend/workerpool/pool_test.go`

---

### 15. 结构化日志与链路追踪

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
// HTTP 中间件中注入 TraceID
ctx = logging.WithTraceID(r.Context())

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
  "trace_id": "a1b2c3d4e5f67890abcd",
  "token_in": "WETH",
  "token_out": "USDC",
  "amount": "1000000000000000000"
}
```

在 Grafana / Kibana 中搜索 `trace_id = "a1b2c3d4e5f67890abcd"` 即可看到这个请求经过的所有模块的日志。

**代码位置**：`backend/logging/logging.go`

---

### 16. Prometheus 风格可观测性指标

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

**预定义的系统指标**：

```go
// 预注册到全局 Registry，各模块直接使用
var (
    HTTPRequestsTotal      = DefaultRegistry.Counter("http_requests_total")
    HTTPRequestDuration    = DefaultRegistry.Histogram("http_request_duration_ms", DefaultLatencyBuckets())
    HTTPActiveConnections  = DefaultRegistry.Gauge("http_active_connections")
    RouterRequestsTotal    = DefaultRegistry.Counter("router_requests_total")
    RouterDegradedTotal    = DefaultRegistry.Counter("router_degraded_total")
    RouterLatency          = DefaultRegistry.Histogram("router_latency_ms", DefaultLatencyBuckets())
    IndexerBlocksProcessed = DefaultRegistry.Counter("indexer_blocks_processed")
    IndexerReorgsTotal     = DefaultRegistry.Counter("indexer_reorgs_total")
    CacheHitsTotal         = DefaultRegistry.Counter("cache_hits_total")
    CacheMissesTotal       = DefaultRegistry.Counter("cache_misses_total")
    CircuitBreakerOpen     = DefaultRegistry.Gauge("circuitbreaker_open")
    WorkerPoolQueueUsed    = DefaultRegistry.Gauge("workerpool_queue_used")
)
```

**`/metrics` 端点输出**（Prometheus 文本格式）：

```
# TYPE http_requests_total counter
http_requests_total 12345

# TYPE http_request_duration_ms histogram
http_request_duration_ms_bucket{le="5"} 100
http_request_duration_ms_bucket{le="10"} 250
http_request_duration_ms_bucket{le="25"} 800
http_request_duration_ms_bucket{le="50"} 1100
http_request_duration_ms_bucket{le="100"} 1180
http_request_duration_ms_bucket{le="+Inf"} 1200
http_request_duration_ms_count 1200
http_request_duration_ms_avg 18.50
http_request_duration_ms_p50 12.00
http_request_duration_ms_p95 45.00
http_request_duration_ms_p99 95.00

# TYPE circuitbreaker_open gauge
circuitbreaker_open 0
```

Prometheus 定期 scrape 此端点，Grafana 展示面板。

**代码位置**：`backend/metrics/metrics.go`

---

### 17. WebSocket/SSE 实时推送

**为什么需要实时推送？**

| 方式 | 延迟 | 带宽 | 说明 |
|------|------|------|------|
| HTTP 轮询 | 轮询间隔/2 | 高（大量无效请求） | 100 用户 × 每秒 1 次 = 100 QPS 无效负载 |
| SSE 推送 | 毫秒级 | 低（仅推送变化） | 单向，服务端→客户端 |
| WebSocket | 毫秒级 | 低 | 双向，更复杂 |

本实现使用 SSE（Server-Sent Events），因为价格推送是单向的（服务端→客户端），SSE 更简单且浏览器原生支持。

**Hub 发布-订阅模式**：

```
    ┌──────────┐    ┌──────────┐    ┌──────────┐
    │ Client A │    │ Client B │    │ Client C │
    │ (SSE 连接)│    │ (SSE 连接)│    │ (SSE 连接)│
    └────┬─────┘    └────┬─────┘    └────┬─────┘
         │               │               │
         └───────────────┼───────────────┘
                         │ register / unregister
                    ┌────▼────┐
                    │   Hub   │  ← 单一 goroutine 处理所有事件
                    │         │     避免并发竞争
                    │ clients │  ← map[*Client]bool
                    │broadcast│  ← chan []byte (256 缓冲)
                    └────┬────┘
                         │ 价格变动事件
                    ┌────▼──────────┐
                    │ Event Indexer │
                    └───────────────┘
```

**关键设计**：

```go
// Hub 事件循环（单一 goroutine，无竞争）
func (h *Hub) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            // 关闭所有客户端
        case client := <-h.register:
            h.clients[client] = true
        case client := <-h.unregister:
            close(client.send)
            delete(h.clients, client)
        case message := <-h.broadcast:
            // 扇出到所有客户端
            for client := range h.clients {
                select {
                case client.send <- message:
                    // 发送成功
                default:
                    // 慢客户端检测：send buffer 满了
                    // 关闭连接，防止阻塞 Hub
                    close(client.send)
                    delete(h.clients, client)
                }
            }
        }
    }
}
```

**慢客户端驱逐策略**：

每个 Client 有一个 64 容量的 send channel。如果某个客户端消费太慢（网络差、处理慢），channel 满后 Hub 会立即关闭该连接。这防止了一个慢客户端阻塞整个 Hub 的广播。

**SSE 协议格式**：

```go
// SSEHandler — 标准 SSE 格式输出
func (h *Hub) SSEHandler() http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // SSE 必需的响应头
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")

        // 心跳：每 15 秒发送 SSE 注释行，保持连接活跃
        // 格式：": heartbeat\n\n"（冒号开头的行是注释，客户端忽略）
        heartbeat := time.NewTicker(15 * time.Second)

        for {
            select {
            case <-r.Context().Done():
                return  // 客户端断开
            case msg := <-client.send:
                // SSE 数据格式：data: {json}\n\n
                w.Write([]byte("data: "))
                w.Write(msg)
                w.Write([]byte("\n\n"))
                flusher.Flush()
            case <-heartbeat.C:
                w.Write([]byte(": heartbeat\n\n"))
                flusher.Flush()
            }
        }
    })
}
```

**客户端使用**：

```javascript
// 浏览器原生 EventSource，自动重连
const es = new EventSource("/ws/prices");
es.onmessage = (event) => {
    const update = JSON.parse(event.data);
    console.log(`${update.token0}/${update.token1}: ${update.price}`);
};
```

**代码位置**：`backend/ws/ws.go`

---

### 18. API 限流与背压控制

**双层保护**：

```
请求进入
    │
    ▼
┌───────────────────────────────────┐
│ Layer 1: 每 IP 限流 (Token Bucket)│
│                                   │
│ 每个 IP 有一个令牌桶               │
│ 容量 = maxTokens (100)            │
│ 补充速率 = refillRate (100ms/个)  │
│ 每次请求消耗 1 个令牌              │
│                                   │
│ 桶空？→ 429 Too Many Requests     │
└───────────┬───────────────────────┘
            │ 通过
            ▼
┌───────────────────────────────────┐
│ Layer 2: 全局并发限制 (Semaphore)  │
│                                   │
│ 有界 Channel (capacity=1000)      │
│ 非阻塞获取                        │
│                                   │
│ 满了？→ 503 Service Unavailable   │
│        + Retry-After: 1 头        │
└───────────┬───────────────────────┘
            │ 通过
            ▼
      正常处理请求
```

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

### 19. 优雅关闭与生命周期管理

**启动顺序（依赖图）**：

```
1. PoolStateManager (无依赖)
        ↓
2. Indexer (依赖 PoolStateManager)
        ↓
3. Cache + Warmup (依赖数据源)
        ↓
4. Oracle (无依赖)
        ↓
5. Router (依赖 PoolStateManager)
        ↓
6. API Server (依赖所有上游)
```

**关闭顺序（反向）**：

```go
// main.go — 优雅关闭
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
<-sigCh  // 阻塞等待退出信号

// 1. 先关闭 API Server（停止接受新请求）
shutdownCtx, _ := context.WithTimeout(context.Background(), 10*time.Second)
server.Shutdown(shutdownCtx)

// 2. 再关闭后台服务（Indexer、WebSocket Hub 等）
cancel()  // 取消根 context，所有 goroutine 通过 ctx.Done() 退出
```

**关键原则**：

1. **先停止入口**：关闭 HTTP Server，停止接受新请求
2. **等待处理中的请求完成**：`server.Shutdown(ctx)` 会等待活跃连接处理完毕
3. **超时保护**：10 秒后强制退出，防止某些连接永不关闭
4. **反向关闭依赖**：按依赖图反向关闭，确保上游不会调用已关闭的下游

**代码位置**：`backend/main.go`

---

## Benchmark 性能对比

### Goroutine 池 vs 朴素 goroutine

在 Apple M2 上运行 `go test -bench=. -benchmem`：

```
                                        耗时/op       内存/op      allocs/op
BenchmarkNaiveGoroutine (1000 tasks)    664μs        16.0 KB      1001
BenchmarkPooledGoroutine (1000 tasks)   462μs        16.0 KB      1001

BenchmarkGoroutineMemory:
  Naive  (10000 tasks)                  22.6ms       1.19 MB      20445
  Pool   (10000 tasks)                  13.7ms       0.16 MB      10011
```

| 场景 | 提升 | 原因 |
|------|------|------|
| 1000 任务延迟 | 30% 更快 | 无 goroutine 创建/销毁开销 |
| 10000 任务延迟 | 39% 更快 | 调度器管理的 goroutine 数固定 |
| 10000 任务内存 | 86% 更少 | 栈空间复用，无重复分配 |
| 10000 任务 allocs | 51% 更少 | GC 扫描范围更小，STW 更短 |

**关键洞察**：任务规模越大，池化的优势越明显。这是因为朴素方案的开销与任务数线性增长，而池化方案的开销是固定的。

运行方式：`cd backend && go test ./workerpool/ -bench=. -benchmem -count=3`

---

## 快速开始

### 环境要求

- Node.js >= 18
- Go >= 1.21

### 编译与测试

```bash
# 克隆项目
git clone <repo-url>
cd UniGo-DEX

# === 智能合约 ===
cd contracts
npm install
npx hardhat compile    # 编译 23 个 Solidity 文件
npx hardhat test       # 运行 14 个合约测试

# === Go 后端 ===
cd ../backend
go build ./...         # 编译所有 12 个包
go test ./...          # 运行所有测试（math + circuitbreaker + workerpool）

# 运行 benchmark
go test ./workerpool/ -bench=. -benchmem -count=3

# 启动后端
go run main.go         # API 运行在 :8080
```

### 部署到本地 Hardhat 网络

```bash
# 终端 1：启动本地节点
cd contracts && npx hardhat node

# 终端 2：部署合约
cd contracts && npx hardhat run scripts/deploy.js --network localhost
```

### API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/quote?token_in=&token_out=&amount_in=` | 获取报价（Singleflight 防击穿） |
| POST | `/api/v1/swap` | 提交交易（含 EIP-712 签名验证） |
| GET | `/api/v1/pools` | 所有池子信息 |
| GET | `/api/v1/status` | 系统状态（路由/索引器/缓存统计） |
| GET | `/health` | 健康检查 |
| GET | `/metrics` | Prometheus 指标（P50/P95/P99） |
| GET | `/ws/prices` | SSE 实时价格推送 |

---

## 项目结构

```
UniGo-DEX/
├── contracts/                              # Solidity 智能合约层
│   ├── contracts/
│   │   ├── UniGoPool.sol                   # 核心流动性池（mint/swap/burn/collect）
│   │   ├── UniGoFactory.sol                # 池工厂（CREATE2 确定性部署）
│   │   ├── UniGoManager.sol                # 用户交互管理器（回调处理）
│   │   ├── UniGoQuoter.sol                 # 链上报价（revert-catch 模拟）
│   │   ├── interfaces/
│   │   │   ├── IUniGoPool.sol              # Pool 接口
│   │   │   ├── IUniGoFactory.sol           # Factory 接口
│   │   │   └── IUniGoPoolCallbacks.sol     # mint/swap 回调接口
│   │   ├── lib/                            # 数学库（10 个）
│   │   │   ├── Math.sol                    # 512 位 mulDiv（牛顿法模逆）
│   │   │   ├── TickMath.sol                # tick ↔ sqrtPrice（查表法）
│   │   │   ├── SqrtPriceMath.sol           # 基于价格计算 token 数量
│   │   │   ├── SwapMath.sol                # 单步 swap 计算
│   │   │   ├── TickBitmap.sol              # O(1) tick 查找（位图）
│   │   │   ├── BitMath.sol                 # MSB/LSB 位运算
│   │   │   ├── Tick.sol                    # tick 状态（update/cross）
│   │   │   ├── Position.sol                # position 状态
│   │   │   ├── LiquidityMath.sol           # 流动性加减
│   │   │   └── FixedPoint96.sol            # Q64.96 常量
│   │   └── test/
│   │       └── TestERC20.sol               # 测试用 ERC20
│   ├── test/
│   │   ├── UniGoPool.test.js               # Pool 测试（4 个用例）
│   │   └── UniGoFactory.test.js            # Factory 测试（10 个用例）
│   ├── scripts/
│   │   └── deploy.js                       # 部署脚本
│   ├── hardhat.config.js
│   └── package.json
│
├── backend/                                # Go 后端（12 个模块）
│   ├── main.go                             # 入口 + 模块编排 + 优雅关闭
│   │
│   ├── math/                               # 链下数学库（链上链下 bit-exact 一致）
│   │   ├── tickmath.go                     # tick ↔ sqrtPrice（20 魔数查表法）
│   │   ├── sqrtpricemath.go                # token 数量计算 + ComputeSwapStep
│   │   ├── constants.go                    # Q96, MIN/MAX_TICK, FeeToTickSpacing
│   │   └── tickmath_test.go                # 已知值验证 + 往返测试
│   │
│   ├── router/                             # 智能路由引擎
│   │   ├── router.go                       # BFS + 200ms 超时 + 二级降级 + sync.Pool
│   │   └── quote.go                        # 链下 swap 模拟（SimulateSwap）
│   │
│   ├── indexer/                            # 事件索引器
│   │   ├── indexer.go                      # 区块轮询 + parentHash Reorg 检测 + 回滚
│   │   ├── events.go                       # Mint/Burn/Swap/Collect 事件类型
│   │   └── pool_state.go                   # PoolState + PoolStateManager + Snapshot
│   │
│   ├── cache/                              # 缓存层
│   │   └── cache.go                        # Singleflight 请求合并 + TTL + Warmup 预热
│   │
│   ├── oracle/                             # 价格预言机验证
│   │   └── oracle.go                       # Chainlink 过期检测 + 交叉验证 + 保护模式
│   │
│   ├── eip712/                             # 签名验证
│   │   └── eip712.go                       # EIP-712 三层防重放（Deadline+Nonce+Domain）
│   │
│   ├── circuitbreaker/                     # 断路器
│   │   ├── circuitbreaker.go               # 三态状态机（Closed→Open→HalfOpen）
│   │   └── circuitbreaker_test.go          # 状态转换 + 恢复 + 回调测试
│   │
│   ├── workerpool/                         # Goroutine 池
│   │   ├── pool.go                         # 固定 Worker + 有界 Channel + 泛型结果
│   │   └── pool_test.go                    # 功能测试 + Benchmark 对比
│   │
│   ├── logging/                            # 结构化日志
│   │   └── logging.go                      # slog + JSON/Text + TraceID 链路追踪
│   │
│   ├── metrics/                            # 可观测性指标
│   │   └── metrics.go                      # Counter/Gauge/Histogram + P99 + /metrics 端点
│   │
│   ├── ws/                                 # 实时推送
│   │   └── ws.go                           # Hub 发布-订阅 + SSE + 慢客户端驱逐
│   │
│   ├── api/                                # REST API
│   │   ├── server.go                       # HTTP 路由 + 端点实现
│   │   ├── middleware.go                   # Token Bucket 限流 + 信号量并发控制
│   │   └── types.go                        # 请求/响应类型定义
│   │
│   ├── go.mod
│   └── go.sum
│
├── README.md                               # 本文件
├── Makefile                                # make compile / test / deploy / run-backend
└── .gitignore
```

---

## 参考资料

- [Uniswap V3 白皮书](https://uniswap.org/whitepaper-v3.pdf)
- [Uniswap V3 开发教程 (Jeiwan)](https://uniswapv3book.com/)
- [Uniswap V3 Core 源码](https://github.com/Uniswap/v3-core)
- [EIP-712: Typed structured data hashing and signing](https://eips.ethereum.org/EIPS/eip-712)
- [Chainlink Price Feeds](https://docs.chain.link/data-feeds)
- [Go slog 官方文档](https://pkg.go.dev/log/slog)
- [Circuit Breaker Pattern (Martin Fowler)](https://martinfowler.com/bliki/CircuitBreaker.html)
- [Token Bucket Algorithm](https://en.wikipedia.org/wiki/Token_bucket)

## 许可证

GPL-2.0-or-later
