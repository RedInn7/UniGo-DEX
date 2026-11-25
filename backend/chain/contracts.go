package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"

	"unigo-dex/chain/contractabi"
	umath "unigo-dex/math"
)

// contracts.go — 合约读调用与交易数据编码
//
// 读：slot0 / liquidity / observe（TWAP）/ Quoter 报价，全部走 eth_call。
// 写：只负责把参数编码成调用数据，交给用户钱包签名，后端不持有私钥。

// ErrTWAPUnavailable 池子的预言机观测覆盖的时间不够，算不出要求窗口的 TWAP
var ErrTWAPUnavailable = errors.New("twap unavailable: not enough oracle observations")

// callContract 对合约做一次只读调用并解码返回值
func (c *Client) callContract(ctx context.Context, to string, contract *abi.ABI, method string, args ...interface{}) ([]interface{}, error) {
	data, err := contract.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	addr := common.HexToAddress(to)
	out, err := call(c, func() ([]byte, error) {
		return c.eth.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: data}, nil)
	})
	if err != nil {
		return nil, err
	}
	return contract.Unpack(method, out)
}

// isRevert 节点执行了调用但合约 revert（区别于节点不可达）
func isRevert(err error) bool {
	var rpcErr rpc.Error
	return errors.As(err, &rpcErr)
}

// ChainID 返回链 ID
func (c *Client) ChainID(ctx context.Context) (*big.Int, error) {
	return call(c, func() (*big.Int, error) { return c.eth.ChainID(ctx) })
}

// Slot0 池子的 slot0：当前价格、tick 与预言机游标
type Slot0 struct {
	SqrtPriceX96               *big.Int
	Tick                       int32
	ObservationIndex           uint16
	ObservationCardinality     uint16
	ObservationCardinalityNext uint16
}

// Slot0 读取池子的 slot0
func (c *Client) Slot0(ctx context.Context, pool string) (*Slot0, error) {
	out, err := c.callContract(ctx, pool, &contractabi.Pool, "slot0")
	if err != nil {
		return nil, err
	}
	return &Slot0{
		SqrtPriceX96:               out[0].(*big.Int),
		Tick:                       int32(out[1].(*big.Int).Int64()),
		ObservationIndex:           out[3].(uint16),
		ObservationCardinality:     out[4].(uint16),
		ObservationCardinalityNext: out[5].(uint16),
	}, nil
}

// PoolLiquidity 读取池子当前的活跃流动性
func (c *Client) PoolLiquidity(ctx context.Context, pool string) (*big.Int, error) {
	out, err := c.callContract(ctx, pool, &contractabi.Pool, "liquidity")
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

// TWAP 时间加权平均价
type TWAP struct {
	Tick     int32   // 窗口内的算术平均 tick（向下取整）
	Price    float64 // 1.0001^Tick，即 tick 的几何平均价
	Window   uint32  // 实际使用的窗口（秒）
	Degraded bool    // 观测不够覆盖要求的窗口，退到了能覆盖的最长窗口
}

// TWAP 读取池子最近 window 秒的时间加权平均价
//
// 池子的预言机记录 tick 对时间的累计值，两个时刻的累计值之差除以间隔就是这段时间的平均 tick。
// 操纵 TWAP 需要把价格在整个窗口内都顶住，成本远高于在单个区块里拉一下现价。
//
// 降级：观测容量小或池子太新时，window 秒前的数据已经被覆盖（链上 revert "OLD"），
// 这时退到最老观测能覆盖的最长窗口；连 minWindow 都覆盖不了就返回 ErrTWAPUnavailable。
func (c *Client) TWAP(ctx context.Context, pool string, window, minWindow uint32) (*TWAP, error) {
	twap, err := c.observe(ctx, pool, window)
	if err == nil || !isRevert(err) {
		return twap, err
	}

	available, err := c.oldestObservationAge(ctx, pool)
	if err != nil {
		return nil, err
	}
	if available > window {
		available = window
	}
	if available < minWindow || available == 0 {
		return nil, fmt.Errorf("%w: 只覆盖最近 %d 秒，至少需要 %d 秒", ErrTWAPUnavailable, available, minWindow)
	}
	twap, err = c.observe(ctx, pool, available)
	if err != nil {
		if isRevert(err) {
			return nil, fmt.Errorf("%w: %v", ErrTWAPUnavailable, err)
		}
		return nil, err
	}
	twap.Degraded = true
	return twap, nil
}

// observe 调用池子的 observe([window, 0]) 并算出平均 tick
func (c *Client) observe(ctx context.Context, pool string, window uint32) (*TWAP, error) {
	out, err := c.callContract(ctx, pool, &contractabi.Pool, "observe", []uint32{window, 0})
	if err != nil {
		return nil, err
	}
	cumulatives := out[0].([]*big.Int)
	if len(cumulatives) != 2 {
		return nil, errors.New("observe returned unexpected data")
	}
	tick := AverageTick(cumulatives[0], cumulatives[1], window)
	return &TWAP{Tick: tick, Price: umath.TickToPrice(tick), Window: window}, nil
}

// AverageTick 由窗口两端的 tick 累计值算平均 tick，向下取整（负数时向负无穷）
func AverageTick(tickCumulativeStart, tickCumulativeEnd *big.Int, window uint32) int32 {
	delta := new(big.Int).Sub(tickCumulativeEnd, tickCumulativeStart)
	// big.Int 的 Div 是欧几里得除法，除数为正时就是向下取整
	return int32(delta.Div(delta, big.NewInt(int64(window))).Int64())
}

// oldestObservationAge 池子最老的一条观测距最新区块有多少秒
func (c *Client) oldestObservationAge(ctx context.Context, pool string) (uint32, error) {
	slot0, err := c.Slot0(ctx, pool)
	if err != nil {
		return 0, err
	}
	if slot0.ObservationCardinality == 0 {
		return 0, nil
	}
	// 环形数组里 index 的下一个位置就是最老的观测；还没写满时那个位置未初始化，最老的是 0 号
	timestamp, initialized, err := c.observation(ctx, pool, (uint64(slot0.ObservationIndex)+1)%uint64(slot0.ObservationCardinality))
	if err != nil {
		return 0, err
	}
	if !initialized {
		if timestamp, _, err = c.observation(ctx, pool, 0); err != nil {
			return 0, err
		}
	}
	latest, err := c.LatestBlock(ctx)
	if err != nil {
		return 0, err
	}
	block, err := c.BlockByNumber(ctx, latest)
	if err != nil {
		return 0, err
	}
	return uint32(block.Timestamp.Unix()) - timestamp, nil
}

func (c *Client) observation(ctx context.Context, pool string, index uint64) (timestamp uint32, initialized bool, err error) {
	out, err := c.callContract(ctx, pool, &contractabi.Pool, "observations", new(big.Int).SetUint64(index))
	if err != nil {
		return 0, false, err
	}
	return out[0].(uint32), out[3].(bool), nil
}

// QuoteExactInput 调用链上 Quoter 报价（eth_call 模拟，不上链）
func (c *Client) QuoteExactInput(ctx context.Context, quoter string, path []byte, amountIn *big.Int) (*big.Int, error) {
	out, err := c.callContract(ctx, quoter, &contractabi.Quoter, "quoteExactInput", path, amountIn)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

// EstimateGas 让节点估算一笔交易的 gas 用量
func (c *Client) EstimateGas(ctx context.Context, from, to string, data []byte) (uint64, error) {
	toAddr := common.HexToAddress(to)
	return call(c, func() (uint64, error) {
		return c.eth.EstimateGas(ctx, ethereum.CallMsg{From: common.HexToAddress(from), To: &toAddr, Data: data})
	})
}

// ===== 交易数据编码 =====

// EncodePath 编码 Manager / Quoter 的多跳路径
//
// 格式：token(20 字节) + fee(3 字节大端) + token(20 字节) + ...，按兑换方向排列。
func EncodePath(tokens []string, fees []uint32) ([]byte, error) {
	if len(tokens) < 2 || len(fees) != len(tokens)-1 {
		return nil, fmt.Errorf("invalid path: %d tokens, %d fees", len(tokens), len(fees))
	}
	path := make([]byte, 0, 20+23*len(fees))
	for i, token := range tokens {
		if !common.IsHexAddress(token) {
			return nil, fmt.Errorf("invalid token address %q", token)
		}
		path = append(path, common.HexToAddress(token).Bytes()...)
		if i < len(fees) {
			if fees[i] >= 1<<24 {
				return nil, fmt.Errorf("fee %d does not fit in 3 bytes", fees[i])
			}
			path = append(path, byte(fees[i]>>16), byte(fees[i]>>8), byte(fees[i]))
		}
	}
	return path, nil
}

// 与 Manager / FlashArbitrage 的结构体参数一一对应，字段名由 ABI 的参数名决定
type (
	swapParams struct {
		Pool              common.Address
		ZeroForOne        bool
		AmountSpecified   *big.Int
		SqrtPriceLimitX96 *big.Int
		Recipient         common.Address
		AmountOutMinimum  *big.Int
		Deadline          *big.Int
	}
	exactInputParams struct {
		Path             []byte
		Recipient        common.Address
		AmountIn         *big.Int
		AmountOutMinimum *big.Int
		Deadline         *big.Int
	}
	exactOutputSingleParams struct {
		TokenIn           common.Address
		TokenOut          common.Address
		Fee               *big.Int
		Recipient         common.Address
		AmountOut         *big.Int
		AmountInMaximum   *big.Int
		SqrtPriceLimitX96 *big.Int
		Deadline          *big.Int
	}
	arbitrageParams struct {
		Lender      common.Address
		SellPool    common.Address
		BuyPool     common.Address
		BorrowToken common.Address
		Amount      *big.Int
		MinProfit   *big.Int
	}
)

// EncodeSwap 编码 Manager.swap：单池精确输入，不限价
func EncodeSwap(pool string, zeroForOne bool, amountIn *big.Int, recipient string, amountOutMinimum *big.Int, deadline int64) ([]byte, error) {
	limit := new(big.Int).Sub(umath.MaxSqrtRatio, big.NewInt(1))
	if zeroForOne {
		limit = new(big.Int).Add(umath.MinSqrtRatio, big.NewInt(1))
	}
	return contractabi.Manager.Pack("swap", swapParams{
		Pool:              common.HexToAddress(pool),
		ZeroForOne:        zeroForOne,
		AmountSpecified:   amountIn,
		SqrtPriceLimitX96: limit,
		Recipient:         common.HexToAddress(recipient),
		AmountOutMinimum:  amountOutMinimum,
		Deadline:          big.NewInt(deadline),
	})
}

// EncodeExactInput 编码 Manager.exactInput：多跳精确输入
func EncodeExactInput(path []byte, recipient string, amountIn, amountOutMinimum *big.Int, deadline int64) ([]byte, error) {
	return contractabi.Manager.Pack("exactInput", exactInputParams{
		Path:             path,
		Recipient:        common.HexToAddress(recipient),
		AmountIn:         amountIn,
		AmountOutMinimum: amountOutMinimum,
		Deadline:         big.NewInt(deadline),
	})
}

// EncodeExactOutputSingle 编码 Manager.exactOutputSingle：单池精确输出，不限价
func EncodeExactOutputSingle(tokenIn, tokenOut string, fee uint32, recipient string, amountOut, amountInMaximum *big.Int, deadline int64) ([]byte, error) {
	return contractabi.Manager.Pack("exactOutputSingle", exactOutputSingleParams{
		TokenIn:           common.HexToAddress(tokenIn),
		TokenOut:          common.HexToAddress(tokenOut),
		Fee:               big.NewInt(int64(fee)),
		Recipient:         common.HexToAddress(recipient),
		AmountOut:         amountOut,
		AmountInMaximum:   amountInMaximum,
		SqrtPriceLimitX96: big.NewInt(0), // 0 = 不限价
		Deadline:          big.NewInt(deadline),
	})
}

// EncodeArbitrage 编码 FlashArbitrage.arbitrage
func EncodeArbitrage(lender, sellPool, buyPool, borrowToken string, amount, minProfit *big.Int) ([]byte, error) {
	return contractabi.FlashArbitrage.Pack("arbitrage", arbitrageParams{
		Lender:      common.HexToAddress(lender),
		SellPool:    common.HexToAddress(sellPool),
		BuyPool:     common.HexToAddress(buyPool),
		BorrowToken: common.HexToAddress(borrowToken),
		Amount:      amount,
		MinProfit:   minProfit,
	})
}
