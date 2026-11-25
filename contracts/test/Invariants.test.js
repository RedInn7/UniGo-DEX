const { expect } = require("chai");
const { ethers } = require("hardhat");
const {
  NO_DEADLINE,
  MIN_SQRT_RATIO_PLUS_ONE,
  MAX_SQRT_RATIO_MINUS_ONE,
  Q128,
  MAX_UINT128,
  deployTokens,
  deployCore,
  createPool,
  fund,
  positionKey,
} = require("./helpers");

/**
 * 不变量 / 随机化测试
 *
 * 用固定种子的伪随机数对一个池子执行几百步随机操作
 * （mint / burn / 双向 swap / 精确输出 swap / flash / collect），每一步之后检查：
 * 1. 偿付能力：池子两种 token 的余额 ≥ 所有 position 的应得本金 + 未领手续费 + 已结算未提取数量
 * 2. 活跃流动性：pool.liquidity 等于所有覆盖当前 tick 的 position 流动性之和
 * 3. 手续费累计值 feeGrowthGlobal 单调不减
 * 4. 每个 tick 的 liquidityGross / liquidityNet 与所有 position 的流动性吻合，且不超过单 tick 上限
 *
 * 操作集合里包含针对单 tick 流动性上限的攻击：在远离现价的极端区间反复 mint 接近
 * int128 上限的流动性，试图让 liquidityNet 回绕；这些 mint 要么被拒绝，要么如实记账
 * 最后所有 LP 全部撤出并提取，池子必须付得出每一笔，且只剩舍入产生的零头
 *
 * 可用环境变量 INVARIANT_SEED / INVARIANT_STEPS 换种子或加长步数
 */
describe("不变量随机测试", function () {
  const SEED = Number(process.env.INVARIANT_SEED || 42);
  const STEPS = Number(process.env.INVARIANT_STEPS || 300);
  const UINT256 = 2n ** 256n;
  const ONE = ethers.parseEther("1");

  // 可选的价格区间（tickSpacing = 60），0 号是永不撤出的全范围底仓
  const RANGES = [
    [-887220, 887220],
    [-600, 600],
    [-120, 120],
    [-60, 60],
    [60, 300],
    [-300, -60],
    [540, 1200],
    [-1200, -540],
    // 远离现价的极端区间：本金几乎为零，是堆积巨额流动性成本最低的地方
    [887100, 887160],
    [-887160, -887100],
  ];
  // 极端区间在 RANGES 里的下标
  const EXTREME_RANGES = [8, 9];
  // 接近 int128 上限的流动性
  const NEAR_INT128_MAX = 2n ** 127n - 1n;

  // mulberry32：固定种子的 32 位伪随机数发生器
  function createRandom(seed) {
    let state = seed >>> 0;
    const next = () => {
      state = (state + 0x6d2b79f5) >>> 0;
      let t = state;
      t = Math.imul(t ^ (t >>> 15), t | 1);
      t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
    return {
      // [0, n) 的整数
      int: (n) => Math.floor(next() * n),
      // [min, max] ether 之间的随机数量（精确到 wei 级别的不规则数）
      amount: (min, max) => {
        const scaled = BigInt(Math.floor(next() * 1e9));
        const span = ethers.parseEther(String(max)) - ethers.parseEther(String(min));
        return ethers.parseEther(String(min)) + (span * scaled) / 1000000000n + BigInt(Math.floor(next() * 1e6));
      },
      pick: (list) => list[Math.floor(next() * list.length)],
    };
  }

  // 模 2^256 的减法（手续费累计值依赖回绕）
  const sub256 = (a, b) => (((a - b) % UINT256) + UINT256) % UINT256;

  it(`固定种子 ${SEED}，随机执行 ${STEPS} 步后不变量始终成立`, async function () {
    this.timeout(600000);

    const [owner, lpA, lpB] = await ethers.getSigners();
    const actors = [owner, lpA, lpB];
    const { factory, manager } = await deployCore();
    const managerAddr = await manager.getAddress();
    const [token0, token1] = await deployTokens(2);
    for (const actor of actors) await fund([token0, token1], actor, managerAddr);

    const pool = await createPool(factory, token0, token1, 3000);
    const poolAddr = await pool.getAddress();
    const addr0 = await token0.getAddress();
    const addr1 = await token1.getAddress();

    const Borrower = await ethers.getContractFactory("TestFlashBorrower");
    const borrower = await Borrower.deploy();
    await borrower.waitForDeployment();
    await token0.mint(await borrower.getAddress(), ethers.parseEther("1000"));
    await token1.mint(await borrower.getAddress(), ethers.parseEther("1000"));

    const random = createRandom(SEED);
    // 链下模型：记录每个 position 的流动性，key 为 "actor 下标:区间下标"
    const model = new Map();
    const counts = { mint: 0, burn: 0, swapIn: 0, swapOut: 0, flash: 0, collect: 0, hugeMint: 0 };
    let hugeMintAccepted = 0;
    let hugeMintRejected = 0;
    const maxLiquidityPerTick = await pool.maxLiquidityPerTick();
    const crossed = new Set();

    async function mint(actorIndex, rangeIndex, amount) {
      const [tickLower, tickUpper] = RANGES[rangeIndex];
      await manager.connect(actors[actorIndex]).mint({
        pool: poolAddr,
        tickLower,
        tickUpper,
        amount,
        amount0Min: 0,
        amount1Min: 0,
        deadline: NO_DEADLINE,
      });
      const key = `${actorIndex}:${rangeIndex}`;
      model.set(key, (model.get(key) || 0n) + amount);
    }

    // 底仓：全范围流动性，保证任何价格下都能成交
    await mint(0, 0, ethers.parseEther("1000"));

    // ---------- 随机操作 ----------

    const actions = {
      async mint() {
        // 普通 mint 只用常规区间（极端区间留给 hugeMint）
        await mint(1 + random.int(2), 1 + random.int(EXTREME_RANGES[0] - 1), random.amount(1, 500));
      },

      // 攻击：在极端区间 mint 巨额流动性，试图让 tick 的 liquidityNet 回绕
      async hugeMint() {
        const amount = random.pick([
          NEAR_INT128_MAX,
          maxLiquidityPerTick,
          maxLiquidityPerTick / 2n + 1n,
          maxLiquidityPerTick / 3n,
        ]);
        try {
          await mint(1 + random.int(2), random.pick(EXTREME_RANGES), amount);
          hugeMintAccepted++;
        } catch (error) {
          // 只允许因为超过单 tick 上限而被拒绝
          if (!error.message.includes("'LO'")) throw error;
          hugeMintRejected++;
        }
      },

      async burn() {
        const candidates = [...model.entries()].filter(([key, l]) => key !== "0:0" && l > 0n);
        if (candidates.length === 0) return actions.mint();
        const [key, liquidity] = random.pick(candidates);
        const [actorIndex, rangeIndex] = key.split(":").map(Number);
        const amount = (liquidity * BigInt(random.pick([25, 50, 100]))) / 100n;
        await pool.connect(actors[actorIndex]).burn(...RANGES[rangeIndex], amount);
        model.set(key, liquidity - amount);
      },

      async swapIn() {
        const zeroForOne = random.int(2) === 0;
        await manager.swap({
          pool: poolAddr,
          zeroForOne,
          amountSpecified: random.amount(0.01, 40),
          sqrtPriceLimitX96: zeroForOne ? MIN_SQRT_RATIO_PLUS_ONE : MAX_SQRT_RATIO_MINUS_ONE,
          recipient: owner.address,
          amountOutMinimum: 0,
          deadline: NO_DEADLINE,
        });
      },

      async swapOut() {
        const zeroForOne = random.int(2) === 0;
        await manager.exactOutputSingle({
          tokenIn: zeroForOne ? addr0 : addr1,
          tokenOut: zeroForOne ? addr1 : addr0,
          fee: 3000,
          recipient: owner.address,
          amountOut: random.amount(0.01, 15),
          amountInMaximum: ethers.MaxUint256,
          sqrtPriceLimitX96: 0,
          deadline: NO_DEADLINE,
        });
      },

      async flash() {
        // 随机只借一种或两种都借
        const mode = random.int(3);
        const amount0 = mode === 1 ? 0n : random.amount(0.01, 50);
        const amount1 = mode === 0 ? 0n : random.amount(0.01, 50);
        await borrower.flash(poolAddr, amount0, amount1, 0);
      },

      async collect() {
        const [key, liquidity] = random.pick([...model.entries()]);
        const [actorIndex, rangeIndex] = key.split(":").map(Number);
        const actor = actors[actorIndex];
        const [tickLower, tickUpper] = RANGES[rangeIndex];
        // 先结算手续费，再随机提取一半或全部
        if (liquidity > 0n) await pool.connect(actor).burn(tickLower, tickUpper, 0);
        const position = await pool.positions(positionKey(actor.address, tickLower, tickUpper));
        const divisor = BigInt(random.pick([1, 2]));
        await pool
          .connect(actor)
          .collect(actor.address, tickLower, tickUpper, position.tokensOwed0 / divisor, position.tokensOwed1 / divisor);
      },
    };

    // 按权重展开的操作表
    const schedule = [
      ...Array(4).fill("mint"),
      ...Array(3).fill("burn"),
      ...Array(6).fill("swapIn"),
      ...Array(2).fill("swapOut"),
      ...Array(2).fill("flash"),
      ...Array(3).fill("collect"),
      ...Array(2).fill("hugeMint"),
    ];

    // ---------- 不变量检查 ----------

    let lastFeeGrowth0 = 0n;
    let lastFeeGrowth1 = 0n;

    async function checkInvariants(step, action) {
      const label = `第 ${step} 步（${action}）`;
      const tick = (await pool.slot0()).tick;
      const feeGrowth0 = await pool.feeGrowthGlobal0X128();
      const feeGrowth1 = await pool.feeGrowthGlobal1X128();

      // 3. 手续费累计值单调不减
      expect(feeGrowth0, `${label} feeGrowthGlobal0 回退`).to.be.gte(lastFeeGrowth0);
      expect(feeGrowth1, `${label} feeGrowthGlobal1 回退`).to.be.gte(lastFeeGrowth1);
      lastFeeGrowth0 = feeGrowth0;
      lastFeeGrowth1 = feeGrowth1;

      // 每个用到的 tick 只读一次
      const tickCache = new Map();
      const readTick = async (t) => {
        if (!tickCache.has(t)) tickCache.set(t, await pool.ticks(t));
        return tickCache.get(t);
      };

      let activeLiquidity = 0n;
      let claims0 = 0n;
      let claims1 = 0n;

      for (const [key, liquidity] of model) {
        const [actorIndex, rangeIndex] = key.split(":").map(Number);
        const actor = actors[actorIndex];
        const [tickLower, tickUpper] = RANGES[rangeIndex];
        const position = await pool.positions(positionKey(actor.address, tickLower, tickUpper));

        // 链上流动性与模型一致
        expect(position.liquidity, `${label} position ${key} 流动性`).to.equal(liquidity);

        // 已结算未提取的数量
        claims0 += position.tokensOwed0;
        claims1 += position.tokensOwed1;

        if (liquidity === 0n) continue;

        if (BigInt(tickLower) <= tick && tick < BigInt(tickUpper)) activeLiquidity += liquidity;

        // 应得本金：模拟把全部流动性撤出能拿回多少
        const [principal0, principal1] = await pool
          .connect(actor)
          .burn.staticCall(tickLower, tickUpper, liquidity);
        claims0 += principal0;
        claims1 += principal1;

        // 未结算的手续费：区间内累计值的增量 * 流动性
        const lower = await readTick(tickLower);
        const upper = await readTick(tickUpper);
        const inside = (global, lowerOutside, upperOutside) => {
          const below = tick >= BigInt(tickLower) ? lowerOutside : sub256(global, lowerOutside);
          const above = tick < BigInt(tickUpper) ? upperOutside : sub256(global, upperOutside);
          return sub256(sub256(global, below), above);
        };
        const inside0 = inside(feeGrowth0, lower.feeGrowthOutside0X128, upper.feeGrowthOutside0X128);
        const inside1 = inside(feeGrowth1, lower.feeGrowthOutside1X128, upper.feeGrowthOutside1X128);
        claims0 += (sub256(inside0, position.feeGrowthInside0LastX128) * liquidity) / Q128;
        claims1 += (sub256(inside1, position.feeGrowthInside1LastX128) * liquidity) / Q128;
      }

      // 4. 每个 tick 的流动性统计与 position 吻合，且不超过上限
      const expectedGross = new Map();
      const expectedNet = new Map();
      for (const [key, liquidity] of model) {
        const [tickLower, tickUpper] = RANGES[Number(key.split(":")[1])];
        expectedGross.set(tickLower, (expectedGross.get(tickLower) || 0n) + liquidity);
        expectedGross.set(tickUpper, (expectedGross.get(tickUpper) || 0n) + liquidity);
        expectedNet.set(tickLower, (expectedNet.get(tickLower) || 0n) + liquidity);
        expectedNet.set(tickUpper, (expectedNet.get(tickUpper) || 0n) - liquidity);
      }
      for (const [t, gross] of expectedGross) {
        const info = await readTick(t);
        expect(info.liquidityGross, `${label} tick ${t} liquidityGross`).to.equal(gross);
        expect(info.liquidityNet, `${label} tick ${t} liquidityNet`).to.equal(expectedNet.get(t));
        expect(info.liquidityGross, `${label} tick ${t} 超过上限`).to.be.lte(maxLiquidityPerTick);
      }

      // 2. 活跃流动性等于覆盖当前 tick 的 position 之和
      expect(await pool.liquidity(), `${label} 活跃流动性`).to.equal(activeLiquidity);

      // 1. 偿付能力
      expect(await token0.balanceOf(poolAddr), `${label} token0 资不抵债`).to.be.gte(claims0);
      expect(await token1.balanceOf(poolAddr), `${label} token1 资不抵债`).to.be.gte(claims1);

      // 记录价格走到过哪些区间边界的哪一侧，用于确认确实发生了跨 tick
      for (const [tickLower, tickUpper] of RANGES.slice(1)) {
        if (tick < BigInt(tickLower)) crossed.add(`${tickLower}<`);
        if (tick >= BigInt(tickUpper)) crossed.add(`${tickUpper}>`);
      }
    }

    await checkInvariants(0, "初始底仓");

    for (let step = 1; step <= STEPS; step++) {
      const action = random.pick(schedule);
      await actions[action]();
      counts[action]++;
      await checkInvariants(step, action);
    }

    // 每种操作都执行过，且价格确实多次越过区间边界
    for (const [action, count] of Object.entries(counts)) {
      expect(count, `${action} 没有被执行`).to.be.gt(0);
    }
    expect(crossed.size, "价格没有越过足够多的区间边界").to.be.gte(6);
    // 攻击性的巨额 mint 既有被上限挡住的，也有在上限以内如实记账的
    expect(hugeMintRejected, "没有巨额 mint 被上限拒绝").to.be.gt(0);
    expect(hugeMintAccepted, "没有巨额 mint 在上限以内成功").to.be.gt(0);

    // ---------- 收尾：所有 LP 全部撤出并提取 ----------

    for (const [key, liquidity] of model) {
      const [actorIndex, rangeIndex] = key.split(":").map(Number);
      const actor = actors[actorIndex];
      const [tickLower, tickUpper] = RANGES[rangeIndex];
      if (liquidity > 0n) await pool.connect(actor).burn(tickLower, tickUpper, liquidity);
      await pool.connect(actor).collect(actor.address, tickLower, tickUpper, MAX_UINT128, MAX_UINT128);

      const position = await pool.positions(positionKey(actor.address, tickLower, tickUpper));
      expect(position.liquidity).to.equal(0);
      expect(position.tokensOwed0).to.equal(0);
      expect(position.tokensOwed1).to.equal(0);
    }

    expect(await pool.liquidity()).to.equal(0);
    // 每一笔都付得出，池子里只剩向池子有利方向舍入攒下的零头
    const dust0 = await token0.balanceOf(poolAddr);
    const dust1 = await token1.balanceOf(poolAddr);
    expect(dust0, "token0 零头过大").to.be.lt(ONE / 1000000n);
    expect(dust1, "token1 零头过大").to.be.lt(ONE / 1000000n);
  });
});
