/**
 * UniGo-DEX 部署脚本
 *
 * 部署出一套可以直接交易的本地环境：
 * 1. 核心合约：UniGoFactory / UniGoManager / UniGoQuoter / FlashArbitrage
 * 2. 测试代币：WETH / USDC / DAI / WBTC（都是 18 位小数）
 * 3. 六个池子，覆盖三档费率，能组成多跳路径：
 *      WETH/USDC 0.05% 和 0.3%、USDC/DAI 0.05% 和 0.3%、DAI/WBTC 0.3% 和 1%
 * 4. 每个池子初始化价格并注入流动性（一个宽区间打底，叠加窄区间和单边区间）
 * 5. 模拟的 Chainlink 聚合器（ETH / USD）
 * 6. 给前几个测试账户铸币并授权 Manager，方便直接发起交易
 *
 * 最后打印后端需要的全部环境变量，复制到终端即可启动后端。
 *
 * 使用方法：
 *   npx hardhat node                                       # 启动本地节点
 *   npx hardhat run scripts/deploy.js --network localhost  # 部署到本地
 *
 * 可选环境变量：
 *   RPC_URL        本地节点地址，默认 http://127.0.0.1:8545
 *   DEPLOY_OUTPUT  把部署结果（地址清单）写成 JSON 文件的路径
 */
const fs = require("fs");
const { ethers } = require("hardhat");

const Q192 = 2n ** 192n;
const DEADLINE = 4102444800n; // 2100 年
// 每个账户铸造的代币数量
const SUPPLY = ethers.parseEther("1000000000000");
// 宽区间的流动性；窄区间和单边区间按倍数叠加
const BASE_LIQUIDITY = 10n ** 24n;

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

// tick 向下对齐到 tickSpacing 的整数倍
function align(tick, spacing) {
  return Math.floor(tick / spacing) * spacing;
}

async function deploy(name, ...args) {
  const factory = await ethers.getContractFactory(name);
  const contract = await factory.deploy(...args);
  await contract.waitForDeployment();
  return contract;
}

async function main() {
  const signers = await ethers.getSigners();
  const deployer = signers[0];
  console.log("部署账户:", deployer.address);

  // 1. 核心合约
  const factory = await deploy("UniGoFactory");
  const factoryAddr = await factory.getAddress();
  const manager = await deploy("UniGoManager", factoryAddr);
  const managerAddr = await manager.getAddress();
  const quoter = await deploy("UniGoQuoter", factoryAddr);
  const flashArb = await deploy("FlashArbitrage", factoryAddr);

  // 2. 测试代币；给部署账户和前三个测试账户铸币并授权 Manager
  const tokens = {};
  for (const [symbol, name] of [
    ["WETH", "Wrapped Ether"],
    ["USDC", "USD Coin"],
    ["DAI", "Dai Stablecoin"],
    ["WBTC", "Wrapped BTC"],
  ]) {
    const token = await deploy("TestERC20", name, symbol);
    tokens[symbol] = { contract: token, address: await token.getAddress() };
    for (const account of signers.slice(0, 4)) {
      await (await token.mint(account.address, SUPPLY)).wait();
      await (await token.connect(account).approve(managerAddr, ethers.MaxUint256)).wait();
    }
  }

  // 3. 池子：price 是 "1 个 base 值多少 quote"
  const poolSpecs = [
    { base: "WETH", quote: "USDC", fee: 500, price: 2000n },
    { base: "WETH", quote: "USDC", fee: 3000, price: 2000n },
    { base: "USDC", quote: "DAI", fee: 500, price: 1n },
    { base: "USDC", quote: "DAI", fee: 3000, price: 1n },
    { base: "WBTC", quote: "DAI", fee: 3000, price: 60000n },
    { base: "WBTC", quote: "DAI", fee: 10000, price: 60000n },
  ];
  const pools = [];
  for (const spec of poolSpecs) {
    const base = tokens[spec.base].address;
    const quote = tokens[spec.quote].address;
    await (await factory.createPool(base, quote, spec.fee)).wait();
    const poolAddr = await factory.getPool(base, quote, spec.fee);
    const pool = await ethers.getContractAt("UniGoPool", poolAddr);

    // 池子价格是 token1 / token0；base 的地址较小时 base 是 token0，否则取倒数
    const baseIsToken0 = BigInt(base) < BigInt(quote);
    const sqrtPriceX96 = baseIsToken0 ? sqrt(spec.price * Q192) : sqrt(Q192 / spec.price);
    await (await pool.initialize(sqrtPriceX96)).wait();

    // 4. 流动性：宽区间打底 + 当前价附近的窄区间 + 上下各一个单边区间，
    //    这样大一点的 swap 会穿越已初始化的 tick
    const tick = Number((await pool.slot0()).tick);
    const spacing = Number(await pool.tickSpacing());
    const center = align(tick, spacing);
    const positions = [
      [center - 100 * spacing, center + 100 * spacing, BASE_LIQUIDITY],
      [center - 10 * spacing, center + 10 * spacing, BASE_LIQUIDITY * 4n],
      [center + 2 * spacing, center + 30 * spacing, BASE_LIQUIDITY * 2n],
      [center - 30 * spacing, center - 2 * spacing, BASE_LIQUIDITY * 2n],
    ];
    for (const [tickLower, tickUpper, amount] of positions) {
      await (
        await manager.mint({
          pool: poolAddr,
          tickLower,
          tickUpper,
          amount,
          amount0Min: 0,
          amount1Min: 0,
          deadline: DEADLINE,
        })
      ).wait();
    }
    pools.push({ pair: `${spec.base}/${spec.quote}`, fee: spec.fee, address: poolAddr, tick });
    console.log(`池子 ${spec.base}/${spec.quote} ${spec.fee / 10000}% -> ${poolAddr}（tick ${tick}，${positions.length} 个头寸）`);
  }

  // 5. 模拟的 Chainlink 聚合器：ETH / USD，8 位小数
  const feed = await deploy("TestAggregator", 8, "ETH / USD", 2000n * 10n ** 8n);
  const feedAddr = await feed.getAddress();

  // 自检：链上 Quoter 报一下 1 WETH 换多少 USDC，确认池子真的能成交
  const path = ethers.solidityPacked(["address", "uint24", "address"], [tokens.WETH.address, 500, tokens.USDC.address]);
  const quoted = await quoter.quoteExactInput.staticCall(path, ethers.parseEther("1"));
  console.log(`自检：1 WETH -> ${ethers.formatEther(quoted)} USDC（0.05% 池）`);

  const result = {
    rpcUrl: process.env.RPC_URL || "http://127.0.0.1:8545",
    chainId: Number((await ethers.provider.getNetwork()).chainId),
    factory: factoryAddr,
    manager: managerAddr,
    quoter: await quoter.getAddress(),
    flashArbitrage: await flashArb.getAddress(),
    tokens: Object.fromEntries(Object.entries(tokens).map(([symbol, t]) => [symbol, t.address])),
    pools,
    chainlinkFeeds: `${tokens.WETH.address}:${tokens.USDC.address}:${feedAddr}:18:18:3600`,
    aggregator: feedAddr,
  };
  if (process.env.DEPLOY_OUTPUT) {
    fs.writeFileSync(process.env.DEPLOY_OUTPUT, JSON.stringify(result, null, 2) + "\n");
  }

  console.log("\n========================================");
  console.log("  UniGo-DEX 部署完成");
  console.log("========================================");
  for (const [symbol, address] of Object.entries(result.tokens)) {
    console.log(`${symbol.padEnd(5)} ${address}`);
  }
  console.log("\n启动后端（在 backend/ 目录下执行）：\n");
  console.log(
    [
      `RPC_URL=${result.rpcUrl}`,
      `FACTORY_ADDRESS=${result.factory}`,
      `MANAGER_ADDRESS=${result.manager}`,
      `QUOTER_ADDRESS=${result.quoter}`,
      `FLASH_ARB_ADDRESS=${result.flashArbitrage}`,
      `WETH_ADDRESS=${result.tokens.WETH}`,
      `CHAINLINK_FEEDS=${result.chainlinkFeeds}`,
      "go run .",
    ].join(" \\\n")
  );
  console.log("\n报价示例（1 WETH -> USDC）：\n");
  console.log(
    `curl "http://localhost:8080/api/v1/quote?token_in=${result.tokens.WETH}&token_out=${result.tokens.USDC}&amount_in=1000000000000000000"`
  );
}

main()
  .then(() => process.exit(0))
  .catch((error) => {
    console.error("部署失败:", error);
    process.exit(1);
  });
