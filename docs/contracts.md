# 智能合约层与子图

> 小节编号沿用原文：1–13 见 [contracts.md](contracts.md)，14–22 见 [backend.md](backend.md)，23–26 见 [market-mev.md](market-mev.md)，27–32 见 [infrastructure.md](infrastructure.md)；测试见 [testing.md](testing.md)，压测见 [benchmark.md](benchmark.md)，部署与 API 见 [getting-started.md](getting-started.md)。

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

> 使用说明：子图需要自行部署 graph-node 来运行。`subgraph.yaml` 默认指向本地 Hardhat 节点（network `localhost`）和 `scripts/deploy.js` 在全新节点上部署出的 Factory 地址，部署到其它网络时修改 `network / address / startBlock`。构建步骤见[快速开始](getting-started.md)。

**代码位置**：`subgraph/schema.graphql`, `subgraph/subgraph.yaml`, `subgraph/src/factory.ts`, `subgraph/src/pool.ts`

