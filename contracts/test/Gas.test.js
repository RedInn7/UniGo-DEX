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
 * Gas 基准
 *
 * 逐个场景记录单笔交易的 gasUsed；REPORT_GAS=1 时在测试末尾打印汇总表
 */
describe("Gas 基准", function () {
  let manager;
  let tokenA;
  let tokenB;
  let tokenC;
  let poolAB;
  let poolBC;
  let poolAddr;
  let owner;
  const results = [];

  const LIQUIDITY = ethers.parseEther("100");

  async function measure(name, txPromise) {
    const receipt = await (await txPromise).wait();
    results.push([name, receipt.gasUsed]);
    return receipt.gasUsed;
  }

  function mintParams(pool, tickLower, tickUpper) {
    return {
      pool,
      tickLower,
      tickUpper,
      amount: LIQUIDITY,
      amount0Min: 0,
      amount1Min: 0,
      deadline: NO_DEADLINE,
    };
  }

  function swapParams(amount) {
    return {
      pool: poolAddr,
      zeroForOne: true,
      amountSpecified: amount,
      sqrtPriceLimitX96: MIN_SQRT_RATIO_PLUS_ONE,
      recipient: owner.address,
      amountOutMinimum: 0,
      deadline: NO_DEADLINE,
    };
  }

  beforeEach(async function () {
    [owner] = await ethers.getSigners();
    let factory;
    ({ factory, manager } = await deployCore());
    [tokenA, tokenB, tokenC] = await deployTokens(3);
    await fund([tokenA, tokenB, tokenC], owner, await manager.getAddress());

    poolAB = await createPool(factory, tokenA, tokenB, 3000);
    poolBC = await createPool(factory, tokenB, tokenC, 500);
    poolAddr = await poolAB.getAddress();

    // 宽区间 + 窄区间，窄区间的边界 tick -60 用来制造跨 tick 的 swap
    await manager.mint(mintParams(poolAddr, -600, 600));
    await manager.mint(mintParams(poolAddr, -60, 60));
    await manager.mint(mintParams(await poolBC.getAddress(), -600, 600));
    // 预热：让收款地址的余额槽位、池子的手续费累计值都已是非零，贴近稳态成本
    await manager.swap(swapParams(ethers.parseEther("0.01")));
  });

  after(function () {
    if (!process.env.REPORT_GAS) return;
    console.log("\n    Gas 基准（单笔交易 gasUsed）");
    for (const [name, gas] of results) {
      console.log(`      ${name.padEnd(36)} ${gas.toString().padStart(8)}`);
    }
  });

  it("mint：新建 position（两端 tick 均未初始化）", async function () {
    const gas = await measure("mint 新区间", manager.mint(mintParams(poolAddr, -1200, 1200)));
    expect(gas).to.be.gt(0);
  });

  it("mint：向已有 position 追加流动性", async function () {
    const gas = await measure("mint 追加到已有 position", manager.mint(mintParams(poolAddr, -600, 600)));
    expect(gas).to.be.gt(0);
  });

  it("单跳 swap：不跨 tick 比跨 tick 便宜", async function () {
    const within = await measure("单跳 swap（不跨 tick）", manager.swap(swapParams(ethers.parseEther("0.1"))));
    expect((await poolAB.slot0()).tick).to.be.gte(-60);

    const crossing = await measure("单跳 swap（跨 1 个已初始化 tick）", manager.swap(swapParams(ethers.parseEther("1"))));
    expect((await poolAB.slot0()).tick).to.be.lt(-60);

    expect(crossing).to.be.gt(within);
  });

  it("两跳 swap", async function () {
    const path = encodePath(
      [await tokenA.getAddress(), await tokenB.getAddress(), await tokenC.getAddress()],
      [3000, 500]
    );
    const gas = await measure(
      "两跳 exactInput",
      manager.exactInput({
        path,
        recipient: owner.address,
        amountIn: ethers.parseEther("0.1"),
        amountOutMinimum: 0,
        deadline: NO_DEADLINE,
      })
    );
    expect(gas).to.be.gt(0);
  });

  it("精确输出 swap", async function () {
    const gas = await measure(
      "exactOutputSingle（不跨 tick）",
      manager.exactOutputSingle({
        tokenIn: await tokenA.getAddress(),
        tokenOut: await tokenB.getAddress(),
        fee: 3000,
        recipient: owner.address,
        amountOut: ethers.parseEther("0.1"),
        amountInMaximum: ethers.MaxUint256,
        sqrtPriceLimitX96: 0,
        deadline: NO_DEADLINE,
      })
    );
    expect(gas).to.be.gt(0);
  });

  it("flash：借一种和同时借两种", async function () {
    const Borrower = await ethers.getContractFactory("TestFlashBorrower");
    const borrower = await Borrower.deploy();
    await borrower.waitForDeployment();
    const borrowerAddr = await borrower.getAddress();
    await tokenA.mint(borrowerAddr, ethers.parseEther("1"));
    await tokenB.mint(borrowerAddr, ethers.parseEther("1"));
    // 预热借款合约自身的存储槽位
    await borrower.flash(poolAddr, 1000, 1000, 0);

    const single = await measure("flash（借 token0）", borrower.flash(poolAddr, ethers.parseEther("1"), 0, 0));
    const both = await measure(
      "flash（同时借两种）",
      borrower.flash(poolAddr, ethers.parseEther("1"), ethers.parseEther("1"), 0)
    );
    expect(both).to.be.gt(single);
  });
});
