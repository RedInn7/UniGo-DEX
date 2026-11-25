package chain

import (
	"context"
	"log/slog"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"unigo-dex/indexer"
)

// events.go — 合约事件解码
//
// 事件签名与 contracts/contracts/interfaces 下的 IUniGoPool.sol、IUniGoFactory.sol 一致。
// 日志的 topics[0] 是事件签名哈希，indexed 参数在 topics[1:]，其余参数 ABI 编码在 data 里。
// 解码结果是 indexer/events.go 中的事件结构，地址统一为小写十六进制。

const eventsABIJSON = `[
{"type":"event","name":"Mint","inputs":[
  {"name":"sender","type":"address","indexed":false},
  {"name":"owner","type":"address","indexed":true},
  {"name":"tickLower","type":"int24","indexed":true},
  {"name":"tickUpper","type":"int24","indexed":true},
  {"name":"amount","type":"uint128","indexed":false},
  {"name":"amount0","type":"uint256","indexed":false},
  {"name":"amount1","type":"uint256","indexed":false}]},
{"type":"event","name":"Burn","inputs":[
  {"name":"owner","type":"address","indexed":true},
  {"name":"tickLower","type":"int24","indexed":true},
  {"name":"tickUpper","type":"int24","indexed":true},
  {"name":"amount","type":"uint128","indexed":false},
  {"name":"amount0","type":"uint256","indexed":false},
  {"name":"amount1","type":"uint256","indexed":false}]},
{"type":"event","name":"Swap","inputs":[
  {"name":"sender","type":"address","indexed":true},
  {"name":"recipient","type":"address","indexed":true},
  {"name":"amount0","type":"int256","indexed":false},
  {"name":"amount1","type":"int256","indexed":false},
  {"name":"sqrtPriceX96","type":"uint160","indexed":false},
  {"name":"liquidity","type":"uint128","indexed":false},
  {"name":"tick","type":"int24","indexed":false}]},
{"type":"event","name":"Collect","inputs":[
  {"name":"owner","type":"address","indexed":true},
  {"name":"recipient","type":"address","indexed":false},
  {"name":"tickLower","type":"int24","indexed":true},
  {"name":"tickUpper","type":"int24","indexed":true},
  {"name":"amount0","type":"uint128","indexed":false},
  {"name":"amount1","type":"uint128","indexed":false}]},
{"type":"event","name":"Flash","inputs":[
  {"name":"sender","type":"address","indexed":true},
  {"name":"recipient","type":"address","indexed":true},
  {"name":"amount0","type":"uint256","indexed":false},
  {"name":"amount1","type":"uint256","indexed":false},
  {"name":"paid0","type":"uint256","indexed":false},
  {"name":"paid1","type":"uint256","indexed":false}]},
{"type":"event","name":"PoolCreated","inputs":[
  {"name":"token0","type":"address","indexed":true},
  {"name":"token1","type":"address","indexed":true},
  {"name":"fee","type":"uint24","indexed":true},
  {"name":"tickSpacing","type":"int24","indexed":false},
  {"name":"pool","type":"address","indexed":false}]}
]`

var (
	eventsABI   abi.ABI
	eventTopics []common.Hash // 所有关心的事件签名，用作 FilterLogs 的 topic0 过滤
)

func init() {
	var err error
	eventsABI, err = abi.JSON(strings.NewReader(eventsABIJSON))
	if err != nil {
		panic(err)
	}
	for _, ev := range eventsABI.Events {
		eventTopics = append(eventTopics, ev.ID)
	}
}

// BlockEvents 拉取并解码指定区块内的事件，按 logIndex 升序
//
// 用区块哈希过滤：区块若已被 Reorg 掉，节点会报错而不是返回另一条分叉上的日志。
func (c *Client) BlockEvents(ctx context.Context, block *indexer.BlockInfo) ([]interface{}, error) {
	// ponytail: 每个区块一次 FilterLogs，追大量历史区块偏慢；需要时改成按区块范围批量拉取
	hash := common.HexToHash(block.Hash)
	logs, err := c.FilterLogs(ctx, ethereum.FilterQuery{
		BlockHash: &hash,
		Topics:    [][]common.Hash{eventTopics},
	})
	if err != nil {
		return nil, err
	}

	events := make([]interface{}, 0, len(logs))
	for _, lg := range logs {
		event, err := c.decodeLog(lg, uint64(block.Timestamp.Unix()))
		if err != nil {
			// 任何合约都能发出签名相同但内容畸形的日志，跳过即可，不能卡住索引
			slog.Warn("跳过无法解码的日志", "address", lg.Address.Hex(), "tx", lg.TxHash.Hex(), "err", err)
			continue
		}
		if event != nil {
			events = append(events, event)
		}
	}
	return events, nil
}

// decodeLog 把一条原始日志解码为 indexer 的事件结构
//
// 不关心的日志返回 (nil, nil)。
func (c *Client) decodeLog(lg types.Log, timestamp uint64) (interface{}, error) {
	if lg.Removed || len(lg.Topics) == 0 {
		return nil, nil
	}
	ev, err := eventsABI.EventByID(lg.Topics[0])
	if err != nil {
		return nil, nil
	}

	args := make(map[string]interface{})
	if err := eventsABI.UnpackIntoMap(args, ev.Name, lg.Data); err != nil {
		return nil, err
	}
	var indexed abi.Arguments
	for _, in := range ev.Inputs {
		if in.Indexed {
			indexed = append(indexed, in)
		}
	}
	if err := abi.ParseTopicsIntoMap(args, indexed, lg.Topics[1:]); err != nil {
		return nil, err
	}

	addr := func(name string) string { return formatAddress(args[name].(common.Address)) }
	num := func(name string) *big.Int { return args[name].(*big.Int) }
	tick := func(name string) int32 { return int32(num(name).Int64()) }

	pool := formatAddress(lg.Address)
	txHash := lg.TxHash.Hex()

	switch ev.Name {
	case "Mint":
		return &indexer.MintEvent{
			PoolAddress: pool, Sender: addr("sender"), Owner: addr("owner"),
			TickLower: tick("tickLower"), TickUpper: tick("tickUpper"),
			Amount: num("amount"), Amount0: num("amount0"), Amount1: num("amount1"),
			BlockNumber: lg.BlockNumber, Timestamp: timestamp, TxHash: txHash, LogIndex: lg.Index,
		}, nil
	case "Burn":
		return &indexer.BurnEvent{
			PoolAddress: pool, Owner: addr("owner"),
			TickLower: tick("tickLower"), TickUpper: tick("tickUpper"),
			Amount: num("amount"), Amount0: num("amount0"), Amount1: num("amount1"),
			BlockNumber: lg.BlockNumber, Timestamp: timestamp, TxHash: txHash, LogIndex: lg.Index,
		}, nil
	case "Swap":
		return &indexer.SwapEvent{
			PoolAddress: pool, Sender: addr("sender"), Recipient: addr("recipient"),
			Amount0: num("amount0"), Amount1: num("amount1"),
			SqrtPriceX96: num("sqrtPriceX96"), Liquidity: num("liquidity"), Tick: tick("tick"),
			BlockNumber: lg.BlockNumber, Timestamp: timestamp, TxHash: txHash, LogIndex: lg.Index,
		}, nil
	case "Collect":
		return &indexer.CollectEvent{
			PoolAddress: pool, Owner: addr("owner"), Recipient: addr("recipient"),
			TickLower: tick("tickLower"), TickUpper: tick("tickUpper"),
			Amount0: num("amount0"), Amount1: num("amount1"),
			BlockNumber: lg.BlockNumber, Timestamp: timestamp, TxHash: txHash, LogIndex: lg.Index,
		}, nil
	case "Flash":
		return &indexer.FlashEvent{
			PoolAddress: pool, Sender: addr("sender"), Recipient: addr("recipient"),
			Amount0: num("amount0"), Amount1: num("amount1"),
			Paid0: num("paid0"), Paid1: num("paid1"),
			BlockNumber: lg.BlockNumber, Timestamp: timestamp, TxHash: txHash, LogIndex: lg.Index,
		}, nil
	case "PoolCreated":
		// 只信任 Factory 发出的 PoolCreated，否则任何人都能伪造池子
		if lg.Address != c.factory {
			return nil, nil
		}
		return &indexer.PoolCreatedEvent{
			Token0: addr("token0"), Token1: addr("token1"),
			Fee: uint32(num("fee").Uint64()), TickSpacing: tick("tickSpacing"), Pool: addr("pool"),
			BlockNumber: lg.BlockNumber, Timestamp: timestamp, TxHash: txHash,
		}, nil
	}
	return nil, nil
}

// formatAddress 地址统一为小写十六进制，作为链下状态的 key
func formatAddress(a common.Address) string {
	return strings.ToLower(a.Hex())
}
