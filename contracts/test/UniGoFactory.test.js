const { expect } = require("chai");
const { ethers } = require("hardhat");

/**
 * UniGoFactory 合约测试
 *
 * 测试覆盖：
 * 1. 创建交易对池
 * 2. 费率与 tickSpacing 映射
 * 3. 重复创建检查
 * 4. 参数验证
 */
describe("UniGoFactory", function () {
  let factory;
  let tokenA;
  let tokenB;
  let owner;

  beforeEach(async function () {
    [owner] = await ethers.getSigners();

    // 部署 Factory
    const Factory = await ethers.getContractFactory("UniGoFactory");
    factory = await Factory.deploy();
    await factory.waitForDeployment();

    // 部署测试代币
    const Token = await ethers.getContractFactory("TestERC20");
    tokenA = await Token.deploy("Token A", "TKA");
    tokenB = await Token.deploy("Token B", "TKB");
    await tokenA.waitForDeployment();
    await tokenB.waitForDeployment();
  });

  describe("费率配置", function () {
    it("应该支持 500 (0.05%) 费率", async function () {
      expect(await factory.feeAmountTickSpacing(500)).to.equal(10);
    });

    it("应该支持 3000 (0.3%) 费率", async function () {
      expect(await factory.feeAmountTickSpacing(3000)).to.equal(60);
    });

    it("应该支持 10000 (1%) 费率", async function () {
      expect(await factory.feeAmountTickSpacing(10000)).to.equal(200);
    });
  });

  describe("创建池子", function () {
    it("应该能创建新的交易对池", async function () {
      const addrA = await tokenA.getAddress();
      const addrB = await tokenB.getAddress();

      const tx = await factory.createPool(addrA, addrB, 3000);
      const receipt = await tx.wait();

      // 验证池子已创建（地址非零）
      const poolAddr = await factory.getPool(addrA, addrB, 3000);
      expect(poolAddr).to.not.equal(ethers.ZeroAddress);

      // 验证反向查询也能找到
      const poolAddrReverse = await factory.getPool(addrB, addrA, 3000);
      expect(poolAddrReverse).to.equal(poolAddr);
    });

    it("应该触发 PoolCreated 事件", async function () {
      const addrA = await tokenA.getAddress();
      const addrB = await tokenB.getAddress();

      // 排序 token 地址
      const [token0Addr, token1Addr] = addrA.toLowerCase() < addrB.toLowerCase()
        ? [addrA, addrB]
        : [addrB, addrA];

      await expect(factory.createPool(addrA, addrB, 3000))
        .to.emit(factory, "PoolCreated")
        .withArgs(token0Addr, token1Addr, 3000, 60, (addr) => addr !== ethers.ZeroAddress);
    });

    it("不能创建相同的交易对池", async function () {
      const addrA = await tokenA.getAddress();
      const addrB = await tokenB.getAddress();

      await factory.createPool(addrA, addrB, 3000);
      await expect(factory.createPool(addrA, addrB, 3000)).to.be.revertedWith("PE");
    });

    it("不能使用不支持的费率", async function () {
      const addrA = await tokenA.getAddress();
      const addrB = await tokenB.getAddress();

      await expect(factory.createPool(addrA, addrB, 100)).to.be.revertedWith("FEE");
    });

    it("不能使用相同的 token 地址", async function () {
      const addrA = await tokenA.getAddress();
      await expect(factory.createPool(addrA, addrA, 3000)).to.be.revertedWith("IA");
    });

    it("可以为同一交易对创建不同费率的池子", async function () {
      const addrA = await tokenA.getAddress();
      const addrB = await tokenB.getAddress();

      await factory.createPool(addrA, addrB, 500);
      await factory.createPool(addrA, addrB, 3000);
      await factory.createPool(addrA, addrB, 10000);

      const pool500 = await factory.getPool(addrA, addrB, 500);
      const pool3000 = await factory.getPool(addrA, addrB, 3000);
      const pool10000 = await factory.getPool(addrA, addrB, 10000);

      // 三个池子地址应该不同
      expect(pool500).to.not.equal(pool3000);
      expect(pool3000).to.not.equal(pool10000);
      expect(pool500).to.not.equal(pool10000);
    });
  });

  describe("Pool 属性验证", function () {
    it("创建的池子应该有正确的属性", async function () {
      const addrA = await tokenA.getAddress();
      const addrB = await tokenB.getAddress();

      await factory.createPool(addrA, addrB, 3000);
      const poolAddr = await factory.getPool(addrA, addrB, 3000);
      const pool = await ethers.getContractAt("UniGoPool", poolAddr);

      // 验证 token 排序
      const [expectedToken0, expectedToken1] = addrA.toLowerCase() < addrB.toLowerCase()
        ? [addrA, addrB]
        : [addrB, addrA];

      expect(await pool.token0()).to.equal(expectedToken0);
      expect(await pool.token1()).to.equal(expectedToken1);
      expect(await pool.fee()).to.equal(3000);
      expect(await pool.tickSpacing()).to.equal(60);
    });
  });
});
