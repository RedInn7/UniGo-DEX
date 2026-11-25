package chain

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"

	"unigo-dex/chain/contractabi"
	"unigo-dex/circuitbreaker"
	"unigo-dex/indexer"
)

// 测试全部跑在 go-ethereum 的模拟后端上，不依赖外部节点。
//
// 模拟链的创世状态里预置了三个手写字节码的合约：
//   - factoryAddr：把 calldata 的前 4 个 32 字节当作 topics、其余当作 data 发出 LOG4
//   - poolAddr：同上，发出 LOG3（Swap / Flash 是 3 个 topic）
//   - reverterAddr：任何调用都 revert
// 这样无需编译 Solidity 就能在链上产生与合约一致的事件日志。

var (
	factoryAddr  = common.HexToAddress("0x00000000000000000000000000000000000fac70")
	poolAddr     = common.HexToAddress("0x0000000000000000000000000000000000009001")
	reverterAddr = common.HexToAddress("0x000000000000000000000000000000000000dead")

	log4Code   = common.FromHex("60803603806080600037606035604035602035600035846000a400")
	log3Code   = common.FromHex("60603603806060600037604035602035600035836000a300")
	revertCode = common.FromHex("60006000fd")

	userA = common.HexToAddress("0x00000000000000000000000000000000000000a1")
	userB = common.HexToAddress("0x00000000000000000000000000000000000000b2")
)

type env struct {
	t       *testing.T
	backend *simulated.Backend
	client  *Client
	key     *ecdsa.PrivateKey
	from    common.Address
}

func newEnv(t *testing.T) *env {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	backend := simulated.NewBackend(types.GenesisAlloc{
		from:         {Balance: new(big.Int).Lsh(big.NewInt(1), 80)},
		factoryAddr:  {Code: log4Code, Balance: big.NewInt(0)},
		poolAddr:     {Code: log3Code, Balance: big.NewInt(0)},
		reverterAddr: {Code: revertCode, Balance: big.NewInt(0)},
	})
	t.Cleanup(func() { backend.Close() })
	e := &env{t: t, backend: backend, client: NewClient(backend.Client(), factoryAddr), key: key, from: from}

	// 先出一个区块并等节点的交易索引就绪，之后"查不到交易"才会稳定返回 NotFound
	backend.Commit()
	waitFor(t, "tx indexer", func() bool {
		_, err := e.client.Receipt(context.Background(), common.HexToHash("0x01"))
		return errors.Is(err, ErrNotFound)
	})
	return e
}

// send 签名并发送一笔交易（不出块）
func (e *env) send(to common.Address, data []byte) *types.Transaction {
	e.t.Helper()
	ctx := context.Background()
	nonce, err := e.backend.Client().PendingNonceAt(ctx, e.from)
	if err != nil {
		e.t.Fatal(err)
	}
	tx, err := types.SignNewTx(e.key, types.LatestSignerForChainID(big.NewInt(1337)), &types.DynamicFeeTx{
		ChainID:   big.NewInt(1337),
		Nonce:     nonce,
		GasTipCap: big.NewInt(1e9),
		GasFeeCap: big.NewInt(100e9),
		Gas:       300000,
		To:        &to,
		Data:      data,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.backend.Client().SendTransaction(ctx, tx); err != nil {
		e.t.Fatal(err)
	}
	return tx
}

// emit 让 to 合约发出一条指定事件的日志
func (e *env) emit(to common.Address, event string, indexed []interface{}, nonIndexed ...interface{}) *types.Transaction {
	e.t.Helper()
	ev, ok := contractabi.Pool.Events[event]
	if !ok {
		ev = contractabi.Factory.Events[event]
	}
	data := ev.ID.Bytes()
	for _, v := range indexed {
		topics, err := abi.MakeTopics([]interface{}{v})
		if err != nil {
			e.t.Fatal(err)
		}
		data = append(data, topics[0][0].Bytes()...)
	}
	packed, err := ev.Inputs.NonIndexed().Pack(nonIndexed...)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.send(to, append(data, packed...))
}

func (e *env) emitSwap(sender, recipient common.Address, amount0, amount1 int64, tick int64) *types.Transaction {
	return e.emit(poolAddr, "Swap", []interface{}{sender, recipient},
		big.NewInt(amount0), big.NewInt(amount1), new(big.Int).Lsh(big.NewInt(1), 96), big.NewInt(777), big.NewInt(tick))
}

func (e *env) emitPoolCreated() *types.Transaction {
	return e.emit(factoryAddr, "PoolCreated", []interface{}{userA, userB, big.NewInt(3000)}, big.NewInt(60), poolAddr)
}

// events 返回最新区块里解码出的事件
func (e *env) events() ([]interface{}, *indexer.BlockInfo) {
	e.t.Helper()
	ctx := context.Background()
	latest, err := e.client.LatestBlock(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	block, err := e.client.BlockByNumber(ctx, latest)
	if err != nil {
		e.t.Fatal(err)
	}
	events, err := e.client.BlockEvents(ctx, []*indexer.BlockInfo{block})
	if err != nil {
		e.t.Fatal(err)
	}
	return events[0], block
}

func TestBlockHeaders(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h1 := e.backend.Commit()
	h2 := e.backend.Commit()

	latest, err := e.client.LatestBlock(ctx)
	if err != nil || latest != 3 {
		t.Fatalf("latest = %d, err = %v", latest, err)
	}
	b1, err := e.client.BlockByNumber(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := e.client.BlockByNumber(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Hash != h1.Hex() || b2.Hash != h2.Hex() {
		t.Fatalf("hash mismatch: %s %s", b1.Hash, b2.Hash)
	}
	if b2.ParentHash != b1.Hash {
		t.Fatalf("parentHash = %s, want %s", b2.ParentHash, b1.Hash)
	}
	if b2.Number != 3 || b2.Timestamp.Before(b1.Timestamp) || b1.Timestamp.Unix() == 0 {
		t.Fatalf("bad block info: %+v", b2)
	}
	if _, err := e.client.BlockByNumber(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing block err = %v, want ErrNotFound", err)
	}
}

func TestDecodePoolEvents(t *testing.T) {
	e := newEnv(t)

	// 一个区块里放 Swap + Flash，检查顺序、字段和时间戳
	swapTx := e.emitSwap(userA, userB, 1000, -990, -12)
	e.emit(poolAddr, "Flash", []interface{}{userA, userB}, big.NewInt(5), big.NewInt(6), big.NewInt(1), big.NewInt(2))
	e.backend.Commit()

	events, block := e.events()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	swap, ok := events[0].(*indexer.SwapEvent)
	if !ok {
		t.Fatalf("events[0] is %T", events[0])
	}
	if swap.PoolAddress != formatAddress(poolAddr) || swap.Sender != formatAddress(userA) || swap.Recipient != formatAddress(userB) {
		t.Fatalf("swap addresses: %+v", swap)
	}
	if swap.Amount0.Int64() != 1000 || swap.Amount1.Int64() != -990 || swap.Tick != -12 || swap.Liquidity.Int64() != 777 {
		t.Fatalf("swap values: %+v", swap)
	}
	if swap.SqrtPriceX96.Cmp(new(big.Int).Lsh(big.NewInt(1), 96)) != 0 {
		t.Fatalf("sqrtPriceX96 = %s", swap.SqrtPriceX96)
	}
	if swap.TxHash != swapTx.Hash().Hex() || swap.BlockNumber != block.Number || swap.LogIndex != 0 {
		t.Fatalf("swap position: %+v", swap)
	}
	if swap.Timestamp != uint64(block.Timestamp.Unix()) || swap.Timestamp == 0 {
		t.Fatalf("swap timestamp = %d", swap.Timestamp)
	}
	flash, ok := events[1].(*indexer.FlashEvent)
	if !ok || flash.Amount1.Int64() != 6 || flash.Paid0.Int64() != 1 || flash.LogIndex != 1 {
		t.Fatalf("flash: %+v", events[1])
	}

	// 4 个 topic 的事件：Mint / Burn / Collect（测试里由 LOG4 合约代发）
	e.emit(factoryAddr, "Mint", []interface{}{userB, big.NewInt(-120), big.NewInt(180)},
		userA, big.NewInt(500), big.NewInt(11), big.NewInt(22))
	e.emit(factoryAddr, "Burn", []interface{}{userB, big.NewInt(-120), big.NewInt(180)},
		big.NewInt(200), big.NewInt(3), big.NewInt(4))
	e.emit(factoryAddr, "Collect", []interface{}{userB, big.NewInt(-120), big.NewInt(180)},
		userA, big.NewInt(7), big.NewInt(8))
	e.backend.Commit()

	events, _ = e.events()
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	mint := events[0].(*indexer.MintEvent)
	if mint.Sender != formatAddress(userA) || mint.Owner != formatAddress(userB) ||
		mint.TickLower != -120 || mint.TickUpper != 180 ||
		mint.Amount.Int64() != 500 || mint.Amount0.Int64() != 11 || mint.Amount1.Int64() != 22 {
		t.Fatalf("mint: %+v", mint)
	}
	burn := events[1].(*indexer.BurnEvent)
	if burn.Owner != formatAddress(userB) || burn.TickLower != -120 || burn.Amount.Int64() != 200 || burn.Amount1.Int64() != 4 {
		t.Fatalf("burn: %+v", burn)
	}
	collect := events[2].(*indexer.CollectEvent)
	if collect.Recipient != formatAddress(userA) || collect.TickUpper != 180 || collect.Amount0.Int64() != 7 || collect.Amount1.Int64() != 8 {
		t.Fatalf("collect: %+v", collect)
	}
}

func TestDecodePoolCreatedOnlyFromFactory(t *testing.T) {
	e := newEnv(t)
	e.emitPoolCreated()
	e.backend.Commit()

	events, _ := e.events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	created := events[0].(*indexer.PoolCreatedEvent)
	if created.Token0 != formatAddress(userA) || created.Token1 != formatAddress(userB) ||
		created.Fee != 3000 || created.TickSpacing != 60 || created.Pool != formatAddress(poolAddr) {
		t.Fatalf("poolCreated: %+v", created)
	}

	// 同样的日志由别的合约发出时不采信
	other := NewClient(e.backend.Client(), userA)
	latest, _ := other.LatestBlock(context.Background())
	block, _ := other.BlockByNumber(context.Background(), latest)
	fromOther, err := other.BlockEvents(context.Background(), []*indexer.BlockInfo{block})
	events = fromOther[0]
	if err != nil || len(events) != 0 {
		t.Fatalf("events = %v, err = %v; want none", events, err)
	}
}

// 一次查询拉取一段区块：事件按区块分好；区块哈希对不上（读取期间重组）时报错而不是张冠李戴
func TestBlockEventsRange(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.emitSwap(userA, userB, 1, -1, 11)
	e.backend.Commit() // 区块 2：1 笔
	e.backend.Commit() // 区块 3：空
	e.emitSwap(userA, userB, 2, -2, 22)
	e.emitSwap(userB, userA, 3, -3, 33)
	e.backend.Commit() // 区块 4：2 笔

	var blocks []*indexer.BlockInfo
	for n := uint64(1); n <= 4; n++ {
		block, err := e.client.BlockByNumber(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, block)
	}
	events, err := e.client.BlockEvents(ctx, blocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || len(events[0]) != 0 || len(events[1]) != 1 || len(events[2]) != 0 || len(events[3]) != 2 {
		t.Fatalf("events per block: %d %d %d %d", len(events[0]), len(events[1]), len(events[2]), len(events[3]))
	}
	first, last := events[1][0].(*indexer.SwapEvent), events[3][1].(*indexer.SwapEvent)
	if first.Tick != 11 || first.BlockNumber != 2 || first.Timestamp != uint64(blocks[1].Timestamp.Unix()) {
		t.Fatalf("block 2 swap: %+v", first)
	}
	if last.Tick != 33 || last.BlockNumber != 4 || last.LogIndex != 1 || last.Timestamp != uint64(blocks[3].Timestamp.Unix()) {
		t.Fatalf("block 4 second swap: %+v", last)
	}

	// 调用方手里的区块 4 已经不是链上的那个（哈希不同）：必须报 ErrReorged
	stale := *blocks[3]
	stale.Hash = blocks[2].Hash
	if _, err := e.client.BlockEvents(ctx, []*indexer.BlockInfo{blocks[2], &stale}); !errors.Is(err, ErrReorged) {
		t.Fatalf("err = %v, want ErrReorged", err)
	}
	if events, err := e.client.BlockEvents(ctx, nil); err != nil || len(events) != 0 {
		t.Fatalf("empty range: %v %v", events, err)
	}
}

func TestMalformedLogIsSkipped(t *testing.T) {
	e := newEnv(t)
	// 签名是 Swap，但 data 为空
	swap := contractabi.Pool.Events["Swap"]
	data := append(swap.ID.Bytes(), make([]byte, 64)...)
	e.send(poolAddr, data)
	e.emitSwap(userA, userB, 1, -1, 0)
	e.backend.Commit()

	events, _ := e.events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (畸形日志应被跳过)", len(events))
	}
}

func TestTxStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const required = 3

	tx := e.emitSwap(userA, userB, 1, -1, 0)
	hash := tx.Hash().Hex()

	st, err := e.client.TxStatus(ctx, hash, required)
	if err != nil || st.Status != TxPending {
		t.Fatalf("before mining: %+v, err = %v", st, err)
	}

	e.backend.Commit()
	st, err = e.client.TxStatus(ctx, hash, required)
	if err != nil || st.Status != TxPendingConfirmation || st.Confirmations != 1 || st.BlockNumber != 2 {
		t.Fatalf("after 1 block: %+v, err = %v", st, err)
	}
	if st.GasUsed == 0 || st.EffectiveGasPrice == "" || st.RequiredConfirmations != required {
		t.Fatalf("receipt fields missing: %+v", st)
	}

	e.backend.Commit()
	e.backend.Commit()
	st, err = e.client.TxStatus(ctx, hash, required)
	if err != nil || st.Status != TxConfirmed || st.Confirmations != 3 {
		t.Fatalf("after 3 blocks: %+v, err = %v", st, err)
	}

	reverted := e.send(reverterAddr, nil)
	e.backend.Commit()
	st, err = e.client.TxStatus(ctx, reverted.Hash().Hex(), required)
	if err != nil || st.Status != TxReverted {
		t.Fatalf("reverted tx: %+v, err = %v", st, err)
	}

	if _, err := e.client.TxStatus(ctx, common.HexToHash("0x1234").Hex(), required); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tx err = %v, want ErrNotFound", err)
	}
	if _, err := e.client.TxStatus(ctx, "0x1234", required); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("short hash err = %v, want ErrInvalidHash", err)
	}
}

func TestSendRawTransaction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tx, err := types.SignNewTx(e.key, types.LatestSignerForChainID(big.NewInt(1337)), &types.DynamicFeeTx{
		ChainID: big.NewInt(1337), GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(100e9), Gas: 21000, To: &userA, Value: big.NewInt(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := tx.MarshalBinary()

	hash, err := e.client.SendRawTransaction(ctx, hexutil.Encode(raw))
	if err != nil || hash != tx.Hash().Hex() {
		t.Fatalf("hash = %s, err = %v", hash, err)
	}
	e.backend.Commit()
	st, err := e.client.TxStatus(ctx, hash, 1)
	if err != nil || st.Status != TxConfirmed {
		t.Fatalf("status: %+v, err = %v", st, err)
	}

	if _, err := e.client.SendRawTransaction(ctx, "0xdeadbeef"); err == nil {
		t.Fatal("garbage raw tx should be rejected")
	}
}

func TestFees(t *testing.T) {
	e := newEnv(t)
	e.backend.Commit()
	baseFee, tip, err := e.client.Fees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if baseFee.Sign() <= 0 || tip.Sign() <= 0 {
		t.Fatalf("baseFee = %s, tip = %s", baseFee, tip)
	}
}

// failingBackend 所有区块号查询都失败，用来触发断路器
type failingBackend struct {
	Backend
	calls int
}

func (f *failingBackend) BlockNumber(ctx context.Context) (uint64, error) {
	f.calls++
	return 0, errors.New("connection refused")
}

func TestCircuitBreakerOpensOnRPCFailures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// 查不到交易不算节点故障，不应触发熔断
	for i := 0; i < 10; i++ {
		if _, err := e.client.Receipt(ctx, common.HexToHash("0x01")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if _, err := e.client.LatestBlock(ctx); err != nil {
		t.Fatalf("breaker tripped by NotFound: %v", err)
	}

	failing := &failingBackend{Backend: e.backend.Client()}
	client := NewClient(failing, factoryAddr)
	for i := 0; i < 5; i++ {
		if _, err := client.LatestBlock(ctx); err == nil {
			t.Fatal("expected error")
		}
	}
	if _, err := client.LatestBlock(ctx); !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if failing.calls != 5 {
		t.Fatalf("node was called %d times, want 5 (熔断后不应再打到节点)", failing.calls)
	}
}

type swapRecorder struct {
	mu     sync.Mutex
	ticks  []int32
	reorgs []uint64
}

func (r *swapRecorder) OnSwap(e *indexer.SwapEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ticks = append(r.ticks, e.Tick)
}

func (r *swapRecorder) OnReorg(fork uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reorgs = append(r.reorgs, fork)
}

func (r *swapRecorder) snapshot() ([]int32, []uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int32(nil), r.ticks...), append([]uint64(nil), r.reorgs...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 端到端：索引器 + 真实区块头 + 日志解码，并在模拟链上制造一次真实的分叉
func TestIndexerFollowsChainAndHandlesReorg(t *testing.T) {
	e := newEnv(t)

	e.emitPoolCreated()
	e.backend.Commit() // 区块 2
	e.emitSwap(userA, userB, 100, -99, 10)
	forkParent := e.backend.Commit() // 区块 3
	e.emitSwap(userA, userB, 100, -99, 20)
	e.backend.Commit() // 区块 4（稍后被分叉替换）

	state := indexer.NewPoolStateManager()
	cfg := indexer.DefaultConfig()
	cfg.PollInterval = 5 * time.Millisecond
	idx := indexer.NewIndexer(cfg, state, e.client)
	rec := &swapRecorder{}
	idx.Subscribe(rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := idx.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "initial sync", func() bool {
		ticks, _ := rec.snapshot()
		return len(ticks) == 2 && idx.GetStats().LastBlockNumber == 4
	})

	// 从区块 3 分叉：新的区块 4 里多了一笔 swap，再出一个区块让新链更长。
	// 被分叉掉的那笔交易会回到交易池，可能被重新打包进新链，所以只断言最后一笔。
	if err := e.backend.Fork(forkParent); err != nil {
		t.Fatal(err)
	}
	e.emitSwap(userB, userA, -50, 51, 30)
	e.backend.Commit() // 区块 4'
	e.backend.Commit() // 区块 5'

	waitFor(t, "reorg handling", func() bool {
		ticks, reorgs := rec.snapshot()
		return len(reorgs) == 1 && ticks[len(ticks)-1] == 30 && idx.GetStats().LastBlockNumber == 5
	})

	if _, reorgs := rec.snapshot(); reorgs[0] != 4 {
		t.Fatalf("reorgs = %v, want [4]", reorgs)
	}
	pool, ok := state.GetPool(formatAddress(poolAddr))
	if !ok {
		t.Fatal("pool missing")
	}
	if snap := pool.Snapshot(); snap.Tick != 30 {
		t.Fatalf("pool tick = %d, want 30", snap.Tick)
	}
}
