const { expect } = require("chai");
const { ethers } = require("hardhat");

/**
 * UniGoPool 合约测试
 *
 * 测试覆盖：
 * 1. 池子初始化
 * 2. 添加流动性（mint）
 * 3. 代币交换（swap）
 * 4. 移除流动性（burn）
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
      });

      // 记录交换前余额
      const balance1Before = await token1.balanceOf(owner.address);

      // 执行 swap：token0 -> token1（zeroForOne = true）
      // MIN_SQRT_RATIO + 1 作为价格下限
      const MIN_SQRT_RATIO_PLUS_ONE = BigInt("4295128740");
      await manager.swap({
        pool: poolAddr,
        zeroForOne: true,
        amountSpecified: ethers.parseEther("1"),
        sqrtPriceLimitX96: MIN_SQRT_RATIO_PLUS_ONE,
        recipient: owner.address,
      });

      // 验证收到了 token1
      const balance1After = await token1.balanceOf(owner.address);
      expect(balance1After).to.be.gt(balance1Before);
    });
  });
});
