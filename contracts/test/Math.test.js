const { expect } = require("chai");
const { ethers } = require("hardhat");
const { sqrt } = require("./helpers");

/**
 * TickMath / BitMath 对照测试
 *
 * 用两种独立的参照确认常数表没有抄错：
 * 1. Uniswap V3 的已知值（边界 tick 的 sqrtPriceX96）
 * 2. 用高精度定点数在测试里重新计算 sqrt(1.0001^tick) * 2^96，逐个 tick 比对
 */
describe("数学库对照", function () {
  let math;

  const MIN_TICK = -887272;
  const MAX_TICK = 887272;
  const MIN_SQRT_RATIO = 4295128739n;
  const MAX_SQRT_RATIO = 1461446703485210103287273052203988822378723970342n;

  // 高精度参照实现：300 位小数的定点数，对 sqrt(1.0001) 做快速幂
  const PRECISION = 300n;
  const ONE = 1n << PRECISION;
  const SQRT_BASE = sqrt((10001n << (2n * PRECISION)) / 10000n);

  function referenceSqrtRatio(tick) {
    let exponent = BigInt(tick < 0 ? -tick : tick);
    let base = SQRT_BASE;
    let result = ONE;
    while (exponent > 0n) {
      if (exponent & 1n) result = (result * base) >> PRECISION;
      base = (base * base) >> PRECISION;
      exponent >>= 1n;
    }
    if (tick < 0) result = (ONE * ONE) / result;
    return result >> (PRECISION - 96n);
  }

  // 合约结果与参照值的允许误差：绝对 1 个单位（最终取整）加上相对 1e-15（查表累积的截断）
  function expectClose(actual, expected, label) {
    const diff = actual > expected ? actual - expected : expected - actual;
    expect(diff <= 1n + expected / 10n ** 15n, `${label}: 合约 ${actual}，参照 ${expected}`).to.equal(true);
  }

  before(async function () {
    const Harness = await ethers.getContractFactory("TestMathHarness");
    math = await Harness.deploy();
    await math.waitForDeployment();
  });

  describe("TickMath.getSqrtRatioAtTick", function () {
    it("边界值与 Uniswap V3 一致", async function () {
      expect(await math.getSqrtRatioAtTick(MIN_TICK)).to.equal(MIN_SQRT_RATIO);
      expect(await math.getSqrtRatioAtTick(MAX_TICK)).to.equal(MAX_SQRT_RATIO);
      expect(await math.getSqrtRatioAtTick(MIN_TICK + 1)).to.equal(4295343490n);
      expect(await math.getSqrtRatioAtTick(MAX_TICK - 1)).to.equal(
        1461373636630004318706518188784493106690254656249n
      );
      expect(await math.getSqrtRatioAtTick(0)).to.equal(2n ** 96n);
    });

    it("超出范围的 tick 被拒绝", async function () {
      await expect(math.getSqrtRatioAtTick(MIN_TICK - 1)).to.be.revertedWith("T");
      await expect(math.getSqrtRatioAtTick(MAX_TICK + 1)).to.be.revertedWith("T");
    });

    it("常数表的每一位都与高精度计算一致（tick = ±2^i）", async function () {
      // 每个 2 的幂只用到常数表里的一项，任何一项抄错都会在这里暴露
      for (let i = 0; i < 20; i++) {
        const tick = 1 << i;
        if (tick > MAX_TICK) break;
        expectClose(await math.getSqrtRatioAtTick(-tick), referenceSqrtRatio(-tick), `tick -2^${i}`);
        expectClose(await math.getSqrtRatioAtTick(tick), referenceSqrtRatio(tick), `tick 2^${i}`);
      }
    });

    it("一批中间 tick 与高精度计算一致", async function () {
      const ticks = [
        MIN_TICK, -800000, -500000, -250000, -150000, -100000, -50000, -6932, -600, -60, -1,
        1, 60, 600, 6931, 50000, 100000, 150000, 250000, 500000, 738203, MAX_TICK,
      ];
      for (const tick of ticks) {
        expectClose(await math.getSqrtRatioAtTick(tick), referenceSqrtRatio(tick), `tick ${tick}`);
      }
    });

    it("随 tick 严格单调递增", async function () {
      let previous = 0n;
      for (let tick = MIN_TICK; tick <= MAX_TICK; tick += 44363) {
        const ratio = await math.getSqrtRatioAtTick(tick);
        expect(ratio, `tick ${tick}`).to.be.gt(previous);
        previous = ratio;
      }
    });
  });

  describe("TickMath.getTickAtSqrtRatio", function () {
    it("边界值", async function () {
      expect(await math.getTickAtSqrtRatio(MIN_SQRT_RATIO)).to.equal(MIN_TICK);
      expect(await math.getTickAtSqrtRatio(MAX_SQRT_RATIO - 1n)).to.equal(MAX_TICK - 1);
      expect(await math.getTickAtSqrtRatio(4295343490n)).to.equal(MIN_TICK + 1);
      expect(await math.getTickAtSqrtRatio(2n ** 96n)).to.equal(0);
      expect(await math.getTickAtSqrtRatio(2n ** 96n - 1n)).to.equal(-1);
    });

    it("范围之外的价格被拒绝", async function () {
      await expect(math.getTickAtSqrtRatio(MIN_SQRT_RATIO - 1n)).to.be.revertedWith("R");
      await expect(math.getTickAtSqrtRatio(MAX_SQRT_RATIO)).to.be.revertedWith("R");
    });

    it("与 getSqrtRatioAtTick 往返一致，且在 tick 边界两侧取值正确", async function () {
      const ticks = [
        MIN_TICK, MIN_TICK + 1, -887220, -500000, -123456, -6932, -6931, -600, -61, -60, -59, -2, -1,
        0, 1, 2, 59, 60, 61, 600, 6931, 6932, 123456, 500000, 887220, MAX_TICK - 1,
      ];
      for (const tick of ticks) {
        const ratio = await math.getSqrtRatioAtTick(tick);
        // 恰好在 tick 的价格上：返回该 tick
        expect(await math.getTickAtSqrtRatio(ratio), `tick ${tick}`).to.equal(tick);
        // 比该价格低 1 个单位：落到前一个 tick
        if (tick > MIN_TICK) {
          expect(await math.getTickAtSqrtRatio(ratio - 1n), `tick ${tick} 下方`).to.equal(tick - 1);
        }
        // 下一个 tick 的价格减 1：仍属于当前 tick
        const next = await math.getSqrtRatioAtTick(tick + 1);
        if (next - 1n < MAX_SQRT_RATIO) {
          expect(await math.getTickAtSqrtRatio(next - 1n), `tick ${tick} 上沿`).to.equal(tick);
        }
      }
    });

    it("任意价格都满足 ratio(tick) <= price < ratio(tick + 1)", async function () {
      // 在整个价格范围内按几何级数取样
      for (let price = MIN_SQRT_RATIO; price < MAX_SQRT_RATIO; price = (price * 1337n) / 100n + 12345n) {
        const tick = await math.getTickAtSqrtRatio(price);
        expect(await math.getSqrtRatioAtTick(tick), `price ${price}`).to.be.lte(price);
        expect(await math.getSqrtRatioAtTick(tick + 1n), `price ${price}`).to.be.gt(price);
      }
    });
  });

  describe("BitMath", function () {
    // 参照实现
    const msb = (x) => BigInt(x.toString(2).length - 1);
    const lsb = (x) => {
      let position = 0n;
      while (((x >> position) & 1n) === 0n) position++;
      return position;
    };

    it("0 没有有效位，直接 revert", async function () {
      await expect(math.mostSignificantBit(0)).to.be.reverted;
      await expect(math.leastSignificantBit(0)).to.be.reverted;
    });

    it("256 个单比特输入逐一正确", async function () {
      for (let i = 0n; i < 256n; i++) {
        expect(await math.mostSignificantBit(1n << i), `msb 2^${i}`).to.equal(i);
        expect(await math.leastSignificantBit(1n << i), `lsb 2^${i}`).to.equal(i);
      }
    });

    it("2^i - 1、2^i + 1 和全 1 等边界输入正确", async function () {
      expect(await math.mostSignificantBit(ethers.MaxUint256)).to.equal(255);
      expect(await math.leastSignificantBit(ethers.MaxUint256)).to.equal(0);
      for (let i = 1n; i < 256n; i += 5n) {
        const below = (1n << i) - 1n;
        const above = (1n << i) + 1n;
        expect(await math.mostSignificantBit(below), `msb 2^${i}-1`).to.equal(i - 1n);
        expect(await math.leastSignificantBit(below), `lsb 2^${i}-1`).to.equal(0);
        expect(await math.mostSignificantBit(above), `msb 2^${i}+1`).to.equal(i);
        expect(await math.leastSignificantBit(above), `lsb 2^${i}+1`).to.equal(0);
        // 高位全 1、低 i 位全 0
        const highOnes = ethers.MaxUint256 - below;
        expect(await math.mostSignificantBit(highOnes)).to.equal(255);
        expect(await math.leastSignificantBit(highOnes), `lsb 高位全 1`).to.equal(i);
      }
    });

    it("伪随机输入与参照实现一致", async function () {
      let x = 0x9e3779b97f4a7c15f39cc0605cedc834n;
      for (let i = 0; i < 64; i++) {
        // 线性同余生成 256 位以内的数，并随机右移以覆盖不同位宽
        x = (x * 6364136223846793005n + 1442695040888963407n) % (1n << 256n);
        const value = (x >> BigInt((i * 37) % 250)) | 1n << BigInt((i * 11) % 200);
        expect(await math.mostSignificantBit(value), `msb ${value}`).to.equal(msb(value));
        expect(await math.leastSignificantBit(value), `lsb ${value}`).to.equal(lsb(value));
      }
    });
  });
});
