const { expect } = require("chai");
const { ethers } = require("hardhat");
const {
  NO_DEADLINE,
  INIT_SQRT_PRICE,
  encodeSqrtPrice,
  deployTokens,
  deployCore,
  createPool,
  fund,
} = require("./helpers");

/**
 * 闪电贷套利示例测试
 *
 * 场景：同一交易对的三个费率池
 * - 0.05% 池：只负责放贷
 * - 0.3%  池：token0 价格 1.05（偏贵）
 * - 1%    池：token0 价格 1.00（偏便宜）
 * 套利者没有任何本金，借入 token0 在贵的池子卖出、在便宜的池子买回，赚取价差
 */
describe("闪电贷套利 (FlashArbitrage)", function () {
  let factory;
  let manager;
  let token0;
  let token1;
  let lender;
  let expensivePool;
  let cheapPool;
  let arbitrage;
  let arbitrageAddr;
  let owner;
  let trader;

  const LIQUIDITY = ethers.parseEther("1000");
  const BORROW = ethers.parseEther("5");

  async function addLiquidity(pool, tickLower, tickUpper) {
    await manager.mint({
      pool: await pool.getAddress(),
      tickLower,
      tickUpper,
      amount: LIQUIDITY,
      amount0Min: 0,
      amount1Min: 0,
      deadline: NO_DEADLINE,
    });
  }

  // 部署三个池子，expensivePrice 为 0.3% 池的初始价格
  async function setup(expensivePrice) {
    lender = await createPool(factory, token0, token1, 500);
    expensivePool = await createPool(factory, token0, token1, 3000, expensivePrice);
    cheapPool = await createPool(factory, token0, token1, 10000);

    await addLiquidity(lender, -6000, 6000);
    await addLiquidity(expensivePool, -6000, 6000);
    await addLiquidity(cheapPool, -6000, 6000);
  }

  async function params(overrides = {}) {
    return {
      lender: await lender.getAddress(),
      sellPool: await expensivePool.getAddress(),
      buyPool: await cheapPool.getAddress(),
      borrowToken: await token0.getAddress(),
      amount: BORROW,
      minProfit: 0,
      ...overrides,
    };
  }

  // 读取各池子和发起人的余额
  async function snapshot() {
    const result = { pools: [] };
    for (const pool of [lender, expensivePool, cheapPool]) {
      const addr = await pool.getAddress();
      result.pools.push({
        balance0: await token0.balanceOf(addr),
        balance1: await token1.balanceOf(addr),
        sqrtPriceX96: (await pool.slot0()).sqrtPriceX96,
      });
    }
    result.trader0 = await token0.balanceOf(trader.address);
    result.trader1 = await token1.balanceOf(trader.address);
    return result;
  }

  beforeEach(async function () {
    [owner, trader] = await ethers.getSigners();
    ({ factory, manager } = await deployCore());
    [token0, token1] = await deployTokens(2);
    await fund([token0, token1], owner, await manager.getAddress());

    const Arbitrage = await ethers.getContractFactory("FlashArbitrage");
    arbitrage = await Arbitrage.deploy(await factory.getAddress());
    await arbitrage.waitForDeployment();
    arbitrageAddr = await arbitrage.getAddress();
  });

  describe("存在价差", function () {
    beforeEach(async function () {
      await setup(encodeSqrtPrice(105, 100));
    });

    it("发起人零本金净赚，利润与事件一致", async function () {
      const before = await snapshot();
      expect(before.trader0).to.equal(0);
      expect(before.trader1).to.equal(0);

      const expectedProfit = await arbitrage.connect(trader).arbitrage.staticCall(await params());
      await expect(arbitrage.connect(trader).arbitrage(await params()))
        .to.emit(arbitrage, "Arbitrage")
        .withArgs(trader.address, await token0.getAddress(), BORROW, expectedProfit);

      const after = await snapshot();
      expect(after.trader0).to.equal(expectedProfit);
      expect(after.trader1).to.equal(0);
      // 价差 5%，扣除 0.3% + 1% 的 swap 手续费、0.05% 的闪电贷手续费和价格冲击后仍有利润
      expect(expectedProfit).to.be.gt((BORROW * 2n) / 100n);
      expect(expectedProfit).to.be.lt((BORROW * 5n) / 100n);

      // 套利合约不留任何余额
      expect(await token0.balanceOf(arbitrageAddr)).to.equal(0);
      expect(await token1.balanceOf(arbitrageAddr)).to.equal(0);
    });

    it("两个池子的价格收敛", async function () {
      const before = await snapshot();
      await arbitrage.connect(trader).arbitrage(await params());
      const after = await snapshot();

      const [, expensiveBefore, cheapBefore] = before.pools;
      const [, expensiveAfter, cheapAfter] = after.pools;
      // 贵的池子被卖出 token0，价格下降；便宜的池子被买入 token0，价格上升
      expect(expensiveAfter.sqrtPriceX96).to.be.lt(expensiveBefore.sqrtPriceX96);
      expect(cheapAfter.sqrtPriceX96).to.be.gt(cheapBefore.sqrtPriceX96);
      // 价差缩小但没有反转
      expect(expensiveAfter.sqrtPriceX96).to.be.gt(cheapAfter.sqrtPriceX96);
      expect(expensiveAfter.sqrtPriceX96 - cheapAfter.sqrtPriceX96).to.be.lt(
        expensiveBefore.sqrtPriceX96 - cheapBefore.sqrtPriceX96
      );
    });

    it("放贷池只赚不亏，token 总量守恒", async function () {
      const before = await snapshot();
      await arbitrage.connect(trader).arbitrage(await params());
      const after = await snapshot();

      // 放贷池收回本金并多出 0.05% 的手续费
      const flashFee = (BORROW * 500n) / 1000000n;
      expect(after.pools[0].balance0).to.equal(before.pools[0].balance0 + flashFee);
      expect(after.pools[0].balance1).to.equal(before.pools[0].balance1);
      expect(after.pools[0].sqrtPriceX96).to.equal(before.pools[0].sqrtPriceX96);

      // 贵的池子收到全部借款，便宜的池子收到全部换出的 token1
      expect(after.pools[1].balance0).to.equal(before.pools[1].balance0 + BORROW);
      const token1Moved = before.pools[1].balance1 - after.pools[1].balance1;
      expect(after.pools[2].balance1).to.equal(before.pools[2].balance1 + token1Moved);

      // 没有 token 凭空产生或消失：发起人的利润全部来自池子之间的价差
      const sum = (s, key) => s.pools.reduce((total, p) => total + p[key], 0n);
      expect(sum(after, "balance0") + after.trader0).to.equal(sum(before, "balance0"));
      expect(sum(after, "balance1")).to.equal(sum(before, "balance1"));
    });

    it("利润低于 minProfit 时整笔 revert", async function () {
      const profit = await arbitrage.connect(trader).arbitrage.staticCall(await params());

      await expect(
        arbitrage.connect(trader).arbitrage(await params({ minProfit: profit + 1n }))
      ).to.be.revertedWith("NP");
      await expect(
        arbitrage.connect(trader).arbitrage(await params({ minProfit: profit }))
      ).not.to.be.reverted;
    });

    it("买卖方向弄反时无利可图，整笔 revert", async function () {
      await expect(
        arbitrage.connect(trader).arbitrage(
          await params({
            sellPool: await cheapPool.getAddress(),
            buyPool: await expensivePool.getAddress(),
          })
        )
      ).to.be.revertedWith("NP");
    });

    it("借入 token1 反向套利同样获利", async function () {
      // token1 在 1% 池更贵（token0 更便宜），所以在 1% 池卖出 token1、在 0.3% 池买回
      const reversed = await params({
        sellPool: await cheapPool.getAddress(),
        buyPool: await expensivePool.getAddress(),
        borrowToken: await token1.getAddress(),
      });
      await arbitrage.connect(trader).arbitrage(reversed);

      expect(await token1.balanceOf(trader.address)).to.be.gt(0);
      expect(await token0.balanceOf(trader.address)).to.equal(0);
    });
  });

  describe("没有价差", function () {
    it("价格一致时无利可图，整笔 revert 且状态不变", async function () {
      await setup(INIT_SQRT_PRICE);
      const before = await snapshot();

      await expect(arbitrage.connect(trader).arbitrage(await params())).to.be.revertedWith("NP");

      expect(await snapshot()).to.deep.equal(before);
    });
  });

  describe("参数与权限校验", function () {
    beforeEach(async function () {
      await setup(encodeSqrtPrice(105, 100));
    });

    it("放贷池不能同时是买卖的池子", async function () {
      await expect(
        arbitrage.connect(trader).arbitrage(await params({ sellPool: await lender.getAddress() }))
      ).to.be.revertedWith("LP");
    });

    it("非 Factory 创建的池子被拒绝", async function () {
      const FakePool = await ethers.getContractFactory("TestFakePool");
      const fake = await FakePool.deploy(await token0.getAddress(), await token1.getAddress(), 3000);
      await fake.waitForDeployment();

      await expect(
        arbitrage.connect(trader).arbitrage(await params({ sellPool: await fake.getAddress() }))
      ).to.be.revertedWith("POOL");
    });

    it("套利过程之外不接受任何回调", async function () {
      await expect(arbitrage.connect(trader).uniGoFlashCallback(0, 0, "0x")).to.be.revertedWith("CB");
      await expect(arbitrage.connect(trader).uniGoSwapCallback(1, 0, "0x")).to.be.revertedWith("CB");
    });
  });
});
