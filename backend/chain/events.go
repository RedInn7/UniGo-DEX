package chain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"unigo-dex/chain/contractabi"
	"unigo-dex/indexer"
)

// events.go — 合约事件解码
//
// 事件定义取自内嵌的合约 ABI（contractabi 包，由 Hardhat 编译产物生成），不手写签名。
// 日志的 topics[0] 是事件签名哈希，indexed 参数在 topics[1:]，其余参数 ABI 编码在 data 里。
// 解码结果是 indexer/events.go 中的事件结构，地址统一为小写十六进制。

// 关心的事件签名，用作 FilterLogs 的 topic0 过滤
var eventTopics = func() []common.Hash {
	var topics []common.Hash
	for _, name := range []string{"Initialize", "Mint", "Burn", "Swap", "Collect", "Flash"} {
		topics = append(topics, contractabi.Pool.Events[name].ID)
	}
	return append(topics, contractabi.Factory.Events["PoolCreated"].ID)
}()

// eventByID 按签名哈希查找 Pool / Factory 的事件定义
func eventByID(id common.Hash) (*abi.Event, bool) {
	if ev, err := contractabi.Pool.EventByID(id); err == nil {
		return ev, true
	}
	if ev, err := contractabi.Factory.EventByID(id); err == nil {
		return ev, true
	}
	return nil, false
}

// ErrReorged 读取日志期间链发生了重组：日志所属的区块已经不是调用方手里的那个区块
var ErrReorged = errors.New("chain reorganized while reading logs")

// BlockEvents 一次 eth_getLogs 拉取一段连续区块内的事件并解码
//
// 结果与 blocks 一一对应，每个区块内按 logIndex 升序。
// 按区块号范围查询拿到的是节点当前主链上的日志，所以要逐条核对日志的 blockHash
// 与调用方给的区块头一致；对不上说明这期间发生了重组，返回 ErrReorged 让调用方重读。
func (c *Client) BlockEvents(ctx context.Context, blocks []*indexer.BlockInfo) ([][]interface{}, error) {
	events := make([][]interface{}, len(blocks))
	if len(blocks) == 0 {
		return events, nil
	}
	first, last := blocks[0].Number, blocks[len(blocks)-1].Number
	logs, err := c.rangeLogs(ctx, first, last)
	if err != nil {
		return nil, err
	}

	for _, lg := range logs {
		if lg.BlockNumber < first || lg.BlockNumber > last {
			return nil, fmt.Errorf("节点返回了查询范围之外的日志（区块 %d）", lg.BlockNumber)
		}
		block := blocks[lg.BlockNumber-first]
		if lg.BlockHash.Hex() != block.Hash {
			return nil, fmt.Errorf("%w: 区块 %d", ErrReorged, lg.BlockNumber)
		}
		event, err := c.decodeLog(lg, uint64(block.Timestamp.Unix()))
		if err != nil {
			// 任何合约都能发出签名相同但内容畸形的日志，跳过即可，不能卡住索引
			slog.Warn("跳过无法解码的日志", "address", lg.Address.Hex(), "tx", lg.TxHash.Hex(), "err", err)
			continue
		}
		if event != nil {
			i := lg.BlockNumber - first
			events[i] = append(events[i], event)
		}
	}
	return events, nil
}

// rangeLogs 拉取 [first, last] 内 Factory 和所有已知池子的日志，按 (区块, logIndex) 升序
//
// 查询必须带地址过滤：公共节点会拒绝不带地址的 eth_getLogs，而且全链同签名的日志
// 量很大。所以分两步，先查 Factory 的 PoolCreated 把这段区块里新建的池子记下来，
// 再按池子地址查其余事件，这样池子创建和它的首批事件落在同一批区块里也不会漏。
// 没配 Factory 地址时（单元测试里的模拟链）退回按事件签名查全部地址。
func (c *Client) rangeLogs(ctx context.Context, first, last uint64) ([]types.Log, error) {
	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(first),
		ToBlock:   new(big.Int).SetUint64(last),
	}
	if c.factory == (common.Address{}) {
		query.Topics = [][]common.Hash{eventTopics}
		return c.FilterLogs(ctx, query)
	}

	created := contractabi.Factory.Events["PoolCreated"]
	query.Addresses = []common.Address{c.factory}
	query.Topics = [][]common.Hash{{created.ID}}
	logs, err := c.FilterLogs(ctx, query)
	if err != nil {
		return nil, err
	}

	c.poolsMu.Lock()
	for _, lg := range logs {
		args := make(map[string]interface{})
		if err := created.Inputs.UnpackIntoMap(args, lg.Data); err != nil {
			continue
		}
		if pool, ok := args["pool"].(common.Address); ok && !slices.Contains(c.pools, pool) {
			c.pools = append(c.pools, pool)
		}
	}
	// ponytail: 池子地址全量放进一次查询，池子多到超过节点的地址数上限时再分批
	pools := slices.Clone(c.pools)
	c.poolsMu.Unlock()
	if len(pools) == 0 {
		return logs, nil
	}

	// PoolCreated 上一步已经查过，这里只要池子自己的事件（eventTopics 的最后一项是 PoolCreated）
	query.Addresses = pools
	query.Topics = [][]common.Hash{eventTopics[:len(eventTopics)-1]}
	poolLogs, err := c.FilterLogs(ctx, query)
	if err != nil {
		return nil, err
	}
	logs = append(logs, poolLogs...)
	slices.SortFunc(logs, func(a, b types.Log) int {
		return cmp.Or(cmp.Compare(a.BlockNumber, b.BlockNumber), cmp.Compare(a.Index, b.Index))
	})
	return logs, nil
}

// decodeLog 把一条原始日志解码为 indexer 的事件结构
//
// 不关心的日志返回 (nil, nil)。
func (c *Client) decodeLog(lg types.Log, timestamp uint64) (interface{}, error) {
	if lg.Removed || len(lg.Topics) == 0 {
		return nil, nil
	}
	ev, ok := eventByID(lg.Topics[0])
	if !ok {
		return nil, nil
	}

	args := make(map[string]interface{})
	if err := ev.Inputs.UnpackIntoMap(args, lg.Data); err != nil {
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
	case "Initialize":
		return &indexer.InitializeEvent{
			PoolAddress: pool, SqrtPriceX96: num("sqrtPriceX96"), Tick: tick("tick"),
			BlockNumber: lg.BlockNumber, Timestamp: timestamp, TxHash: txHash, LogIndex: lg.Index,
		}, nil
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
