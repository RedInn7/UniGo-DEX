/**
 * 压测数据灌入：在 scripts/deploy.js 部署好的环境上制造一批历史成交
 *
 * 用法（由 bench/up.sh 调用）：
 *   cd contracts && NODE_PATH=$PWD/node_modules DEPLOY_FILE=../bench/.run/deploy.json \
 *     npx hardhat run ../bench/seed.js --network localhost
 *
 * 环境变量：
 *   DEPLOY_FILE       deploy.js 写出的部署结果
 *   BENCH_SWAPS       成交笔数，默认 3000
 *   BENCH_PER_BLOCK   每个区块几笔，默认 5
 *   BENCH_BLOCK_SECS  区块间隔（秒），默认 12
 *
 * 三个测试账户随机在六个池子里做单池 swap，方向带均值回归（价格被推偏后更可能往回走），
 * 金额有大有小，大单会穿越已初始化的 tick。随机数用固定种子，每次灌入的数据相同。
 * 关掉自动出块，每个区块打包多笔交易，区块时间戳按固定间隔推进。
 */
const fs = require("fs");
const { ethers } = require("hardhat");

const SWAPS = Number(process.env.BENCH_SWAPS || 3000);
const PER_BLOCK = Number(process.env.BENCH_PER_BLOCK || 5);
const BLOCK_SECS = Number(process.env.BENCH_BLOCK_SECS || 12);
const MIN_SQRT_RATIO_PLUS_ONE = 4295128740n;
const MAX_SQRT_RATIO_MINUS_ONE = 1461446703485210103287273052203988822378723970341n;
const DEADLINE = 4102444800n;

// mulberry32：固定种子的伪随机数
function rng(seed) {
  return () => {
    seed |= 0;
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// 各 token 单笔成交的金额范围（整数个 token）
const RANGE = { WETH: [0.05, 25], USDC: [100, 50000], DAI: [100, 50000], WBTC: [0.002, 0.8] };

async function main() {
  const deployment = JSON.parse(fs.readFileSync(process.env.DEPLOY_FILE));
  const random = rng(20240601);
  const signers = await ethers.getSigners();
  const traders = signers.slice(1, 4);
  const manager = await ethers.getContractAt("UniGoManager", deployment.manager);
  const provider = ethers.provider;

  const symbolOf = Object.fromEntries(Object.entries(deployment.tokens).map(([s, a]) => [a.toLowerCase(), s]));
  const pools = [];
  for (const p of deployment.pools) {
    const pool = await ethers.getContractAt("UniGoPool", p.address);
    pools.push({
      address: p.address,
      token0: symbolOf[(await pool.token0()).toLowerCase()],
      token1: symbolOf[(await pool.token1()).toLowerCase()],
      drift: 0, // token0 净流入的笔数：正数说明价格被往下推了
    });
  }

  const nonces = new Map();
  for (const t of traders) nonces.set(t.address, await provider.getTransactionCount(t.address));

  await provider.send("evm_setAutomine", [false]);
  let timestamp = (await provider.getBlock("latest")).timestamp;
  const started = Date.now();
  let sent = 0;
  let failed = 0;

  while (sent < SWAPS) {
    const hashes = [];
    for (let i = 0; i < PER_BLOCK && sent < SWAPS; i++, sent++) {
      const pool = pools[Math.floor(random() * pools.length)];
      const trader = traders[Math.floor(random() * traders.length)];
      // 均值回归：偏离越多，往回走的概率越大
      const pBack = Math.min(0.9, 0.5 + 0.08 * Math.abs(pool.drift));
      const zeroForOne = pool.drift > 0 ? random() > pBack : pool.drift < 0 ? random() < pBack : random() < 0.5;
      pool.drift += zeroForOne ? 1 : -1;

      const [lo, hi] = RANGE[zeroForOne ? pool.token0 : pool.token1];
      // 对数均匀分布：小单多、大单少
      const amount = lo * Math.pow(hi / lo, random());
      const tx = await manager.connect(trader).swap(
        {
          pool: pool.address,
          zeroForOne,
          amountSpecified: ethers.parseEther(amount.toFixed(6)),
          sqrtPriceLimitX96: zeroForOne ? MIN_SQRT_RATIO_PLUS_ONE : MAX_SQRT_RATIO_MINUS_ONE,
          recipient: trader.address,
          amountOutMinimum: 0,
          deadline: DEADLINE,
        },
        { nonce: nonces.get(trader.address), gasLimit: 3000000 }
      );
      nonces.set(trader.address, nonces.get(trader.address) + 1);
      hashes.push(tx.hash);
    }
    timestamp += BLOCK_SECS;
    await provider.send("evm_setNextBlockTimestamp", [timestamp]);
    await provider.send("evm_mine", []);
    for (const hash of hashes) {
      const receipt = await provider.getTransactionReceipt(hash);
      if (!receipt || receipt.status !== 1) {
        failed++;
        if (failed <= 3) {
          const tx = await provider.getTransaction(hash);
          let reason = receipt ? "reverted" : "not mined";
          if (receipt) {
            try {
              await provider.call({ from: tx.from, to: tx.to, data: tx.data, blockTag: receipt.blockNumber - 1 });
            } catch (e) {
              reason = e.shortMessage || e.message;
            }
          }
          console.log(`失败的交易 ${hash}: ${reason}，gasUsed=${receipt ? receipt.gasUsed : "-"}`);
        }
      }
    }
  }
  await provider.send("evm_setAutomine", [true]);

  // 让 Chainlink 喂价与池子现价对齐（灌完数据后价格已经随机游走过）
  const wethUsdc = await ethers.getContractAt("UniGoPool", deployment.pools[0].address);
  const sqrtPriceX96 = (await wethUsdc.slot0()).sqrtPriceX96;
  const price1per0 = Number((sqrtPriceX96 * sqrtPriceX96 * 10n ** 12n) / 2n ** 192n) / 1e12;
  const wethIsToken0 = (await wethUsdc.token0()).toLowerCase() === deployment.tokens.WETH.toLowerCase();
  const ethUsd = wethIsToken0 ? price1per0 : 1 / price1per0;
  const feed = await ethers.getContractAt("TestAggregator", deployment.aggregator);
  await (await feed.setAnswer(BigInt(Math.round(ethUsd * 1e8)))).wait();

  // 再空出 700 秒：所有池子的 TWAP 窗口（默认 600 秒）都落在最后一笔成交之后
  const latest = (await provider.getBlock("latest")).timestamp;
  await provider.send("evm_setNextBlockTimestamp", [latest + 700]);
  await (await feed.setAnswer(BigInt(Math.round(ethUsd * 1e8)))).wait();

  const head = await provider.getBlock("latest");
  const summary = {
    swaps: SWAPS,
    failed,
    perBlock: PER_BLOCK,
    blockSeconds: BLOCK_SECS,
    headBlock: head.number,
    headTimestamp: head.timestamp,
    ethUsd,
    seedSeconds: (Date.now() - started) / 1000,
  };
  fs.writeFileSync(process.env.DEPLOY_FILE.replace(/deploy\.json$/, "seed.json"), JSON.stringify(summary, null, 2) + "\n");
  console.log("灌入完成:", JSON.stringify(summary));
  if (failed > 0) throw new Error(`${failed} 笔 swap 执行失败`);
}

main()
  .then(() => process.exit(0))
  .catch((error) => {
    console.error("灌入失败:", error);
    process.exit(1);
  });
