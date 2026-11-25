/**
 * UniGo-DEX 部署脚本
 *
 * 部署出一套可以直接交易的本地环境：
 * 1. 核心合约：UniGoFactory / UniGoManager / UniGoQuoter / FlashArbitrage
 * 2. 测试代币：WETH / USDC / DAI / WBTC（都是 18 位小数）
 * 3. 六个池子，覆盖三档费率，能组成多跳路径：
 *      WETH/USDC 0.05% 和 0.3%、USDC/DAI 0.05% 和 0.3%、WBTC/DAI 0.3% 和 1%
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
 * 部署到测试网（需要 DEPLOYER_KEY，账户里要有测试币）：
 *   DEPLOYER_KEY=0x... npx hardhat run scripts/deploy.js --network sepolia
 *   测试网上不部署模拟聚合器，直接读 Chainlink 官方的 ETH / USD 喂价，
 *   WETH/USDC 池子的初始价格也取自喂价
 *
 * 可选环境变量：
 *   RPC_URL        本地节点地址，默认 http://127.0.0.1:8545
 *   DEPLOY_OUTPUT  把部署结果（地址清单）写成 JSON 文件的路径
 *   RESUME         上次部署中断时，把已有的 factory / manager / quoter / flashArbitrage / tokens
 *                  地址写成 JSON 传进来，脚本会复用它们，并跳过已建好、已注入流动性的池子
 *   GAS_PRICE      固定 gas 价（wei），测试币不多时用
 */
const fs = require("fs");
const { ethers } = require("hardhat");

const Q192 = 2n ** 192n;
const DEADLINE = 4102444800n; // 2100 年
// 每个账户铸造的代币数量
const SUPPLY = ethers.parseEther("1000000000000");

// Chainlink 官方 ETH / USD 喂价地址（按 chainId）
const ETH_USD_FEEDS = {
  11155111: "0x694AA1769357215DE4FAC081bf1f309aDC325306", // Sepolia
  84532: "0x4aDC67696bA383F43DD60A9e78F2C97Fbbfc7cb1", // Base Sepolia
};
const FEED_ABI = [
  "function decimals() view returns (uint8)",
  "function latestRoundData() view returns (uint80, int256, uint256, uint256, uint80)",
];

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

// 上一次部署中断时留下的地址清单（RESUME 指向的 JSON），已有的合约直接复用
const existing = process.env.RESUME ? JSON.parse(fs.readFileSync(process.env.RESUME)) : {};
// 费率对应的 tickSpacing，和 UniGoFactory 构造函数里的一致
const TICK_SPACING = { 500: 10, 3000: 60, 10000: 200 };

async function deploy(name, ...args) {
  const factory = await ethers.getContractFactory(name);
  const contract = await factory.deploy(...args);
  await contract.waitForDeployment();
  return contract;
}

// 有现成地址就复用，否则新部署
async function attachOrDeploy(address, name, ...args) {
  return address ? ethers.getContractAt(name, address) : deploy(name, ...args);
}

// 公共 RPC 背后是多台节点，刚出块时读请求可能落到还没同步的节点上，读操作重试几次
async function read(fn) {
  for (let i = 0; ; i++) {
    try {
      return await fn();
    } catch (error) {
      if (i >= 5) throw error;
      await new Promise((resolve) => setTimeout(resolve, 3000));
    }
  }
}

// 从交易回执里取指定事件的参数；回执是确定的，不受节点同步进度影响
function eventArgs(receipt, contract, name) {
  for (const log of receipt.logs) {
    const parsed = log.address === contract.target ? contract.interface.parseLog(log) : null;
    if (parsed && parsed.name === name) return parsed.args;
  }
  throw new Error(`回执里没有 ${name} 事件`);
}

async function main() {
  const signers = await ethers.getSigners();
  const deployer = signers[0];
  console.log("部署账户:", deployer.address);
  const balanceBefore = await ethers.provider.getBalance(deployer.address);
  const chainId = Number((await ethers.provider.getNetwork()).chainId);

  // 有官方喂价的网络直接用官方喂价；CHAINLINK_ETH_USD 可以手动指定（分叉演练时用）
  const realFeed = process.env.CHAINLINK_ETH_USD || ETH_USD_FEEDS[chainId];
  let ethPrice = 2000n;
  if (realFeed) {
    const aggregator = new ethers.Contract(realFeed, FEED_ABI, ethers.provider);
    const [, answer] = await aggregator.latestRoundData();
    ethPrice = answer / 10n ** BigInt(await aggregator.decimals());
    console.log(`Chainlink ETH / USD 喂价 ${realFeed}：${ethPrice}`);
  }

  // 1. 核心合约
  const factory = await attachOrDeploy(existing.factory, "UniGoFactory");
  const factoryAddr = await factory.getAddress();
  const manager = await attachOrDeploy(existing.manager, "UniGoManager", factoryAddr);
  const managerAddr = await manager.getAddress();
  const quoter = await attachOrDeploy(existing.quoter, "UniGoQuoter", factoryAddr);
  const flashArb = await attachOrDeploy(existing.flashArbitrage, "FlashArbitrage", factoryAddr);

  // 2. 测试代币；给部署账户和前三个测试账户铸币并授权 Manager
  const tokens = {};
  for (const [symbol, name] of [
    ["WETH", "Wrapped Ether"],
    ["USDC", "USD Coin"],
    ["DAI", "Dai Stablecoin"],
    ["WBTC", "Wrapped BTC"],
  ]) {
    const reused = existing.tokens && existing.tokens[symbol];
    const token = await attachOrDeploy(reused, "TestERC20", name, symbol);
    tokens[symbol] = { contract: token, address: await token.getAddress() };
    if (reused) continue;
    for (const account of signers.slice(0, 4)) {
      await (await token.mint(account.address, SUPPLY)).wait();
      await (await token.connect(account).approve(managerAddr, ethers.MaxUint256)).wait();
    }
  }

  // 3. 池子：price 是 "1 个 base 值多少 quote"；liquidity 是宽区间的流动性 L，
  //    窄区间和单边区间按倍数叠加。同样的资金量，价格越接近 1 需要的 L 越大，
  //    所以稳定币对给得多一些，各池子当前价 ±10% 内的深度都在百万美元量级
  const poolSpecs = [
    { base: "WETH", quote: "USDC", fee: 500, price: ethPrice, liquidity: 5n * 10n ** 24n },
    { base: "WETH", quote: "USDC", fee: 3000, price: ethPrice, liquidity: 5n * 10n ** 24n },
    { base: "USDC", quote: "DAI", fee: 500, price: 1n, liquidity: 10n ** 26n },
    { base: "USDC", quote: "DAI", fee: 3000, price: 1n, liquidity: 10n ** 26n },
    { base: "WBTC", quote: "DAI", fee: 3000, price: 60000n, liquidity: 10n ** 24n },
    { base: "WBTC", quote: "DAI", fee: 10000, price: 60000n, liquidity: 10n ** 24n },
  ];
  const pools = [];
  for (const spec of poolSpecs) {
    const base = tokens[spec.base].address;
    const quote = tokens[spec.quote].address;
    let poolAddr = await read(() => factory.getPool(base, quote, spec.fee));
    if (poolAddr === ethers.ZeroAddress) {
      const receipt = await (await factory.createPool(base, quote, spec.fee)).wait();
      poolAddr = eventArgs(receipt, factory, "PoolCreated").pool;
    }
    const pool = await ethers.getContractAt("UniGoPool", poolAddr);

    // 池子价格是 token1 / token0；base 的地址较小时 base 是 token0，否则取倒数
    const baseIsToken0 = BigInt(base) < BigInt(quote);
    const sqrtPriceX96 = baseIsToken0 ? sqrt(spec.price * Q192) : sqrt(Q192 / spec.price);
    let tick;
    if ((await read(() => pool.slot0())).sqrtPriceX96 === 0n) {
      const receipt = await (await pool.initialize(sqrtPriceX96)).wait();
      tick = Number(eventArgs(receipt, pool, "Initialize").tick);
    } else {
      tick = Number((await read(() => pool.slot0())).tick);
    }

    // 4. 流动性：宽区间打底 + 当前价附近的窄区间 + 上下各一个单边区间，
    //    这样大一点的 swap 会穿越已初始化的 tick
    const spacing = TICK_SPACING[spec.fee];
    const center = align(tick, spacing);
    const positions = [
      [center - 100 * spacing, center + 100 * spacing, spec.liquidity],
      [center - 10 * spacing, center + 10 * spacing, spec.liquidity * 4n],
      [center + 2 * spacing, center + 30 * spacing, spec.liquidity * 2n],
      [center - 30 * spacing, center - 2 * spacing, spec.liquidity * 2n],
    ];
    // 当前价位上已经有流动性，说明这个池子上次已经注入过，跳过
    const funded = (await read(() => pool.liquidity())) > 0n;
    for (const [tickLower, tickUpper, amount] of funded ? [] : positions) {
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
  //    测试网上用官方喂价，不部署模拟合约
  const feedAddr =
    realFeed || (await (await deploy("TestAggregator", 8, "ETH / USD", 2000n * 10n ** 8n)).getAddress());

  // 自检：链上 Quoter 报一下 1 WETH 换多少 USDC，确认池子真的能成交
  const path = ethers.solidityPacked(["address", "uint24", "address"], [tokens.WETH.address, 500, tokens.USDC.address]);
  const quoted = await read(() => quoter.quoteExactInput.staticCall(path, ethers.parseEther("1")));
  console.log(`自检：1 WETH -> ${ethers.formatEther(quoted)} USDC（0.05% 池）`);

  const result = {
    rpcUrl: process.env.RPC_URL || "http://127.0.0.1:8545",
    chainId,
    factory: factoryAddr,
    manager: managerAddr,
    quoter: await quoter.getAddress(),
    flashArbitrage: await flashArb.getAddress(),
    tokens: Object.fromEntries(Object.entries(tokens).map(([symbol, t]) => [symbol, t.address])),
    pools,
    chainlinkFeeds: `${tokens.WETH.address}:${tokens.USDC.address}:${feedAddr}:18:18:3600`,
    aggregator: feedAddr,
  };
  const spent = balanceBefore - (await ethers.provider.getBalance(deployer.address));
  console.log(`部署总花费：${ethers.formatEther(spent)} ETH`);
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
