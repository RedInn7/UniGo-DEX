/**
 * UniGo-DEX 部署脚本
 *
 * 部署流程：
 * 1. 部署 UniGoFactory（池工厂）
 * 2. 部署 UniGoManager（用户交互管理器）
 * 3. 部署 UniGoQuoter（报价合约）
 * 4. 部署测试代币（仅测试环境）
 * 5. 创建示例交易对池
 *
 * 使用方法：
 *   npx hardhat node                                    # 启动本地节点
 *   npx hardhat run scripts/deploy.js --network localhost  # 部署到本地
 */
const { ethers } = require("hardhat");

async function main() {
  const [deployer] = await ethers.getSigners();
  console.log("部署账户:", deployer.address);
  console.log("账户余额:", ethers.formatEther(await ethers.provider.getBalance(deployer.address)), "ETH");
  console.log("---");

  // 1. 部署 Factory
  console.log("正在部署 UniGoFactory...");
  const Factory = await ethers.getContractFactory("UniGoFactory");
  const factory = await Factory.deploy();
  await factory.waitForDeployment();
  const factoryAddr = await factory.getAddress();
  console.log("UniGoFactory 部署到:", factoryAddr);

  // 2. 部署 Manager
  console.log("正在部署 UniGoManager...");
  const Manager = await ethers.getContractFactory("UniGoManager");
  const manager = await Manager.deploy(factoryAddr);
  await manager.waitForDeployment();
  const managerAddr = await manager.getAddress();
  console.log("UniGoManager 部署到:", managerAddr);

  // 3. 部署 Quoter
  console.log("正在部署 UniGoQuoter...");
  const Quoter = await ethers.getContractFactory("UniGoQuoter");
  const quoter = await Quoter.deploy(factoryAddr);
  await quoter.waitForDeployment();
  const quoterAddr = await quoter.getAddress();
  console.log("UniGoQuoter 部署到:", quoterAddr);

  // 4. 部署测试代币
  console.log("\n正在部署测试代币...");
  const Token = await ethers.getContractFactory("TestERC20");

  const weth = await Token.deploy("Wrapped Ether", "WETH");
  await weth.waitForDeployment();
  console.log("WETH 部署到:", await weth.getAddress());

  const usdc = await Token.deploy("USD Coin", "USDC");
  await usdc.waitForDeployment();
  console.log("USDC 部署到:", await usdc.getAddress());

  // 5. 创建示例交易对池
  console.log("\n正在创建 WETH/USDC 交易对...");
  const wethAddr = await weth.getAddress();
  const usdcAddr = await usdc.getAddress();

  const tx = await factory.createPool(wethAddr, usdcAddr, 3000);
  await tx.wait();

  const poolAddr = await factory.getPool(wethAddr, usdcAddr, 3000);
  console.log("WETH/USDC Pool (0.3%) 部署到:", poolAddr);

  // 初始化池子（假设 ETH 价格 ~2000 USDC）
  // sqrtPriceX96 = sqrt(2000) * 2^96 ≈ 3543191142285914205922034323214
  const pool = await ethers.getContractAt("UniGoPool", poolAddr);
  const sqrtPriceX96 = BigInt("3543191142285914205922034323214");
  await pool.initialize(sqrtPriceX96);
  console.log("Pool 已初始化，ETH/USDC ≈ 2000");

  // 输出部署汇总
  console.log("\n========================================");
  console.log("  UniGo-DEX 部署完成！");
  console.log("========================================");
  console.log("Factory:  ", factoryAddr);
  console.log("Manager:  ", managerAddr);
  console.log("Quoter:   ", quoterAddr);
  console.log("WETH:     ", wethAddr);
  console.log("USDC:     ", usdcAddr);
  console.log("Pool:     ", poolAddr);
  console.log("========================================");
}

main()
  .then(() => process.exit(0))
  .catch((error) => {
    console.error("部署失败:", error);
    process.exit(1);
  });
