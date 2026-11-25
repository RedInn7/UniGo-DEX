const { expect } = require("chai");
const { ethers } = require("hardhat");
const {
  NO_DEADLINE,
  MIN_SQRT_RATIO_PLUS_ONE,
  MAX_SQRT_RATIO_MINUS_ONE,
  MAX_UINT128,
  encodeSqrtPrice,
  deployTokens,
  deployCore,
  createPool,
  fund,
  positionKey,
} = require("./helpers");

/**
 * 安全性回归测试
 *
 * 每一组对应一个曾经存在的问题，测试描述的是修复之后必须成立的行为：
 * 1. 单个 tick 的流动性上限（防止 liquidityNet 回绕后盗取其他 LP 的资金）
 * 2. 代币兼容性（失败时返回 false 的代币、没有返回值的代币）
 * 3. 没有活跃流动性时禁止闪电贷
 * 4. FlashArbitrage 部分成交时整笔回滚
 * 5. Manager.swap 只接受精确输入
 */
describe("安全性回归", function () {
  let factory;
  let manager;
  let managerAddr;
  let owner;
  let honest;
  let attacker;

  const FUNDS = ethers.parseEther("1000");

  function mintParams(poolAddr, tickLower, tickUpper, amount) {
    return {
      pool: poolAddr,
      tickLower,
      tickUpper,
      amount,
      amount0Min: 0,
      amount1Min: 0,
      deadline: NO_DEADLINE,
    };
  }

  function swapParams(poolAddr, recipient, overrides = {}) {
    return {
      pool: poolAddr,
      zeroForOne: true,
      amountSpecified: ethers.parseEther("1"),
      sqrtPriceLimitX96: MIN_SQRT_RATIO_PLUS_ONE,
      recipient,
      amountOutMinimum: 0,
      deadline: NO_DEADLINE,
      ...overrides,
    };
  }

  beforeEach(async function () {
    [owner, honest, attacker] = await ethers.getSigners();
    ({ factory, manager } = await deployCore());
    managerAddr = await manager.getAddress();
  });

  describe("单个 tick 的流动性上限", function () {
    let token0;
    let token1;
    let pool;
    let poolAddr;
    let math;

    const HONEST_LIQUIDITY = ethers.parseEther("1000");
    // 攻击者选在远离现价的极端区间：这里的 token0 本金几乎为零
    const GIANT = [887100, 887160];
    // 覆盖 GIANT 的小仓位，保证价格进入 GIANT 时活跃流动性不为 0
    const HELPER = [887040, 887220];
    // 接近 int128 上限的流动性，两次相加会让 liquidityNet 回绕
    const NEAR_INT128_MAX = 2n ** 127n - 1n;

    const balances = async (address) => [await token0.balanceOf(address), await token1.balanceOf(address)];

    beforeEach(async function () {
      [token0, token1] = await deployTokens(2);
      await fund([token0, token1], honest, managerAddr, FUNDS);
      await fund([token0, token1], attacker, managerAddr, FUNDS);
      pool = await createPool(factory, token0, token1, 3000);
      poolAddr = await pool.getAddress();

      const Harness = await ethers.getContractFactory("TestMathHarness");
      math = await Harness.deploy();
      await math.waitForDeployment();

      await manager.connect(honest).mint(mintParams(poolAddr, -600, 600, HONEST_LIQUIDITY));
    });

    it("上限与 Uniswap V3 的 tickSpacingToMaxLiquidityPerTick 一致", async function () {
      // 上限 = uint128 最大值 / 可用 tick 的个数
      const expected = (tickSpacing) => {
        const maxTick = (887272n / tickSpacing) * tickSpacing;
        return MAX_UINT128 / ((maxTick * 2n) / tickSpacing + 1n);
      };
      expect(await pool.maxLiquidityPerTick()).to.equal(expected(60n));
      // V3 0.3% 池的已知值
      expect(await pool.maxLiquidityPerTick()).to.equal(11505743598341114571880798222544994n);

      const pool500 = await createPool(factory, token0, token1, 500);
      expect(await pool500.maxLiquidityPerTick()).to.equal(1917569901783203986719870431555990n);
      const pool10000 = await createPool(factory, token0, token1, 10000);
      expect(await pool10000.maxLiquidityPerTick()).to.equal(38350317471085141830651933667504588n);
    });

    it("单个 tick 的流动性超过上限时 mint 被拒绝", async function () {
      await expect(
        manager.connect(attacker).mint(mintParams(poolAddr, ...GIANT, NEAR_INT128_MAX))
      ).to.be.revertedWith("LO");

      // 恰好等于上限可以，再多 1 就不行
      const max = await pool.maxLiquidityPerTick();
      await manager.connect(attacker).mint(mintParams(poolAddr, ...GIANT, max));
      await expect(
        manager.connect(attacker).mint(mintParams(poolAddr, ...GIANT, 1n))
      ).to.be.revertedWith("LO");
      // 上限按 tick 统计：共用同一个边界 tick 的其他区间也会被挡住
      await expect(
        manager.connect(attacker).mint(mintParams(poolAddr, GIANT[1], GIANT[1] + 60, 1n))
      ).to.be.revertedWith("LO");
    });

    it("即使不设上限，liquidityNet 溢出也会 revert 而不是回绕", async function () {
      const Harness = await ethers.getContractFactory("TestTickHarness");
      const harness = await Harness.deploy();
      await harness.waitForDeployment();

      // 把上限放到 uint128 最大值，单独验证 liquidityNet 的受检运算
      await harness.update(0, NEAR_INT128_MAX, false, MAX_UINT128);
      await expect(harness.update(0, NEAR_INT128_MAX, false, MAX_UINT128)).to.be.revertedWithPanic(0x11);
      await harness.update(60, NEAR_INT128_MAX, true, MAX_UINT128);
      await expect(harness.update(60, NEAR_INT128_MAX, true, MAX_UINT128)).to.be.revertedWithPanic(0x11);

      expect((await harness.ticks(0)).liquidityNet).to.equal(NEAR_INT128_MAX);
      expect((await harness.ticks(60)).liquidityNet).to.equal(-NEAR_INT128_MAX);
    });

    it("liquidityNet 回绕盗取资金的攻击序列被挡在第一步", async function () {
      // 攻击原理：同一区间 mint 两次接近 int128 上限的流动性，liquidityNet 回绕成 -2。
      // 价格进入该区间时全局流动性不含这笔巨额流动性，手续费却按巨额流动性结算，
      // 攻击者只需极少的手续费就能把池子里其他 LP 的 token 全部“领”走
      await manager.connect(attacker).mint(mintParams(poolAddr, ...HELPER, 10n));
      await expect(
        manager.connect(attacker).mint(mintParams(poolAddr, ...GIANT, NEAR_INT128_MAX))
      ).to.be.revertedWith("LO");
    });

    it("按上限走完同样的攻击序列也拿不走其他 LP 的资金", async function () {
      const attackerBefore = await balances(attacker.address);
      const max = await pool.maxLiquidityPerTick();

      // 1. 攻击者铺好小仓位和上限流动性的大仓位
      await manager.connect(attacker).mint(mintParams(poolAddr, ...HELPER, 10n));
      await manager.connect(attacker).mint(mintParams(poolAddr, ...GIANT, max));
      const lower = await pool.ticks(GIANT[0]);
      expect(lower.liquidityGross).to.equal(max);
      expect(lower.liquidityNet).to.equal(max);

      // 2. 买空诚实 LP 区间里的 token0，把价格推进自己的区间
      await manager.connect(attacker).swap(
        swapParams(poolAddr, attacker.address, {
          zeroForOne: false,
          amountSpecified: ethers.parseEther("500"),
          sqrtPriceLimitX96: await math.getSqrtRatioAtTick(887130),
        })
      );
      expect((await pool.slot0()).tick).to.be.gte(GIANT[0]);
      // 进入区间后全局流动性如实包含大仓位
      expect(await pool.liquidity()).to.equal(max + 10n);

      // 3. 结算并提取大仓位的全部手续费，再撤出所有仓位
      for (const [range, liquidity] of [[GIANT, max], [HELPER, 10n]]) {
        await pool.connect(attacker).burn(...range, liquidity);
        await pool.connect(attacker).collect(attacker.address, ...range, MAX_UINT128, MAX_UINT128);
      }

      // 攻击者只是按市价买走了 token0：付出的 token1 超过拿到的 token0
      const attackerAfter = await balances(attacker.address);
      const gained0 = attackerAfter[0] - attackerBefore[0];
      const paid1 = attackerBefore[1] - attackerAfter[1];
      expect(gained0).to.be.gt(0);
      expect(paid1).to.be.gt(gained0);

      // 诚实 LP 仍能全额撤出，并拿到攻击者付出的 token1（含手续费）
      await pool.connect(honest).burn(-600, 600, HONEST_LIQUIDITY);
      await pool.connect(honest).collect(honest.address, -600, 600, MAX_UINT128, MAX_UINT128);
      const honestAfter = await balances(honest.address);
      // 以 mint 之前的初始资金 FUNDS 为基准：token0 被买走多少就少多少，token1 多出攻击者付的全部
      expect(honestAfter[1]).to.be.closeTo(FUNDS + paid1, 100);
      expect(honestAfter[0]).to.be.closeTo(FUNDS - gained0, 100);

      // 池子里只剩舍入零头
      const [dust0, dust1] = await balances(poolAddr);
      expect(dust0).to.be.lt(100);
      expect(dust1).to.be.lt(100);
    });
  });

  describe("代币兼容性", function () {
    const LIQUIDITY = ethers.parseEther("100");

    // 用一个特殊代币和一个标准代币建池，返回按地址排序后的信息
    async function setupWith(specialName) {
      const Special = await ethers.getContractFactory(specialName);
      const special = await Special.deploy();
      await special.waitForDeployment();
      const [standard] = await deployTokens(1);

      for (const token of [special, standard]) {
        await token.mint(owner.address, FUNDS);
        await token.connect(owner).approve(managerAddr, ethers.MaxUint256);
      }

      const pool = await createPool(factory, special, standard, 3000);
      const poolAddr = await pool.getAddress();
      const specialIsToken0 = (await pool.token0()) === (await special.getAddress());
      return { special, standard, pool, poolAddr, specialIsToken0 };
    }

    describe("没有返回值的代币（USDT 式）", function () {
      let ctx;

      beforeEach(async function () {
        ctx = await setupWith("TestNoReturnERC20");
      });

      it("能正常 mint", async function () {
        await manager.mint(mintParams(ctx.poolAddr, -600, 600, LIQUIDITY));
        expect(await ctx.pool.liquidity()).to.equal(LIQUIDITY);
        expect(await ctx.special.balanceOf(ctx.poolAddr)).to.be.gt(0);
      });

      it("能正常双向 swap", async function () {
        await manager.mint(mintParams(ctx.poolAddr, -600, 600, LIQUIDITY));

        // 卖出特殊代币
        const standardBefore = await ctx.standard.balanceOf(owner.address);
        await manager.swap(
          swapParams(ctx.poolAddr, owner.address, {
            zeroForOne: ctx.specialIsToken0,
            sqrtPriceLimitX96: ctx.specialIsToken0 ? MIN_SQRT_RATIO_PLUS_ONE : MAX_SQRT_RATIO_MINUS_ONE,
          })
        );
        expect(await ctx.standard.balanceOf(owner.address)).to.be.gt(standardBefore);

        // 买回特殊代币
        const specialBefore = await ctx.special.balanceOf(owner.address);
        await manager.swap(
          swapParams(ctx.poolAddr, owner.address, {
            zeroForOne: !ctx.specialIsToken0,
            sqrtPriceLimitX96: ctx.specialIsToken0 ? MAX_SQRT_RATIO_MINUS_ONE : MIN_SQRT_RATIO_PLUS_ONE,
          })
        );
        expect(await ctx.special.balanceOf(owner.address)).to.be.gt(specialBefore);
      });

      it("能正常 burn 和 collect", async function () {
        await manager.mint(mintParams(ctx.poolAddr, -600, 600, LIQUIDITY));
        const deposited = await ctx.special.balanceOf(ctx.poolAddr);
        const before = await ctx.special.balanceOf(owner.address);

        await ctx.pool.burn(-600, 600, LIQUIDITY);
        await ctx.pool.collect(owner.address, -600, 600, MAX_UINT128, MAX_UINT128);

        expect((await ctx.special.balanceOf(owner.address)) - before).to.be.closeTo(deposited, 1);
      });

      it("能正常 flash", async function () {
        await manager.mint(mintParams(ctx.poolAddr, -600, 600, LIQUIDITY));
        const Borrower = await ethers.getContractFactory("TestFlashBorrower");
        const borrower = await Borrower.deploy();
        await borrower.waitForDeployment();
        const borrowerAddr = await borrower.getAddress();
        await ctx.special.mint(borrowerAddr, ethers.parseEther("1"));
        await ctx.standard.mint(borrowerAddr, ethers.parseEther("1"));

        const amount = ethers.parseEther("1");
        const fee = (amount * 3n) / 1000n;
        const before = await ctx.special.balanceOf(ctx.poolAddr);
        await expect(borrower.flash(ctx.poolAddr, amount, amount, 0)).to.emit(ctx.pool, "Flash");
        expect(await ctx.special.balanceOf(ctx.poolAddr)).to.equal(before + fee);
      });
    });

    describe("失败时返回 false 的代币", function () {
      let ctx;

      beforeEach(async function () {
        ctx = await setupWith("TestFalseReturnERC20");
        await manager.mint(mintParams(ctx.poolAddr, -600, 600, LIQUIDITY));
      });

      it("collect 转账失败时回滚，可领取数量不会被白白扣掉", async function () {
        await ctx.pool.burn(-600, 600, LIQUIDITY);
        const key = positionKey(owner.address, -600, 600);
        const owedBefore = await ctx.pool.positions(key);
        expect(owedBefore.tokensOwed0).to.be.gt(0);

        await ctx.special.setPaused(true);
        await expect(ctx.pool.collect(owner.address, -600, 600, MAX_UINT128, MAX_UINT128)).to.be.reverted;

        const owedAfter = await ctx.pool.positions(key);
        expect(owedAfter.tokensOwed0).to.equal(owedBefore.tokensOwed0);
        expect(owedAfter.tokensOwed1).to.equal(owedBefore.tokensOwed1);

        // 恢复后可以正常领取
        await ctx.special.setPaused(false);
        const before = await ctx.special.balanceOf(owner.address);
        await ctx.pool.collect(owner.address, -600, 600, MAX_UINT128, MAX_UINT128);
        expect(await ctx.special.balanceOf(owner.address)).to.be.gt(before);
      });

      it("swap 的输出转账失败时回滚，用户不会付了钱却收不到币", async function () {
        const standardBefore = await ctx.standard.balanceOf(owner.address);
        const specialBefore = await ctx.special.balanceOf(owner.address);

        // 用标准代币买特殊代币，特殊代币的转出会失败
        await ctx.special.setPaused(true);
        await expect(
          manager.swap(
            swapParams(ctx.poolAddr, owner.address, {
              zeroForOne: !ctx.specialIsToken0,
              sqrtPriceLimitX96: ctx.specialIsToken0 ? MAX_SQRT_RATIO_MINUS_ONE : MIN_SQRT_RATIO_PLUS_ONE,
            })
          )
        ).to.be.reverted;

        expect(await ctx.standard.balanceOf(owner.address)).to.equal(standardBefore);
        expect(await ctx.special.balanceOf(owner.address)).to.equal(specialBefore);
      });

      it("付款方余额不足时 mint 回滚", async function () {
        // honest 授权了 Manager 但手里没有特殊代币
        await ctx.special.connect(honest).approve(managerAddr, ethers.MaxUint256);
        await ctx.standard.mint(honest.address, FUNDS);
        await ctx.standard.connect(honest).approve(managerAddr, ethers.MaxUint256);

        await expect(
          manager.connect(honest).mint(mintParams(ctx.poolAddr, -600, 600, LIQUIDITY))
        ).to.be.reverted;
        expect(await ctx.pool.liquidity()).to.equal(LIQUIDITY);
        expect(await ctx.standard.balanceOf(honest.address)).to.equal(FUNDS);
      });
    });
  });

  describe("没有活跃流动性时的闪电贷", function () {
    it("活跃流动性为 0 时 flash 被拒绝，手续费不会无主锁死", async function () {
      const [token0, token1] = await deployTokens(2);
      await fund([token0, token1], owner, managerAddr, FUNDS);
      const pool = await createPool(factory, token0, token1, 3000);
      const poolAddr = await pool.getAddress();

      // 只有当前价格之外的流动性：池子里有 token0 可借，但没有 LP 能分到手续费
      await manager.mint(mintParams(poolAddr, 600, 1200, ethers.parseEther("1000")));
      expect(await pool.liquidity()).to.equal(0);
      expect(await token0.balanceOf(poolAddr)).to.be.gt(ethers.parseEther("1"));

      const Borrower = await ethers.getContractFactory("TestFlashBorrower");
      const borrower = await Borrower.deploy();
      await borrower.waitForDeployment();
      await token0.mint(await borrower.getAddress(), ethers.parseEther("1"));

      await expect(borrower.flash(poolAddr, ethers.parseEther("1"), 0, 0)).to.be.revertedWith("L");
    });
  });

  describe("FlashArbitrage 部分成交", function () {
    it("买回时流动性不足、输入吃不完，整笔回滚且合约不留余额", async function () {
      const [token0, token1] = await deployTokens(2);
      await fund([token0, token1], owner, managerAddr, FUNDS);

      const lender = await createPool(factory, token0, token1, 500);
      const sellPool = await createPool(factory, token0, token1, 3000, encodeSqrtPrice(105, 100));
      // 买回的池子 token0 非常便宜，但只有一小段流动性：
      // 换来的 token1 用不完就把这段流动性买空了，剩下的 token1 没有对手盘
      const buyPool = await createPool(factory, token0, token1, 10000, encodeSqrtPrice(1, 2));

      await manager.mint(mintParams(await lender.getAddress(), -6000, 6000, ethers.parseEther("1000")));
      await manager.mint(mintParams(await sellPool.getAddress(), -6000, 6000, ethers.parseEther("1000")));
      await manager.mint(mintParams(await buyPool.getAddress(), -7000, -6800, ethers.parseEther("650")));

      const Arbitrage = await ethers.getContractFactory("FlashArbitrage");
      const arbitrage = await Arbitrage.deploy(await factory.getAddress());
      await arbitrage.waitForDeployment();
      const arbitrageAddr = await arbitrage.getAddress();

      // 即使只看借入的 token0 是“赚”的，也不允许把吃不完的 token1 留在合约里
      await expect(
        arbitrage.connect(attacker).arbitrage({
          lender: await lender.getAddress(),
          sellPool: await sellPool.getAddress(),
          buyPool: await buyPool.getAddress(),
          borrowToken: await token0.getAddress(),
          amount: ethers.parseEther("5"),
          minProfit: 0,
        })
      ).to.be.revertedWith("PF");

      expect(await token0.balanceOf(arbitrageAddr)).to.equal(0);
      expect(await token1.balanceOf(arbitrageAddr)).to.equal(0);
      expect(await token0.balanceOf(attacker.address)).to.equal(0);
    });
  });

  describe("Manager.swap 只接受精确输入", function () {
    it("精确输出（amountSpecified < 0）被拒绝，必须走带输入上限的 exactOutputSingle", async function () {
      const [token0, token1] = await deployTokens(2);
      await fund([token0, token1], owner, managerAddr, FUNDS);
      const pool = await createPool(factory, token0, token1, 3000);
      const poolAddr = await pool.getAddress();
      await manager.mint(mintParams(poolAddr, -600, 600, ethers.parseEther("1000")));

      await expect(
        manager.swap(swapParams(poolAddr, owner.address, { amountSpecified: -ethers.parseEther("1") }))
      ).to.be.revertedWith("AS");
      await expect(
        manager.swap(swapParams(poolAddr, owner.address, { amountSpecified: 0 }))
      ).to.be.revertedWith("AS");

      // 同样的需求走 exactOutputSingle，可以限制最多付出多少
      await expect(
        manager.exactOutputSingle({
          tokenIn: await token0.getAddress(),
          tokenOut: await token1.getAddress(),
          fee: 3000,
          recipient: owner.address,
          amountOut: ethers.parseEther("1"),
          amountInMaximum: ethers.parseEther("1.01"),
          sqrtPriceLimitX96: 0,
          deadline: NO_DEADLINE,
        })
      ).not.to.be.reverted;
    });
  });
});
