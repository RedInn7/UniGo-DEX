package chain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"unigo-dex/circuitbreaker"
	"unigo-dex/indexer"
	"unigo-dex/metrics"
)

// client.go — 基于 go-ethereum 的链交互客户端
//
// 封装 ethclient，对上层提供：
//   - 区块：最新区块号、按高度取区块头（真实 hash / parentHash / timestamp）
//   - 事件：按区块拉取日志并解码（见 events.go），实现 indexer.ChainSource
//   - 交易：回执查询与状态判定、广播已签名交易
//   - Gas：EIP-1559 的 baseFee 与建议小费
//
// 所有 RPC 调用都经过断路器：节点故障时快速失败，不让请求堆在超时上。

// Backend 用到的节点 RPC 方法
//
// *ethclient.Client 和 go-ethereum 的模拟后端（ethclient/simulated）都满足该接口。
type Backend interface {
	ethereum.BlockNumberReader
	ethereum.ChainReader
	ethereum.LogFilterer
	ethereum.TransactionReader
	ethereum.TransactionSender
	ethereum.GasPricer1559
	ethereum.ContractCaller
	ethereum.GasEstimator
	ethereum.ChainIDReader
}

var (
	// ErrNotFound 交易或区块在节点上不存在
	ErrNotFound = ethereum.NotFound
	// ErrInvalidHash 交易哈希格式不合法
	ErrInvalidHash = errors.New("invalid transaction hash")
)

// Client 链交互客户端
type Client struct {
	eth     Backend
	cb      *circuitbreaker.CircuitBreaker
	factory common.Address
}

// NewClient 用已有的节点连接创建客户端
//
// factory 是 Factory 合约地址，只有它发出的 PoolCreated 才会被采信。
func NewClient(eth Backend, factory common.Address) *Client {
	cfg := circuitbreaker.DefaultConfig()
	cfg.OnStateChange = func(from, to circuitbreaker.State) {
		slog.Warn("RPC 断路器状态变化", "from", from.String(), "to", to.String())
		if to == circuitbreaker.StateOpen {
			metrics.CircuitBreakerOpen.Set(1)
		} else {
			metrics.CircuitBreakerOpen.Set(0)
		}
	}
	return &Client{eth: eth, cb: circuitbreaker.New(cfg), factory: factory}
}

// Dial 连接 RPC 节点并做一次连通性检查
func Dial(ctx context.Context, rpcURL, factory string) (*Client, error) {
	eth, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	head, err := eth.HeaderByNumber(ctx, nil)
	if err != nil {
		eth.Close()
		return nil, err
	}
	// 区块哈希由本地按区块头字段复算。个别节点返回的区块头字段不全时复算结果
	// 会和节点自己的哈希对不上，父哈希校验会一直误报 Reorg，所以启动时先确认一次。
	if _, err := eth.HeaderByHash(ctx, head.Hash()); err != nil {
		eth.Close()
		return nil, fmt.Errorf("节点区块头与本地复算的哈希不一致: %w", err)
	}
	return NewClient(eth, common.HexToAddress(factory)), nil
}

// call 通过断路器执行一次 RPC 调用
//
// 只有节点不可达（连接失败、超时）才计入断路器的失败次数。
// "查不到"和节点明确返回的 JSON-RPC 错误（如交易被拒）说明节点是活的，
// 否则用户提交几笔坏交易就能把索引器一起熔断。
func call[T any](c *Client, fn func() (T, error)) (T, error) {
	var (
		value    T
		answered error // 节点给出的否定应答
	)
	_, err := c.cb.Execute(func() (interface{}, error) {
		var err error
		value, err = fn()
		var rpcErr rpc.Error
		if errors.Is(err, ethereum.NotFound) || errors.As(err, &rpcErr) {
			answered = err
			return nil, nil
		}
		return nil, err
	})
	if err == nil {
		err = answered
	}
	return value, err
}

// LatestBlock 返回最新区块号
func (c *Client) LatestBlock(ctx context.Context) (uint64, error) {
	return call(c, func() (uint64, error) { return c.eth.BlockNumber(ctx) })
}

// BlockByNumber 按高度取区块头
func (c *Client) BlockByNumber(ctx context.Context, number uint64) (*indexer.BlockInfo, error) {
	header, err := call(c, func() (*types.Header, error) {
		return c.eth.HeaderByNumber(ctx, new(big.Int).SetUint64(number))
	})
	if err != nil {
		return nil, err
	}
	return &indexer.BlockInfo{
		Number:     header.Number.Uint64(),
		Hash:       header.Hash().Hex(),
		ParentHash: header.ParentHash.Hex(),
		Timestamp:  time.Unix(int64(header.Time), 0),
	}, nil
}

// FilterLogs 按条件查询日志
func (c *Client) FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	return call(c, func() ([]types.Log, error) { return c.eth.FilterLogs(ctx, query) })
}

// Receipt 查询交易回执，交易未上链时返回 ErrNotFound
func (c *Client) Receipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	return call(c, func() (*types.Receipt, error) { return c.eth.TransactionReceipt(ctx, txHash) })
}

// Fees 返回 EIP-1559 口径的当前费用：最新区块 baseFee 与节点建议的小费
func (c *Client) Fees(ctx context.Context) (baseFee, tip *big.Int, err error) {
	header, err := call(c, func() (*types.Header, error) { return c.eth.HeaderByNumber(ctx, nil) })
	if err != nil {
		return nil, nil, err
	}
	if header.BaseFee == nil {
		return nil, nil, errors.New("节点所在链未启用 EIP-1559")
	}
	tip, err = call(c, func() (*big.Int, error) { return c.eth.SuggestGasTipCap(ctx) })
	if err != nil {
		return nil, nil, err
	}
	return header.BaseFee, tip, nil
}

// 交易状态
const (
	TxPending             = "pending"              // 在交易池中，尚未打包
	TxPendingConfirmation = "pending_confirmation" // 已打包，确认数不足
	TxConfirmed           = "confirmed"            // 确认数达到阈值
	TxReverted            = "reverted"             // 已打包但执行失败
)

// TxStatus 交易状态查询结果
type TxStatus struct {
	TxHash                string `json:"tx_hash"`
	Status                string `json:"status"`
	BlockNumber           uint64 `json:"block_number,omitempty"`
	Confirmations         uint64 `json:"confirmations"`
	RequiredConfirmations uint64 `json:"required_confirmations"`
	GasUsed               uint64 `json:"gas_used,omitempty"`
	EffectiveGasPrice     string `json:"effective_gas_price,omitempty"` // wei
}

// TxStatus 按回执和确认数判定交易状态
//
// required 是最终确认所需的区块数。节点完全不认识该交易时返回 ErrNotFound。
func (c *Client) TxStatus(ctx context.Context, txHash string, required uint64) (*TxStatus, error) {
	raw, err := hexutil.Decode(txHash)
	if err != nil || len(raw) != common.HashLength {
		return nil, ErrInvalidHash
	}
	hash := common.BytesToHash(raw)
	status := &TxStatus{TxHash: hash.Hex(), RequiredConfirmations: required}

	receipt, err := c.Receipt(ctx, hash)
	if errors.Is(err, ErrNotFound) || isTxIndexing(err) {
		// 没有回执：要么还在交易池里，要么节点根本没见过
		_, err = call(c, func() (*types.Transaction, error) {
			tx, _, err := c.eth.TransactionByHash(ctx, hash)
			return tx, err
		})
		if err != nil {
			return nil, err
		}
		status.Status = TxPending
		return status, nil
	}
	if err != nil {
		return nil, err
	}

	latest, err := c.LatestBlock(ctx)
	if err != nil {
		return nil, err
	}
	status.BlockNumber = receipt.BlockNumber.Uint64()
	if latest >= status.BlockNumber {
		status.Confirmations = latest - status.BlockNumber + 1
	}
	status.GasUsed = receipt.GasUsed
	if receipt.EffectiveGasPrice != nil {
		status.EffectiveGasPrice = receipt.EffectiveGasPrice.String()
	}

	switch {
	case receipt.Status == types.ReceiptStatusFailed:
		status.Status = TxReverted
	case status.Confirmations >= required:
		status.Status = TxConfirmed
	default:
		status.Status = TxPendingConfirmation
	}
	return status, nil
}

// isTxIndexing 节点的交易索引还没建完时，查不到的交易会报这个错而不是 NotFound
func isTxIndexing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "transaction indexing is in progress")
}

// RawTxInfo 一笔已签名交易的发送方和目标
type RawTxInfo struct {
	From string // 签名者地址（小写）
	To   string // 目标合约地址（小写）；合约创建交易为空
}

// decodeRawTx 解析已签名交易并恢复签名者
func decodeRawTx(rawTx string) (*types.Transaction, *RawTxInfo, error) {
	raw, err := hexutil.Decode(rawTx)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid raw transaction: %w", err)
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return nil, nil, fmt.Errorf("invalid raw transaction: %w", err)
	}
	from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid raw transaction signature: %w", err)
	}
	info := &RawTxInfo{From: formatAddress(from)}
	if tx.To() != nil {
		info.To = formatAddress(*tx.To())
	}
	return tx, info, nil
}

// InspectRawTx 解析已签名交易（0x 开头的十六进制），返回签名者和目标地址，不广播
func InspectRawTx(rawTx string) (*RawTxInfo, error) {
	_, info, err := decodeRawTx(rawTx)
	return info, err
}

// SendRawTransaction 广播一笔已签名的交易（0x 开头的十六进制），返回交易哈希
//
// 后端不托管私钥：交易由用户钱包签名，这里只负责转发。
func (c *Client) SendRawTransaction(ctx context.Context, rawTx string) (string, error) {
	tx, _, err := decodeRawTx(rawTx)
	if err != nil {
		return "", err
	}
	_, err = call(c, func() (struct{}, error) { return struct{}{}, c.eth.SendTransaction(ctx, tx) })
	if err != nil {
		return "", err
	}
	return tx.Hash().Hex(), nil
}
