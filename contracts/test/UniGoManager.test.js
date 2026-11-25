const { expect } = require("chai");
const { ethers } = require("hardhat");
const {
  NO_DEADLINE,
  MIN_SQRT_RATIO_PLUS_ONE,
  deployTokens,
  deployCore,
  createPool,
  fund,
  encodePath,
} = require("./helpers");

/**
 * UniGoManager / UniGoQuoter 路由测试
 *
 * 测试覆盖：
 * 1. 多跳精确输入（exactInput）及其滑点、期限、路径校验
 * 2. 单池精确输出（exactOutputSingle）及输入上限
 * 3. 多跳报价（quoteExactInput）与实际成交一致
 * 4. 伪造 Pool 无法通过 Manager 划走用户的 token
 */
describe("UniGoManager 路由", function () {
  let factory;
  let manager;
  let quoter;
  let tokenA;
  let tokenB;
  let tokenC;
  let addrA;
  let addrB;
  let addrC;
  let poolAB; // 0.3%
  let poolBC; // 0.05%
  let owner;
  let user;
  let managerAddr;

  const LIQUIDITY = ethers.parseEther("1000");
  const AMOUNT_IN = ethers.parseEther("1");

  beforeEach(async function () {
    [owner, user] = await ethers.getSigners();
    ({ factory, manager, quoter } = await deployCore());
    managerAddr = await manager.getAddress();

    [tokenA, tokenB, tokenC] = await deployTokens(3);
    addrA = await tokenA.getAddress();
    addrB = await tokenB.getAddress();
    addrC = await tokenC.getAddress();
    await fund([tokenA, tokenB, tokenC], owner, managerAddr);

    poolAB = await createPool(factory, tokenA, tokenB, 3000);
    poolBC = await createPool(factory, tokenB, tokenC, 500);

    for (const [pool, lower, upper] of [[poolAB, -600, 600], [poolBC, -600, 600]]) {
      await manager.mint({
        pool: await pool.getAddress(),
        tickLower: lower,
        tickUpper: upper,
        amount: LIQUIDITY,
        amount0Min: 0,
        amount1Min: 0,
        deadline: NO_DEADLINE,
      });
    }
  });

  // 构造 exactInput 参数，默认路径 A -> B -> C
  function exactInputParams(overrides = {}) {
    return {
      path: encodePath([addrA, addrB, addrC], [3000, 500]),
      recipient: user.address,
      amountIn: AMOUNT_IN,
      amountOutMinimum: 0,
      deadline: NO_DEADLINE,
      ...overrides,
    };
  }

  describe("多跳精确输入 (exactInput)", function () {
    it("两跳成交：用户只付 A、只收 C，中间 token 不滞留在 Manager", async function () {
      const expectedOut = await manager.exactInput.staticCall(exactInputParams());
      const ownerA = await tokenA.balanceOf(owner.address);
      const ownerB = await tokenB.balanceOf(owner.address);

      await manager.exactInput(exactInputParams());

      expect(await tokenA.balanceOf(owner.address)).to.equal(ownerA - AMOUNT_IN);
      expect(await tokenB.balanceOf(owner.address)).to.equal(ownerB);
      expect(await tokenC.balanceOf(user.address)).to.equal(expectedOut);
      // 两跳各扣一次手续费（0.3% + 0.05%），再加价格冲击，输出略小于输入
      expect(expectedOut).to.be.lt(AMOUNT_IN);
      expect(expectedOut).to.be.gt((AMOUNT_IN * 99n) / 100n);

      for (const token of [tokenA, tokenB, tokenC]) {
        expect(await token.balanceOf(managerAddr)).to.equal(0);
      }
    });

    it("反向路径 C -> B -> A 同样成交", async function () {
      const params = exactInputParams({ path: encodePath([addrC, addrB, addrA], [500, 3000]) });
      const expectedOut = await manager.exactInput.staticCall(params);

      await manager.exactInput(params);
      expect(await tokenA.balanceOf(user.address)).to.equal(expectedOut);
    });

    it("单跳路径等价于单池 swap", async function () {
      const viaPath = await manager.exactInput.staticCall(
        exactInputParams({ path: encodePath([addrA, addrB], [3000]) })
      );
      const [, amount1] = await manager.swap.staticCall({
        pool: await poolAB.getAddress(),
        zeroForOne: true,
        amountSpecified: AMOUNT_IN,
        sqrtPriceLimitX96: MIN_SQRT_RATIO_PLUS_ONE,
        recipient: user.address,
        amountOutMinimum: 0,
        deadline: NO_DEADLINE,
      });
      expect(viaPath).to.equal(-amount1);
    });

    it("最终输出低于 amountOutMinimum 应 revert", async function () {
      const expectedOut = await manager.exactInput.staticCall(exactInputParams());

      await expect(
        manager.exactInput(exactInputParams({ amountOutMinimum: expectedOut + 1n }))
      ).to.be.revertedWith("SLP");
      await expect(manager.exactInput(exactInputParams({ amountOutMinimum: expectedOut }))).not.to.be.reverted;
    });

    it("过期应 revert", async function () {
      await expect(manager.exactInput(exactInputParams({ deadline: 1 }))).to.be.revertedWith("EXP");
    });

    it("路径长度不合法应 revert", async function () {
      const path = encodePath([addrA, addrB, addrC], [3000, 500]);
      await expect(
        manager.exactInput(exactInputParams({ path: path.slice(0, -2) }))
      ).to.be.revertedWith("PATH");
      await expect(manager.exactInput(exactInputParams({ path: addrA }))).to.be.revertedWith("PATH");
    });

    it("路径中的池子不存在应 revert", async function () {
      await expect(
        manager.exactInput(exactInputParams({ path: encodePath([addrA, addrC], [3000]) }))
      ).to.be.revertedWith("POOL");
    });

    it("流动性不足、输入吃不完时整笔 revert", async function () {
      await expect(
        manager.exactInput(exactInputParams({ amountIn: ethers.parseEther("100000") }))
      ).to.be.revertedWith("PF");
    });
  });

  describe("单池精确输出 (exactOutputSingle)", function () {
    const AMOUNT_OUT = ethers.parseEther("1");

    function exactOutputParams(overrides = {}) {
      return {
        tokenIn: addrA,
        tokenOut: addrB,
        fee: 3000,
        recipient: user.address,
        amountOut: AMOUNT_OUT,
        amountInMaximum: ethers.MaxUint256,
        sqrtPriceLimitX96: 0,
        deadline: NO_DEADLINE,
        ...overrides,
      };
    }

    it("拿到恰好 amountOut，付出的数量等于返回值", async function () {
      const amountIn = await manager.exactOutputSingle.staticCall(exactOutputParams());
      const ownerA = await tokenA.balanceOf(owner.address);

      await manager.exactOutputSingle(exactOutputParams());

      expect(await tokenB.balanceOf(user.address)).to.equal(AMOUNT_OUT);
      expect(await tokenA.balanceOf(owner.address)).to.equal(ownerA - amountIn);
      // 要拿 1 个，至少得付 1 个再加 0.3% 手续费
      expect(amountIn).to.be.gt((AMOUNT_OUT * 1003n) / 1000n);
    });

    it("token1 -> token0 方向同样成立", async function () {
      const params = exactOutputParams({ tokenIn: addrB, tokenOut: addrA });
      const amountIn = await manager.exactOutputSingle.staticCall(params);
      const ownerB = await tokenB.balanceOf(owner.address);

      await manager.exactOutputSingle(params);

      expect(await tokenA.balanceOf(user.address)).to.equal(AMOUNT_OUT);
      expect(await tokenB.balanceOf(owner.address)).to.equal(ownerB - amountIn);
    });

    it("输入超过 amountInMaximum 应 revert", async function () {
      const amountIn = await manager.exactOutputSingle.staticCall(exactOutputParams());

      await expect(
        manager.exactOutputSingle(exactOutputParams({ amountInMaximum: amountIn - 1n }))
      ).to.be.revertedWith("TMI");
      await expect(
        manager.exactOutputSingle(exactOutputParams({ amountInMaximum: amountIn }))
      ).not.to.be.reverted;
    });

    it("流动性不足、拿不满时应 revert", async function () {
      await expect(
        manager.exactOutputSingle(exactOutputParams({ amountOut: ethers.parseEther("100000") }))
      ).to.be.revertedWith("PF");
    });

    it("过期应 revert", async function () {
      await expect(
        manager.exactOutputSingle(exactOutputParams({ deadline: 1 }))
      ).to.be.revertedWith("EXP");
    });
  });

  describe("多跳报价 (quoteExactInput)", function () {
    it("报价与实际成交一致", async function () {
      const path = encodePath([addrA, addrB, addrC], [3000, 500]);
      const quoted = await quoter.quoteExactInput.staticCall(path, AMOUNT_IN);

      await manager.exactInput(exactInputParams({ amountOutMinimum: quoted }));
      expect(await tokenC.balanceOf(user.address)).to.equal(quoted);
    });

    it("报价不改变池子状态", async function () {
      const before = await poolAB.slot0();
      await quoter.quoteExactInput(encodePath([addrA, addrB, addrC], [3000, 500]), AMOUNT_IN);
      expect((await poolAB.slot0()).sqrtPriceX96).to.equal(before.sqrtPriceX96);
    });

    it("池子不存在或路径不合法应 revert", async function () {
      await expect(
        quoter.quoteExactInput.staticCall(encodePath([addrA, addrC], [3000]), AMOUNT_IN)
      ).to.be.revertedWith("POOL");
      await expect(quoter.quoteExactInput.staticCall(addrA, AMOUNT_IN)).to.be.revertedWith("PATH");
    });
  });

  describe("Pool 真伪校验", function () {
    let fakePool;
    let fakeAddr;

    beforeEach(async function () {
      // 伪造的池子冒充 A/B 0.3% 池的 token 和费率
      const FakePool = await ethers.getContractFactory("TestFakePool");
      fakePool = await FakePool.deploy(addrA, addrB, 3000);
      await fakePool.waitForDeployment();
      fakeAddr = await fakePool.getAddress();
    });

    it("伪造的 Pool 不能作为 swap / mint 的目标", async function () {
      await expect(
        manager.swap({
          pool: fakeAddr,
          zeroForOne: true,
          amountSpecified: AMOUNT_IN,
          sqrtPriceLimitX96: MIN_SQRT_RATIO_PLUS_ONE,
          recipient: owner.address,
          amountOutMinimum: 0,
          deadline: NO_DEADLINE,
        })
      ).to.be.revertedWith("POOL");

      await expect(
        manager.mint({
          pool: fakeAddr,
          tickLower: -600,
          tickUpper: 600,
          amount: LIQUIDITY,
          amount0Min: 0,
          amount1Min: 0,
          deadline: NO_DEADLINE,
        })
      ).to.be.revertedWith("POOL");
    });

    it("伪造的 Pool 直接调用回调不能划走已授权用户的 token", async function () {
      const ownerA = await tokenA.balanceOf(owner.address);

      await expect(
        fakePool.attack(managerAddr, owner.address, AMOUNT_IN)
      ).to.be.revertedWith("CB");
      expect(await tokenA.balanceOf(owner.address)).to.equal(ownerA);
    });
  });
});
