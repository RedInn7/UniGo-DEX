const { expect } = require("chai");
const { ethers } = require("hardhat");
const { time } = require("@nomicfoundation/hardhat-network-helpers");
const {
  NO_DEADLINE,
  MIN_SQRT_RATIO_PLUS_ONE,
  MAX_SQRT_RATIO_MINUS_ONE,
  encodeSqrtPrice,
  deployTokens,
  deployCore,
  createPool,
  fund,
} = require("./helpers");

/**
 * 价格预言机（TWAP）测试
 *
 * 测试覆盖：
 * 1. 恒定价格下 TWAP 等于现价
 * 2. 价格变动后 TWAP 介于新旧价格之间，且与手算一致
 * 3. 查询早于最老观测时 revert
 * 4. 扩容与环形覆盖
 *
 * 所有查询都通过 TestOracleReader 以交易方式执行，配合 setNextBlockTimestamp 精确控制时间
 */
describe("价格预言机 (TWAP)", function () {
  let factory;
  let manager;
  let token0;
  let token1;
  let pool;
  let poolAddr;
  let reader;
  let owner;

  const LIQUIDITY = ethers.parseEther("1000");

  // 向负无穷取整的除法
  function floorDiv(a, b) {
    const q = a / b;
    return a % b !== 0n && a < 0n !== b < 0n ? q - 1n : q;
  }

  // 在指定时间戳的区块里执行一笔交易
  async function at(timestamp, send) {
    await time.setNextBlockTimestamp(timestamp);
    return send();
  }

  // 在指定时间戳查询过去 window 秒的平均 tick
  async function consultAt(timestamp, window) {
    await at(timestamp, () => reader.consult(poolAddr, window));
    return reader.averageTick();
  }

  // 精确输入 swap，返回成交后的 tick
  async function swap(zeroForOne, amount) {
    await manager.swap({
      pool: poolAddr,
      zeroForOne,
      amountSpecified: amount,
      sqrtPriceLimitX96: zeroForOne ? MIN_SQRT_RATIO_PLUS_ONE : MAX_SQRT_RATIO_MINUS_ONE,
      recipient: owner.address,
      amountOutMinimum: 0,
      deadline: NO_DEADLINE,
    });
    return (await pool.slot0()).tick;
  }

  async function mint(tickLower, tickUpper, amount = LIQUIDITY) {
    return manager.mint({
      pool: poolAddr,
      tickLower,
      tickUpper,
      amount,
      amount0Min: 0,
      amount1Min: 0,
      deadline: NO_DEADLINE,
    });
  }

  // 创建池子并做好通用准备
  async function setup(sqrtPriceX96) {
    pool = await createPool(factory, token0, token1, 3000, sqrtPriceX96);
    poolAddr = await pool.getAddress();
  }

  beforeEach(async function () {
    [owner] = await ethers.getSigners();
    ({ factory, manager } = await deployCore());
    [token0, token1] = await deployTokens(2);
    await fund([token0, token1], owner, await manager.getAddress());

    const Reader = await ethers.getContractFactory("TestOracleReader");
    reader = await Reader.deploy();
    await reader.waitForDeployment();
  });

  describe("初始化", function () {
    it("初始化时写入第一条观测，容量为 1", async function () {
      await setup();
      const slot0 = await pool.slot0();
      expect(slot0.observationIndex).to.equal(0);
      expect(slot0.observationCardinality).to.equal(1);
      expect(slot0.observationCardinalityNext).to.equal(1);

      const observation = await pool.observations(0);
      expect(observation.initialized).to.equal(true);
      expect(observation.tickCumulative).to.equal(0);
      expect(observation.blockTimestamp).to.equal(await time.latest());
    });

    it("初始化时触发 Initialize 事件", async function () {
      const addr0 = await token0.getAddress();
      const addr1 = await token1.getAddress();
      await factory.createPool(addr0, addr1, 3000);
      const created = await ethers.getContractAt("UniGoPool", await factory.getPool(addr0, addr1, 3000));

      const sqrtPriceX96 = encodeSqrtPrice(2, 1);
      await expect(created.initialize(sqrtPriceX96))
        .to.emit(created, "Initialize")
        .withArgs(sqrtPriceX96, 6931);
    });
  });

  describe("恒定价格", function () {
    it("TWAP 等于现价（tick 0）", async function () {
      await setup();
      await mint(-6000, 6000);
      const start = await time.latest();

      expect(await consultAt(start + 1000, 600)).to.equal(0);
    });

    it("TWAP 等于现价（非零 tick），不同窗口结果相同", async function () {
      // 价格 2:1 对应 tick 6931
      await setup(encodeSqrtPrice(2, 1));
      await mint(0, 12000);
      const start = await time.latest();
      const currentTick = (await pool.slot0()).tick;
      expect(currentTick).to.equal(6931);

      expect(await consultAt(start + 1000, 600)).to.equal(currentTick);
      expect(await consultAt(start + 2000, 1)).to.equal(currentTick);
      expect(await consultAt(start + 3000, 2500)).to.equal(currentTick);
    });

    it("每单位流动性秒数按 窗口 / 流动性 累计", async function () {
      await setup();
      await mint(-6000, 6000);
      const start = await time.latest();

      await consultAt(start + 1000, 600);
      expect(await reader.secondsPerLiquidityDeltaX128()).to.equal((600n << 128n) / LIQUIDITY);
    });
  });

  describe("价格变动", function () {
    let start;

    beforeEach(async function () {
      await setup();
      await pool.increaseObservationCardinalityNext(10);
      await mint(-6000, 6000);
      start = await time.latest();
    });

    it("TWAP 介于新旧价格之间，且与手算一致", async function () {
      // start+1000：价格下跌，tick 从 0 变为 tick1
      await time.setNextBlockTimestamp(start + 1000);
      const tick1 = await swap(true, ethers.parseEther("50"));
      expect(tick1).to.be.lt(-100);

      // start+1300 回看 600 秒：前 300 秒 tick=0，后 300 秒 tick=tick1
      const twap = await consultAt(start + 1300, 600);
      expect(twap).to.equal(floorDiv(tick1 * 300n, 600n));
      expect(twap).to.be.lt(0);
      expect(twap).to.be.gt(tick1);

      // 窗口完全落在新价格区间内时，TWAP 等于新价格
      expect(await consultAt(start + 1400, 300)).to.equal(tick1);
    });

    it("多次变动后按各段持续时间加权", async function () {
      await time.setNextBlockTimestamp(start + 1000);
      const tick1 = await swap(true, ethers.parseEther("50"));
      // start+1300：价格反向上涨到 tick2
      await time.setNextBlockTimestamp(start + 1300);
      const tick2 = await swap(false, ethers.parseEther("120"));
      expect(tick2).to.be.gt(100);

      // start+1400 回看 600 秒：200 秒 tick=0，300 秒 tick=tick1，100 秒 tick=tick2
      const twap = await consultAt(start + 1400, 600);
      expect(twap).to.equal(floorDiv(tick1 * 300n + tick2 * 100n, 600n));
      expect(twap).to.be.gt(tick1);
      expect(twap).to.be.lt(tick2);
    });

    it("同一区块时间戳内只写入一次观测", async function () {
      await time.setNextBlockTimestamp(start + 1000);
      await swap(true, ethers.parseEther("50"));
      const indexBefore = (await pool.slot0()).observationIndex;

      // 关闭自动出块，把两笔 swap 打进同一个区块
      await ethers.provider.send("evm_setAutomine", [false]);
      await swap(false, ethers.parseEther("10"));
      await swap(false, ethers.parseEther("10"));
      await ethers.provider.send("evm_mine", []);
      await ethers.provider.send("evm_setAutomine", [true]);

      expect((await pool.slot0()).observationIndex).to.equal(indexBefore + 1n);
    });
  });

  describe("查询范围", function () {
    it("查询早于最老观测时 revert", async function () {
      await setup();
      await mint(-6000, 6000);
      const start = await time.latest();

      // 容量为 1：swap 写入的观测覆盖掉唯一的槽位，更早的历史丢失
      await time.setNextBlockTimestamp(start + 1000);
      await swap(true, ethers.parseEther("50"));

      await time.setNextBlockTimestamp(start + 1100);
      await expect(reader.consult(poolAddr, 101)).to.be.revertedWith("OLD");
      // 正好回看到最老的观测是允许的
      expect(await consultAt(start + 1200, 200)).to.equal((await pool.slot0()).tick);
    });

    it("未初始化的池子不能查询", async function () {
      const addr0 = await token0.getAddress();
      const addr1 = await token1.getAddress();
      await factory.createPool(addr0, addr1, 500);
      const empty = await ethers.getContractAt("UniGoPool", await factory.getPool(addr0, addr1, 500));

      await expect(empty.observe([0])).to.be.revertedWith("I");
    });
  });

  describe("扩容与环形覆盖", function () {
    it("扩容触发事件，容量只增不减", async function () {
      await setup();
      await expect(pool.increaseObservationCardinalityNext(5))
        .to.emit(pool, "IncreaseObservationCardinalityNext")
        .withArgs(1, 5);
      expect((await pool.slot0()).observationCardinalityNext).to.equal(5);

      // 缩容请求被忽略，不触发事件
      await expect(pool.increaseObservationCardinalityNext(3)).not.to.emit(
        pool,
        "IncreaseObservationCardinalityNext"
      );
      expect((await pool.slot0()).observationCardinalityNext).to.equal(5);
      // 预留容量在下一次写入观测时才生效
      expect((await pool.slot0()).observationCardinality).to.equal(1);
    });

    it("写满后覆盖最老的观测，剩余历史仍可正确查询", async function () {
      await setup();
      // 容量为 1 时 mint 写入的观测覆盖 0 号槽位
      await mint(-6000, 6000);
      await pool.increaseObservationCardinalityNext(3);
      const start = await time.latest();

      // 4 次改变 tick 的 swap，依次写入 1、2、0、1 号槽位
      const ticks = [];
      const amounts = [
        [true, "50"],
        [false, "120"],
        [true, "150"],
        [false, "40"],
      ];
      for (let i = 0; i < amounts.length; i++) {
        await time.setNextBlockTimestamp(start + 100 * (i + 1));
        ticks.push(await swap(amounts[i][0], ethers.parseEther(amounts[i][1])));
      }

      const slot0 = await pool.slot0();
      expect(slot0.observationCardinality).to.equal(3);
      expect(slot0.observationIndex).to.equal(1);
      // 0 号槽位已被第 3 次 swap 覆盖；最老的观测在 2 号槽位（第 2 次 swap）
      expect((await pool.observations(0)).blockTimestamp).to.equal(start + 300);
      expect((await pool.observations(1)).blockTimestamp).to.equal(start + 400);
      expect((await pool.observations(2)).blockTimestamp).to.equal(start + 200);

      // start+500 回看 300 秒，正好回到最老的观测：三段各 100 秒
      const twap = await consultAt(start + 500, 300);
      expect(twap).to.equal(floorDiv(ticks[1] + ticks[2] + ticks[3], 3n));

      // start+600 回看 350 秒：目标时刻 start+250 落在 2 号和 0 号观测之间，线性插值
      const interpolated = await consultAt(start + 600, 350);
      expect(interpolated).to.equal(
        floorDiv(ticks[1] * 50n + ticks[2] * 100n + ticks[3] * 200n, 350n)
      );

      // 再往前一秒就超出了保留的历史
      await time.setNextBlockTimestamp(start + 700);
      await expect(reader.consult(poolAddr, 501)).to.be.revertedWith("OLD");
    });
  });
});
