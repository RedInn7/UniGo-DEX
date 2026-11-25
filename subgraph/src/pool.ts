import { Address, BigDecimal, BigInt, Bytes, ethereum } from "@graphprotocol/graph-ts";
import {
  Burn as BurnEvent,
  Flash as FlashEvent,
  Mint as MintEvent,
  Swap as SwapEvent,
  UniGoPool,
} from "../generated/templates/Pool/UniGoPool";
import { Burn, Candle, Factory, Flash, Mint, Pool, Swap } from "../generated/schema";

const ONE = BigInt.fromI32(1);
// 2^192：sqrtPriceX96 平方后的定点缩放因子
const Q192 = BigInt.fromI32(2).pow(192).toBigDecimal();

// K 线周期：名称与对应秒数一一对应
const INTERVAL_NAMES: string[] = ["1m", "5m", "1h", "1d"];
const INTERVAL_SECONDS: i32[] = [60, 300, 3600, 86400];

// 由 sqrtPriceX96 计算价格：price = (sqrtPriceX96 / 2^96)^2，即 1 个 token0 值多少 token1
// 结果是原始单位的比值，未按两个 token 的 decimals 换算
export function sqrtPriceX96ToPrice(sqrtPriceX96: BigInt): BigDecimal {
  return sqrtPriceX96.times(sqrtPriceX96).toBigDecimal().div(Q192);
}

// 事件实体的 id：交易哈希 + 日志序号
function eventId(event: ethereum.Event): Bytes {
  return event.transaction.hash.concatI32(event.logIndex.toI32());
}

// 池子和工厂的事件计数 +1
function countTx(pool: Pool): void {
  pool.txCount = pool.txCount.plus(ONE);
  const factory = Factory.load(pool.factory);
  if (factory != null) {
    factory.txCount = factory.txCount.plus(ONE);
    factory.save();
  }
}

// 从链上读取池子的最新价格和活跃流动性
// Mint / Burn 事件本身不带价格，且合约没有初始化事件，所以直接读合约状态
function refreshPoolState(pool: Pool, address: Address): void {
  const contract = UniGoPool.bind(address);
  const slot0 = contract.try_slot0();
  if (!slot0.reverted) {
    pool.sqrtPriceX96 = slot0.value.getSqrtPriceX96();
    pool.tick = slot0.value.getTick();
    pool.price = sqrtPriceX96ToPrice(pool.sqrtPriceX96);
  }
  const liquidity = contract.try_liquidity();
  if (!liquidity.reverted) {
    pool.liquidity = liquidity.value;
  }
}

// 用一笔成交更新所有周期的 K 线
// openPrice 为成交前价格，closePrice 为成交后价格
function updateCandles(
  pool: Pool,
  timestamp: i32,
  openPrice: BigDecimal,
  closePrice: BigDecimal,
  volume0: BigInt,
  volume1: BigInt
): void {
  for (let i = 0; i < INTERVAL_NAMES.length; i++) {
    const seconds = INTERVAL_SECONDS[i];
    const periodStart = (timestamp / seconds) * seconds;
    const id = pool.id.toHexString() + "-" + INTERVAL_NAMES[i] + "-" + periodStart.toString();

    let candle = Candle.load(id);
    if (candle == null) {
      candle = new Candle(id);
      candle.pool = pool.id;
      candle.interval = INTERVAL_NAMES[i];
      candle.periodStart = periodStart;
      candle.open = openPrice;
      candle.high = openPrice;
      candle.low = openPrice;
      candle.volumeToken0 = BigInt.zero();
      candle.volumeToken1 = BigInt.zero();
      candle.txCount = BigInt.zero();
    }
    if (closePrice.gt(candle.high)) candle.high = closePrice;
    if (closePrice.lt(candle.low)) candle.low = closePrice;
    candle.close = closePrice;
    candle.volumeToken0 = candle.volumeToken0.plus(volume0);
    candle.volumeToken1 = candle.volumeToken1.plus(volume1);
    candle.txCount = candle.txCount.plus(ONE);
    candle.save();
  }
}

export function handleSwap(event: SwapEvent): void {
  const pool = Pool.load(event.address);
  if (pool == null) return;

  const price = sqrtPriceX96ToPrice(event.params.sqrtPriceX96);
  // 成交前价格；池子价格未知时（理论上不会发生）退化为成交后价格
  const previousPrice = pool.sqrtPriceX96.isZero() ? price : pool.price;
  const volume0 = event.params.amount0.abs();
  const volume1 = event.params.amount1.abs();

  const swap = new Swap(eventId(event));
  swap.pool = pool.id;
  swap.sender = event.params.sender;
  swap.recipient = event.params.recipient;
  swap.amount0 = event.params.amount0;
  swap.amount1 = event.params.amount1;
  swap.sqrtPriceX96 = event.params.sqrtPriceX96;
  swap.liquidity = event.params.liquidity;
  swap.tick = event.params.tick;
  swap.price = price;
  swap.timestamp = event.block.timestamp;
  swap.blockNumber = event.block.number;
  swap.transactionHash = event.transaction.hash;
  swap.save();

  pool.sqrtPriceX96 = event.params.sqrtPriceX96;
  pool.tick = event.params.tick;
  pool.liquidity = event.params.liquidity;
  pool.price = price;
  pool.volumeToken0 = pool.volumeToken0.plus(volume0);
  pool.volumeToken1 = pool.volumeToken1.plus(volume1);
  countTx(pool);
  pool.save();

  updateCandles(pool, event.block.timestamp.toI32(), previousPrice, price, volume0, volume1);
}

export function handleMint(event: MintEvent): void {
  const pool = Pool.load(event.address);
  if (pool == null) return;

  const mint = new Mint(eventId(event));
  mint.pool = pool.id;
  mint.sender = event.params.sender;
  mint.owner = event.params.owner;
  mint.tickLower = event.params.tickLower;
  mint.tickUpper = event.params.tickUpper;
  mint.amount = event.params.amount;
  mint.amount0 = event.params.amount0;
  mint.amount1 = event.params.amount1;
  mint.timestamp = event.block.timestamp;
  mint.blockNumber = event.block.number;
  mint.transactionHash = event.transaction.hash;
  mint.save();

  refreshPoolState(pool, event.address);
  countTx(pool);
  pool.save();
}

export function handleBurn(event: BurnEvent): void {
  const pool = Pool.load(event.address);
  if (pool == null) return;

  const burn = new Burn(eventId(event));
  burn.pool = pool.id;
  burn.owner = event.params.owner;
  burn.tickLower = event.params.tickLower;
  burn.tickUpper = event.params.tickUpper;
  burn.amount = event.params.amount;
  burn.amount0 = event.params.amount0;
  burn.amount1 = event.params.amount1;
  burn.timestamp = event.block.timestamp;
  burn.blockNumber = event.block.number;
  burn.transactionHash = event.transaction.hash;
  burn.save();

  refreshPoolState(pool, event.address);
  countTx(pool);
  pool.save();
}

export function handleFlash(event: FlashEvent): void {
  const pool = Pool.load(event.address);
  if (pool == null) return;

  const flash = new Flash(eventId(event));
  flash.pool = pool.id;
  flash.sender = event.params.sender;
  flash.recipient = event.params.recipient;
  flash.amount0 = event.params.amount0;
  flash.amount1 = event.params.amount1;
  flash.paid0 = event.params.paid0;
  flash.paid1 = event.params.paid1;
  flash.timestamp = event.block.timestamp;
  flash.blockNumber = event.block.number;
  flash.transactionHash = event.transaction.hash;
  flash.save();

  pool.flashFeeToken0 = pool.flashFeeToken0.plus(event.params.paid0);
  pool.flashFeeToken1 = pool.flashFeeToken1.plus(event.params.paid1);
  countTx(pool);
  pool.save();
}
