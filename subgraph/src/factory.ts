import { BigDecimal, BigInt } from "@graphprotocol/graph-ts";
import { PoolCreated } from "../generated/Factory/UniGoFactory";
import { Pool as PoolTemplate } from "../generated/templates";
import { Factory, Pool } from "../generated/schema";

// 新池创建：登记 Pool 实体，并为它启动一个动态数据源
export function handlePoolCreated(event: PoolCreated): void {
  let factory = Factory.load(event.address);
  if (factory == null) {
    factory = new Factory(event.address);
    factory.poolCount = BigInt.zero();
    factory.txCount = BigInt.zero();
  }
  factory.poolCount = factory.poolCount.plus(BigInt.fromI32(1));
  factory.save();

  const pool = new Pool(event.params.pool);
  pool.factory = factory.id;
  pool.token0 = event.params.token0;
  pool.token1 = event.params.token1;
  pool.fee = event.params.fee;
  pool.tickSpacing = event.params.tickSpacing;
  // 池子创建时尚未初始化，价格在首次 mint / swap 时写入
  pool.sqrtPriceX96 = BigInt.zero();
  pool.tick = 0;
  pool.liquidity = BigInt.zero();
  pool.price = BigDecimal.zero();
  pool.volumeToken0 = BigInt.zero();
  pool.volumeToken1 = BigInt.zero();
  pool.flashFeeToken0 = BigInt.zero();
  pool.flashFeeToken1 = BigInt.zero();
  pool.txCount = BigInt.zero();
  pool.createdAtTimestamp = event.block.timestamp;
  pool.createdAtBlockNumber = event.block.number;
  pool.save();

  PoolTemplate.create(event.params.pool);
}
