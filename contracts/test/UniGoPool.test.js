const { expect } = require("chai");
const { ethers } = require("hardhat");

/**
 * UniGoPool 合约测试
 *
 * 测试覆盖：
 * 1. 池子初始化
 * 2. 添加流动性（mint）
 * 3. 代币交换（swap）
 * 4. 移除流动性（burn）与提取（collect）
 * 5. Manager 的期限与滑点保护
 * 6. 闪电贷（flash）
 */
describe("UniGoPool", function () {
  let factory;
  let pool;
  let token0;
  let token1;
  let manager;
  let owner;
  let user;

  // 测试用常量
  // 价格 1:1 对应的 sqrtPriceX96 = sqrt(1) * 2^96
  const INIT_SQRT_PRICE = BigInt("79228162514264337593543950336"); // sqrt(1) * 2^96
  // MIN_SQRT_RATIO + 1，token0 -> token1 时作为价格下限
  const MIN_SQRT_RATIO_PLUS_ONE = BigInt("4295128740");
  // 不限期的 deadline
  const NO_DEADLINE = ethers.MaxUint256;
  const Q128 = 2n ** 128n;

  // 辅助函数：通过 Manager 添加流动性
  async function addLiquidity(tickLower, tickUpper, amount) {
    return manager.mint({
      pool: await pool.getAddress(),
      tickLower,
      tickUpper,
      amount,
      amount0Min: 0,
      amount1Min: 0,
      deadline: NO_DEADLINE,
    });
  }

  // 辅助函数：构造 token0 -> token1 的 swap 参数
  async function swapParams(overrides = {}) {
    return {
      pool: await pool.getAddress(),
      zeroForOne: true,
      amountSpecified: ethers.parseEther("1"),
      sqrtPriceLimitX96: MIN_SQRT_RATIO_PLUS_ONE,
      recipient: owner.address,
      amountOutMinimum: 0,
      deadline: NO_DEADLINE,
      ...overrides,
    };
  }

  // 辅助函数：读取 owner 在指定区间的 position
  async function getPosition(tickLower, tickUpper) {
    const key = ethers.solidityPackedKeccak256(
      ["address", "int24", "int24"],
      [owner.address, tickLower, tickUpper]
    );
    return pool.positions(key);
  }

  // 辅助函数：部署 ERC20 测试代币
  async function deployToken(name, symbol) {
    const Token = await ethers.getContractFactory("TestERC20");
    const token = await Token.deploy(name, symbol);
    await token.waitForDeployment();
    return token;
  }

  beforeEach(async function () {
    [owner, user] = await ethers.getSigners();

    // 部署测试代币
    const tokenA = await deployToken("Token A", "TKA");
    const tokenB = await deployToken("Token B", "TKB");

    // 确保 token0 < token1（按地址排序）
    const addrA = await tokenA.getAddress();
    const addrB = await tokenB.getAddress();
    if (addrA.toLowerCase() < addrB.toLowerCase()) {
      token0 = tokenA;
      token1 = tokenB;
    } else {
      token0 = tokenB;
      token1 = tokenA;
    }

    // 部署 Factory
    const Factory = await ethers.getContractFactory("UniGoFactory");
    factory = await Factory.deploy();
    await factory.waitForDeployment();

    // 创建 Pool（0.3% 费率）
    const token0Addr = await token0.getAddress();
    const token1Addr = await token1.getAddress();
    const tx = await factory.createPool(token0Addr, token1Addr, 3000);
    await tx.wait();

    const poolAddr = await factory.getPool(token0Addr, token1Addr, 3000);
    pool = await ethers.getContractAt("UniGoPool", poolAddr);

    // 初始化池子（价格 1:1）
    await pool.initialize(INIT_SQRT_PRICE);

    // 部署 Manager
    const Manager = await ethers.getContractFactory("UniGoManager");
    manager = await Manager.deploy();
    await manager.waitForDeployment();

    // 给用户铸造测试代币并 approve Manager
    const mintAmount = ethers.parseEther("10000");
    await token0.mint(owner.address, mintAmount);
    await token1.mint(owner.address, mintAmount);
    const managerAddr = await manager.getAddress();
    await token0.approve(managerAddr, mintAmount);
    await token1.approve(managerAddr, mintAmount);
  });

  describe("初始化", function () {
    it("应该正确设置初始价格和 tick", async function () {
      const slot0 = await pool.slot0();
      expect(slot0.sqrtPriceX96).to.equal(INIT_SQRT_PRICE);
      // tick 0 对应价格 1:1
      expect(slot0.tick).to.equal(0);
    });

    it("不能重复初始化", async function () {
      // 池子已在 beforeEach 中初始化，再次初始化应失败
      await expect(pool.initialize(INIT_SQRT_PRICE)).to.be.revertedWith("AI");
    });
  });

  describe("添加流动性 (mint)", function () {
    it("应该能成功添加流动性", async function () {
      const poolAddr = await pool.getAddress();
      const tickLower = -60;  // tickSpacing=60 的整数倍
      const tickUpper = 60;
      const liquidityAmount = ethers.parseEther("1");

      // 通过 Manager 添加流动性
      const tx = await manager.mint({
        pool: poolAddr,
        tickLower: tickLower,
        tickUpper: tickUpper,
        amount: liquidityAmount,
        amount0Min: 0,
        amount1Min: 0,
        deadline: NO_DEADLINE,
      });
      await tx.wait();

      // 验证流动性已添加
      const currentLiquidity = await pool.liquidity();
      expect(currentLiquidity).to.equal(liquidityAmount);
    });
  });

  describe("代币交换 (swap)", function () {
    it("应该能执行 token0 -> token1 的交换", async function () {
      const poolAddr = await pool.getAddress();

      // 先添加流动性
      await manager.mint({
        pool: poolAddr,
        tickLower: -600,
        tickUpper: 600,
        amount: ethers.parseEther("100"),
        amount0Min: 0,
        amount1Min: 0,
        deadline: NO_DEADLINE,
      });

      // 记录交换前余额
      const balance1Before = await token1.balanceOf(owner.address);

      // 执行 swap：token0 -> token1（zeroForOne = true）
      // MIN_SQRT_RATIO + 1 作为价格下限
      await manager.swap({
        pool: poolAddr,
        zeroForOne: true,
        amountSpecified: ethers.parseEther("1"),
        sqrtPriceLimitX96: MIN_SQRT_RATIO_PLUS_ONE,
        recipient: owner.address,
        amountOutMinimum: 0,
        deadline: NO_DEADLINE,
      });

      // 验证收到了 token1
      const balance1After = await token1.balanceOf(owner.address);
      expect(balance1After).to.be.gt(balance1Before);
    });
  });

  describe("移除流动性 (burn) 与提取 (collect)", function () {
    it("burn 后能通过 collect 取回本金，且不能多取", async function () {
      const liquidityAmount = ethers.parseEther("100");
      await addLiquidity(-600, 600, liquidityAmount);
      const poolAddr = await pool.getAddress();
      const deposited0 = await token0.balanceOf(poolAddr);

      await pool.burn(-600, 600, liquidityAmount);
      expect(await pool.liquidity()).to.equal(0);

      // 请求远超应得的数量，实际只能拿到 tokensOwed
      const position = await getPosition(-600, 600);
      const balance0Before = await token0.balanceOf(owner.address);
      await pool.collect(owner.address, -600, 600, ethers.MaxUint256 >> 128n, ethers.MaxUint256 >> 128n);
      const received0 = (await token0.balanceOf(owner.address)) - balance0Before;

      expect(received0).to.equal(position.tokensOwed0);
      // mint 向上取整、burn 向下取整，最多相差 1 wei
      expect(received0).to.be.closeTo(deposited0, 1);
    });

    it("没有 position 的地址提取不到任何 token", async function () {
      await addLiquidity(-600, 600, ethers.parseEther("100"));

      const balance0Before = await token0.balanceOf(user.address);
      await pool.connect(user).collect(user.address, -600, 600, ethers.parseEther("1"), ethers.parseEther("1"));
      expect(await token0.balanceOf(user.address)).to.equal(balance0Before);
    });

    it("swap 手续费计入 LP 可领取的费用", async function () {
      await addLiquidity(-600, 600, ethers.parseEther("100"));
      await manager.swap(await swapParams());

      // burn(0) 只结算手续费
      await pool.burn(-600, 600, 0);
      const position = await getPosition(-600, 600);
      // 输入 1 token0，费率 0.3%
      expect(position.tokensOwed0).to.be.closeTo(ethers.parseEther("0.003"), 1);
      expect(position.tokensOwed1).to.equal(0);
    });
  });

  describe("Manager 期限与滑点保护", function () {
    beforeEach(async function () {
      await addLiquidity(-600, 600, ethers.parseEther("100"));
    });

    it("mint 过期应 revert", async function () {
      await expect(
        manager.mint({
          pool: await pool.getAddress(),
          tickLower: -600,
          tickUpper: 600,
          amount: ethers.parseEther("1"),
          amount0Min: 0,
          amount1Min: 0,
          deadline: 1,
        })
      ).to.be.revertedWith("EXP");
    });

    it("mint 存入数量低于下限应 revert", async function () {
      await expect(
        manager.mint({
          pool: await pool.getAddress(),
          tickLower: -600,
          tickUpper: 600,
          amount: ethers.parseEther("1"),
          amount0Min: ethers.parseEther("1"),
          amount1Min: 0,
          deadline: NO_DEADLINE,
        })
      ).to.be.revertedWith("S0");
    });

    it("swap 过期应 revert", async function () {
      await expect(manager.swap(await swapParams({ deadline: 1 }))).to.be.revertedWith("EXP");
    });

    it("swap 到手数量低于 amountOutMinimum 应 revert", async function () {
      // 先模拟出实际到手数量，再要求多 1 wei
      const [, amount1] = await manager.swap.staticCall(await swapParams());
      const amountOut = -amount1;

      await expect(
        manager.swap(await swapParams({ amountOutMinimum: amountOut + 1n }))
      ).to.be.revertedWith("SLP");
    });

    it("期限内且满足滑点下限时正常成交", async function () {
      const [, amount1] = await manager.swap.staticCall(await swapParams());
      const amountOut = -amount1;
      const latest = await ethers.provider.getBlock("latest");

      const balance1Before = await token1.balanceOf(owner.address);
      await manager.swap(
        await swapParams({ amountOutMinimum: amountOut, deadline: latest.timestamp + 600 })
      );
      expect((await token1.balanceOf(owner.address)) - balance1Before).to.equal(amountOut);
    });
  });

  describe("闪电贷 (flash)", function () {
    let borrower;
    let borrowerAddr;
    let poolAddr;
    const LIQUIDITY = ethers.parseEther("1000");
    const BORROW = ethers.parseEther("10");
    const FEE = ethers.parseEther("0.03"); // 10 * 0.3%
    // TestFlashBorrower.Mode
    const REPAY = 0;
    const NO_REPAY = 1;
    const REENTER = 2;

    beforeEach(async function () {
      poolAddr = await pool.getAddress();
      await addLiquidity(-600, 600, LIQUIDITY);

      const Borrower = await ethers.getContractFactory("TestFlashBorrower");
      borrower = await Borrower.deploy();
      await borrower.waitForDeployment();
      borrowerAddr = await borrower.getAddress();

      // 借款合约只预先持有手续费，本金来自闪电贷
      await token0.mint(borrowerAddr, ethers.parseEther("1"));
      await token1.mint(borrowerAddr, ethers.parseEther("1"));
    });

    it("正常借还：手续费留在池中并触发 Flash 事件", async function () {
      const pool0Before = await token0.balanceOf(poolAddr);
      const pool1Before = await token1.balanceOf(poolAddr);
      const borrower0Before = await token0.balanceOf(borrowerAddr);

      await expect(borrower.flash(poolAddr, BORROW, BORROW, REPAY))
        .to.emit(pool, "Flash")
        .withArgs(borrowerAddr, borrowerAddr, BORROW, BORROW, FEE, FEE);

      expect(await token0.balanceOf(poolAddr)).to.equal(pool0Before + FEE);
      expect(await token1.balanceOf(poolAddr)).to.equal(pool1Before + FEE);
      expect(await token0.balanceOf(borrowerAddr)).to.equal(borrower0Before - FEE);
      expect(await borrower.lastFee0()).to.equal(FEE);

      // 手续费按活跃流动性均摊进全局累计值
      expect(await pool.feeGrowthGlobal0X128()).to.equal((FEE * Q128) / LIQUIDITY);
      expect(await pool.feeGrowthGlobal1X128()).to.equal((FEE * Q128) / LIQUIDITY);
    });

    it("手续费向上取整", async function () {
      // 1001 wei * 0.3% = 3.003，向上取整为 4
      await expect(borrower.flash(poolAddr, 1001, 0, REPAY))
        .to.emit(pool, "Flash")
        .withArgs(borrowerAddr, borrowerAddr, 1001, 0, 4, 0);
    });

    it("不还款应 revert", async function () {
      await expect(borrower.flash(poolAddr, BORROW, 0, NO_REPAY)).to.be.revertedWith("F0");
      await expect(borrower.flash(poolAddr, 0, BORROW, NO_REPAY)).to.be.revertedWith("F1");
    });

    it("回调里重入 Pool 应被重入锁拦截", async function () {
      await expect(borrower.flash(poolAddr, BORROW, BORROW, REENTER)).to.be.revertedWith("LOK");
    });

    it("手续费计入 LP 可领取的费用并能提取", async function () {
      await borrower.flash(poolAddr, BORROW, BORROW, REPAY);

      // burn(0) 只结算手续费
      await pool.burn(-600, 600, 0);
      const position = await getPosition(-600, 600);
      expect(position.tokensOwed0).to.be.closeTo(FEE, 1);
      expect(position.tokensOwed1).to.be.closeTo(FEE, 1);
      // 流动性没有变化
      expect(position.liquidity).to.equal(LIQUIDITY);

      const balance0Before = await token0.balanceOf(owner.address);
      const balance1Before = await token1.balanceOf(owner.address);
      await pool.collect(owner.address, -600, 600, FEE, FEE);
      expect((await token0.balanceOf(owner.address)) - balance0Before).to.equal(position.tokensOwed0);
      expect((await token1.balanceOf(owner.address)) - balance1Before).to.equal(position.tokensOwed1);

      // 领完之后不能重复领取
      expect((await getPosition(-600, 600)).tokensOwed0).to.equal(0);
    });

    it("手续费只分给区间包含当前价格的 position", async function () {
      // 区间在当前价格上方，不提供活跃流动性
      await addLiquidity(1200, 1800, LIQUIDITY);
      await borrower.flash(poolAddr, BORROW, 0, REPAY);

      await pool.burn(1200, 1800, 0);
      expect((await getPosition(1200, 1800)).tokensOwed0).to.equal(0);
    });
  });
});
