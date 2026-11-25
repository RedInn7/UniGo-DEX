package indexer

import (
	"math/big"
)

// events.go — 链上事件类型定义
//
// 这些类型与 Solidity 合约中的 event 一一对应。
// 索引器从区块链捕获原始日志后，解析为这些结构体，
// 供 pool_state.go 用来重建链下状态。

// MintEvent 对应 UniGoPool.Mint 事件
// 当用户添加流动性时触发
type MintEvent struct {
	PoolAddress string   // 池子合约地址
	Sender      string   // 调用者（通常是 Manager 合约）
	Owner       string   // 流动性拥有者
	TickLower   int32    // position 下界 tick
	TickUpper   int32    // position 上界 tick
	Amount      *big.Int // 添加的流动性数量
	Amount0     *big.Int // 存入的 token0 数量
	Amount1     *big.Int // 存入的 token1 数量
	BlockNumber uint64   // 区块号
	Timestamp   uint64   // 区块时间戳（Unix 秒）
	TxHash      string   // 交易哈希
	LogIndex    uint     // 日志索引
}

// BurnEvent 对应 UniGoPool.Burn 事件
// 当用户移除流动性时触发
type BurnEvent struct {
	PoolAddress string
	Owner       string
	TickLower   int32
	TickUpper   int32
	Amount      *big.Int
	Amount0     *big.Int
	Amount1     *big.Int
	BlockNumber uint64
	Timestamp   uint64
	TxHash      string
	LogIndex    uint
}

// SwapEvent 对应 UniGoPool.Swap 事件
// 当用户执行代币交换时触发
type SwapEvent struct {
	PoolAddress  string
	Sender       string
	Recipient    string
	Amount0      *big.Int // token0 变化量（可正可负）
	Amount1      *big.Int // token1 变化量
	SqrtPriceX96 *big.Int // swap 后的价格
	Liquidity    *big.Int // swap 后的流动性
	Tick         int32    // swap 后的 tick
	BlockNumber  uint64
	Timestamp    uint64
	TxHash       string
	LogIndex     uint
}

// CollectEvent 对应 UniGoPool.Collect 事件
type CollectEvent struct {
	PoolAddress string
	Owner       string
	Recipient   string
	TickLower   int32
	TickUpper   int32
	Amount0     *big.Int
	Amount1     *big.Int
	BlockNumber uint64
	Timestamp   uint64
	TxHash      string
	LogIndex    uint
}

// FlashEvent 对应 UniGoPool.Flash 事件
// 闪电贷完成时触发，不改变价格和流动性
type FlashEvent struct {
	PoolAddress string
	Sender      string
	Recipient   string
	Amount0     *big.Int // 借出的 token0 数量
	Amount1     *big.Int // 借出的 token1 数量
	Paid0       *big.Int // 归还时多付的 token0（手续费）
	Paid1       *big.Int // 归还时多付的 token1（手续费）
	BlockNumber uint64
	Timestamp   uint64
	TxHash      string
	LogIndex    uint
}

// PoolCreatedEvent 对应 UniGoFactory.PoolCreated 事件
type PoolCreatedEvent struct {
	Token0      string
	Token1      string
	Fee         uint32
	TickSpacing int32
	Pool        string
	BlockNumber uint64
	Timestamp   uint64
	TxHash      string
}
