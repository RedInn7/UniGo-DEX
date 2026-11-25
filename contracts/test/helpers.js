const { ethers } = require("hardhat");

/**
 * 测试公共工具：常量、部署辅助函数、路径编码
 */

// 价格 1:1 对应的 sqrtPriceX96 = sqrt(1) * 2^96
const INIT_SQRT_PRICE = 2n ** 96n;
// 不限价时使用的价格边界
const MIN_SQRT_RATIO_PLUS_ONE = 4295128740n;
const MAX_SQRT_RATIO_MINUS_ONE = 1461446703485210103287273052203988822378723970341n;
// 不限期的 deadline
const NO_DEADLINE = ethers.MaxUint256;
const Q128 = 2n ** 128n;
const MAX_UINT128 = 2n ** 128n - 1n;

// BigInt 整数平方根（向下取整）
function sqrt(value) {
  if (value < 2n) return value;
  let x = value;
  let y = (x + 1n) / 2n;
  while (y < x) {
    x = y;
    y = (x + value / x) / 2n;
  }
  return x;
}

// 由价格（amount1 / amount0）计算 sqrtPriceX96
function encodeSqrtPrice(amount1, amount0) {
  return sqrt((BigInt(amount1) << 192n) / BigInt(amount0));
}

// 部署 n 个测试代币，按地址升序返回
async function deployTokens(n) {
  const Token = await ethers.getContractFactory("TestERC20");
  const tokens = [];
  for (let i = 0; i < n; i++) {
    const token = await Token.deploy(`Token ${i}`, `TK${i}`);
    await token.waitForDeployment();
    tokens.push({ token, address: (await token.getAddress()).toLowerCase() });
  }
  tokens.sort((a, b) => (a.address < b.address ? -1 : 1));
  return tokens.map((t) => t.token);
}

// 部署 Factory / Manager / Quoter
async function deployCore() {
  const Factory = await ethers.getContractFactory("UniGoFactory");
  const factory = await Factory.deploy();
  await factory.waitForDeployment();
  const factoryAddr = await factory.getAddress();

  const Manager = await ethers.getContractFactory("UniGoManager");
  const manager = await Manager.deploy(factoryAddr);
  await manager.waitForDeployment();

  const Quoter = await ethers.getContractFactory("UniGoQuoter");
  const quoter = await Quoter.deploy(factoryAddr);
  await quoter.waitForDeployment();

  return { factory, manager, quoter };
}

// 创建并初始化一个池子
async function createPool(factory, tokenA, tokenB, fee, sqrtPriceX96 = INIT_SQRT_PRICE) {
  const addrA = await tokenA.getAddress();
  const addrB = await tokenB.getAddress();
  await factory.createPool(addrA, addrB, fee);
  const pool = await ethers.getContractAt("UniGoPool", await factory.getPool(addrA, addrB, fee));
  await pool.initialize(sqrtPriceX96);
  return pool;
}

// 给账户铸造代币并 approve 给 spender
async function fund(tokens, signer, spender, amount = ethers.parseEther("1000000")) {
  for (const token of tokens) {
    await token.mint(signer.address, amount);
    await token.connect(signer).approve(spender, ethers.MaxUint256);
  }
}

// 多跳路径编码：token(20) + fee(3) + token(20) + ...
function encodePath(tokenAddresses, fees) {
  const types = ["address"];
  const values = [tokenAddresses[0]];
  for (let i = 0; i < fees.length; i++) {
    types.push("uint24", "address");
    values.push(fees[i], tokenAddresses[i + 1]);
  }
  return ethers.solidityPacked(types, values);
}

// position 的存储 key
function positionKey(owner, tickLower, tickUpper) {
  return ethers.solidityPackedKeccak256(["address", "int24", "int24"], [owner, tickLower, tickUpper]);
}

module.exports = {
  INIT_SQRT_PRICE,
  MIN_SQRT_RATIO_PLUS_ONE,
  MAX_SQRT_RATIO_MINUS_ONE,
  NO_DEADLINE,
  Q128,
  MAX_UINT128,
  sqrt,
  encodeSqrtPrice,
  deployTokens,
  deployCore,
  createPool,
  fund,
  encodePath,
  positionKey,
};
