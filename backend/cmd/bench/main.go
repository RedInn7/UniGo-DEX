// bench — 压测辅助工具：索引追块 / Reorg 重建耗时、SSE 推送延迟与丢弃
//
// 需要先用 bench/up.sh 拉起压测环境（Hardhat 节点 + 合约 + 历史成交 + 后端）。
//
//	go run ./cmd/bench chase -deploy ../bench/.run/deploy.json -runs 5 -reorg-depth 10 -reorg-runs 5
//	go run ./cmd/bench sse   -deploy ../bench/.run/deploy.json -api http://127.0.0.1:18080 -clients 200 -events 40
//
// 结果以 JSON 打印到标准输出，每行一条。
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"unigo-dex/arb"
	"unigo-dex/chain"
	"unigo-dex/gas"
	"unigo-dex/indexer"
	"unigo-dex/kline"
	"unigo-dex/mev"
	"unigo-dex/oracle"
)

// Hardhat 节点内置的 1 号测试账户（公开的固定私钥，只在本地链上有余额）；deploy.js 已给它铸币并授权 Manager
const traderKey = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"

// deployment scripts/deploy.js 写出的部署结果
type deployment struct {
	RPCURL  string `json:"rpcUrl"`
	ChainID int64  `json:"chainId"`
	Factory string `json:"factory"`
	Manager string `json:"manager"`
	Pools   []struct {
		Pair    string `json:"pair"`
		Fee     uint32 `json:"fee"`
		Address string `json:"address"`
	} `json:"pools"`
}

func loadDeployment(path string) deployment {
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal("读取部署结果失败（先执行 bench/up.sh）: %v", err)
	}
	var d deployment
	if err := json.Unmarshal(raw, &d); err != nil {
		fatal("解析部署结果失败: %v", err)
	}
	return d
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func emit(v interface{}) {
	raw, _ := json.Marshal(v)
	fmt.Println(string(raw))
}

func main() {
	if len(os.Args) < 2 {
		fatal("用法: bench chase|sse [参数]")
	}
	// 压测只量处理本身，日志丢弃
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	switch os.Args[1] {
	case "chase":
		chase(os.Args[2:])
	case "sse":
		sse(os.Args[2:])
	default:
		fatal("未知子命令 %q", os.Args[1])
	}
}

// ===== 发交易 =====

// sender 用测试账户在某个池子上发 swap
type sender struct {
	eth     *ethclient.Client
	rpc     *rpc.Client
	key     *ecdsa.PrivateKey
	from    common.Address
	chainID *big.Int
	manager common.Address
	nonce   uint64
}

func newSender(d deployment) *sender {
	client, err := rpc.Dial(d.RPCURL)
	if err != nil {
		fatal("连接节点失败: %v", err)
	}
	key, _ := crypto.HexToECDSA(traderKey)
	s := &sender{
		eth: ethclient.NewClient(client), rpc: client, key: key,
		from: crypto.PubkeyToAddress(key.PublicKey), chainID: big.NewInt(d.ChainID), manager: common.HexToAddress(d.Manager),
	}
	if s.nonce, err = s.eth.PendingNonceAt(context.Background(), s.from); err != nil {
		fatal("查询 nonce 失败: %v", err)
	}
	return s
}

// swap 发一笔单池 swap（1e15 最小单位，方向交替），返回交易哈希
func (s *sender) swap(pool string, zeroForOne bool) common.Hash {
	data, err := chain.EncodeSwap(pool, zeroForOne, big.NewInt(1e15), s.from.Hex(), big.NewInt(0), 4102444800)
	if err != nil {
		fatal("编码 swap 失败: %v", err)
	}
	tx, err := types.SignNewTx(s.key, types.LatestSignerForChainID(s.chainID), &types.DynamicFeeTx{
		ChainID: s.chainID, Nonce: s.nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(100e9),
		Gas: 1_000_000, To: &s.manager, Data: data,
	})
	if err != nil {
		fatal("签名失败: %v", err)
	}
	if err := s.eth.SendTransaction(context.Background(), tx); err != nil {
		fatal("发送交易失败: %v", err)
	}
	s.nonce++
	return tx.Hash()
}

func (s *sender) evm(method string, args ...interface{}) {
	if err := s.rpc.Call(nil, method, args...); err != nil {
		fatal("%s: %v", method, err)
	}
}

// mineBlocks 出 blocks 个区块，每个区块 perBlock 笔 swap
func (s *sender) mineBlocks(pool string, blocks, perBlock int, zeroForOne bool) {
	s.evm("evm_setAutomine", false)
	for b := 0; b < blocks; b++ {
		for i := 0; i < perBlock; i++ {
			s.swap(pool, zeroForOne)
		}
		s.evm("evm_mine")
	}
	s.evm("evm_setAutomine", true)
}

func (s *sender) head() uint64 {
	n, err := s.eth.BlockNumber(context.Background())
	if err != nil {
		fatal("查询区块号失败: %v", err)
	}
	return n
}

// ===== chase：索引追块与 Reorg 重建 =====

// newIndexer 按 main.go 的方式装配索引器和全部订阅者（预言机 / K 线 / 三明治检测 / 套利扫描）
func newIndexer(client *chain.Client) *indexer.Indexer {
	state := indexer.NewPoolStateManager()
	idx := indexer.NewIndexer(indexer.DefaultConfig(), state, client)
	idx.Subscribe(oracle.NewOracle(oracle.DefaultOracleConfig()).PoolPriceFeed(state))
	idx.Subscribe(kline.NewAggregator(kline.DefaultConfig()))
	idx.Subscribe(mev.NewGuard(mev.DefaultConfig()))
	scanner := arb.NewScanner(arb.DefaultConfig(), state, gas.NewEstimator(gas.DefaultConfig(), nil))
	scanner.Balances = client
	idx.Subscribe(scanner)
	return idx
}

// syncToHead 追到链头，返回下一个待处理的区块号
func syncToHead(idx *indexer.Indexer, next, head uint64) uint64 {
	for next <= head {
		before := next
		next = idx.SyncOnce(context.Background(), next)
		if next == before && idx.GetStats().LastBlockNumber < head {
			// 没有前进：可能刚处理完一次回滚，再来一轮
			time.Sleep(time.Millisecond)
		}
	}
	return next
}

func chase(args []string) {
	fs := flag.NewFlagSet("chase", flag.ExitOnError)
	deployFile := fs.String("deploy", "../bench/.run/deploy.json", "部署结果文件")
	runs := fs.Int("runs", 5, "从创世块追到链头的次数")
	reorgDepth := fs.Int("reorg-depth", 10, "Reorg 深度（区块数），0 = 不测")
	reorgRuns := fs.Int("reorg-runs", 5, "Reorg 测几次")
	fs.Parse(args)

	d := loadDeployment(*deployFile)
	ctx := context.Background()
	client, err := chain.Dial(ctx, d.RPCURL, d.Factory)
	if err != nil {
		fatal("连接节点失败: %v", err)
	}
	s := newSender(d)
	head := s.head()

	var idx *indexer.Indexer
	var next uint64
	for run := 1; run <= *runs; run++ {
		idx = newIndexer(client)
		start := time.Now()
		next = syncToHead(idx, 0, head)
		elapsed := time.Since(start).Seconds()
		stats := idx.GetStats()
		emit(map[string]interface{}{
			"bench": "chase", "run": run, "blocks": stats.BlocksProcessed, "events": stats.EventsProcessed,
			"seconds": elapsed, "blocks_per_s": float64(stats.BlocksProcessed) / elapsed, "events_per_s": float64(stats.EventsProcessed) / elapsed,
		})
	}

	// Reorg：快照 → 出 depth 个带 swap 的区块并索引 → 回滚快照 → 出 depth+1 个不同的区块
	// 计时范围：发现父哈希不匹配、找分叉点、撤销状态、通知订阅者丢弃数据、重新索引到新链头
	pool := d.Pools[0].Address
	for run := 1; run <= *reorgRuns && *reorgDepth > 0; run++ {
		var snapshot string
		if err := s.rpc.Call(&snapshot, "evm_snapshot"); err != nil {
			fatal("evm_snapshot: %v", err)
		}
		nonce := s.nonce
		s.mineBlocks(pool, *reorgDepth, 2, true)
		next = syncToHead(idx, next, s.head())

		var reverted bool
		if err := s.rpc.Call(&reverted, "evm_revert", snapshot); err != nil || !reverted {
			fatal("evm_revert: %v", err)
		}
		s.nonce = nonce
		s.mineBlocks(pool, *reorgDepth+1, 2, false)
		head = s.head()

		reorgsBefore, eventsBefore := idx.GetStats().ReorgsDetected, idx.GetStats().EventsProcessed
		start := time.Now()
		next = syncToHead(idx, next, head)
		elapsed := time.Since(start)
		stats := idx.GetStats()
		if stats.ReorgsDetected != reorgsBefore+1 || stats.LastBlockNumber != head {
			fatal("Reorg 没有按预期处理: %+v", stats)
		}
		emit(map[string]interface{}{
			"bench": "reorg", "run": run, "depth": *reorgDepth, "blocks_reindexed": *reorgDepth + 1,
			"events_reindexed": stats.EventsProcessed - eventsBefore, "ms": float64(elapsed.Microseconds()) / 1000,
		})
	}
}

// ===== sse：推送延迟与丢弃 =====

// subscriber 一个 SSE 订阅端
type subscriber struct {
	received atomic.Int64
	closed   atomic.Bool
}

// subscribe 订阅 trade 事件；每收到一笔成交就把 (交易哈希, 收到时间) 交给 onTrade。
// stalled 为 true 时只建立连接、不读取数据，模拟卡死的慢客户端。
func subscribe(api string, stalled bool, onTrade func(txHash string, at time.Time)) *subscriber {
	sub := &subscriber{}
	resp, err := http.Get(api + "/api/v1/stream?types=trade")
	if err != nil || resp.StatusCode != http.StatusOK {
		fatal("订阅 SSE 失败: %v", err)
	}
	go func() {
		defer resp.Body.Close()
		defer sub.closed.Store(true)
		if stalled {
			select {} // 永远不读
		}
		reader := bufio.NewReaderSize(resp.Body, 64*1024)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			at := time.Now()
			if i := strings.Index(line, `"tx_hash":"`); i >= 0 && strings.HasPrefix(line, "data: ") {
				sub.received.Add(1)
				onTrade(line[i+11:i+77], at)
			}
		}
	}()
	return sub
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted))*p+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func metric(api, name string) int64 {
	resp, err := http.Get(api + "/metrics")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var value int64
		if _, err := fmt.Sscanf(scanner.Text(), name+" %d", &value); err == nil {
			return value
		}
	}
	return -1
}

func sse(args []string) {
	fs := flag.NewFlagSet("sse", flag.ExitOnError)
	deployFile := fs.String("deploy", "../bench/.run/deploy.json", "部署结果文件")
	api := fs.String("api", "http://127.0.0.1:18080", "后端地址")
	clients := fs.Int("clients", 200, "正常订阅端数量")
	stalled := fs.Int("stalled", 0, "只连不读的慢客户端数量")
	events := fs.Int("events", 40, "延迟测试：逐笔发多少笔成交")
	burst := fs.Int("burst", 0, "突发测试：一次性灌多少笔成交（每个区块 100 笔），0 = 不测")
	fs.Parse(args)

	d := loadDeployment(*deployFile)
	s := newSender(d)
	pool := d.Pools[0].Address
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = *clients + *stalled + 8

	// 每笔成交：各订阅端收到的时间
	var mu sync.Mutex
	arrivals := map[string][]time.Time{}
	onTrade := func(txHash string, at time.Time) {
		mu.Lock()
		arrivals[txHash] = append(arrivals[txHash], at)
		mu.Unlock()
	}
	droppedBefore := metric(*api, "stream_dropped_clients_total")

	subs := make([]*subscriber, *clients)
	for i := range subs {
		subs[i] = subscribe(*api, false, onTrade)
	}
	slow := make([]*subscriber, *stalled)
	for i := range slow {
		slow[i] = subscribe(*api, true, nil)
	}
	for metric(*api, "stream_clients") < int64(*clients+*stalled) {
		time.Sleep(20 * time.Millisecond)
	}

	// 延迟测试：逐笔发交易（节点自动出块，发送返回时交易已上链），量到每个订阅端收到推送的时间
	var latencies, spreads []float64
	missing := 0
	for i := 0; i < *events; i++ {
		hash := s.swap(pool, i%2 == 0).Hex()
		mined := time.Now()
		deadline := mined.Add(10 * time.Second)
		for {
			mu.Lock()
			got := len(arrivals[hash])
			mu.Unlock()
			if got >= *clients || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		mu.Lock()
		times := append([]time.Time(nil), arrivals[hash]...)
		mu.Unlock()
		missing += *clients - len(times)
		if len(times) == 0 {
			continue
		}
		first, last := times[0], times[0]
		for _, at := range times {
			latencies = append(latencies, float64(at.Sub(mined).Microseconds())/1000)
			if at.Before(first) {
				first = at
			}
			if at.After(last) {
				last = at
			}
		}
		spreads = append(spreads, float64(last.Sub(first).Microseconds())/1000)
		time.Sleep(150 * time.Millisecond)
	}
	sort.Float64s(latencies)
	sort.Float64s(spreads)
	if *events > 0 {
		emit(map[string]interface{}{
			"bench": "sse_latency", "clients": *clients, "events": *events,
			"deliveries": len(latencies), "missing": missing,
			// 交易上链到订阅端收到推送：包含索引器的轮询间隔
			"e2e_ms_p50": percentile(latencies, 0.50), "e2e_ms_p95": percentile(latencies, 0.95), "e2e_ms_p99": percentile(latencies, 0.99),
			"e2e_ms_max": percentile(latencies, 1),
			// 同一笔成交，最先收到和最后收到的订阅端之间差多久：纯扇出耗时
			"fanout_ms_p50": percentile(spreads, 0.50), "fanout_ms_p95": percentile(spreads, 0.95), "fanout_ms_max": percentile(spreads, 1),
		})
	}

	// 突发测试：短时间灌入大量成交，正常订阅端应全部收到，卡死的慢客户端应被断开
	if *burst > 0 {
		before := make([]int64, len(subs))
		for i, sub := range subs {
			before[i] = sub.received.Load()
		}
		start := time.Now()
		s.mineBlocks(pool, *burst/100, 100, true)
		sent := (*burst / 100) * 100
		sendSeconds := time.Since(start).Seconds()

		// 等推送稳定：连续 1 秒没有新消息
		var total, lastTotal int64 = 0, -1
		for stable := 0; stable < 10; {
			time.Sleep(100 * time.Millisecond)
			total = 0
			for _, sub := range subs {
				total += sub.received.Load()
			}
			if total == lastTotal {
				stable++
			} else {
				stable, lastTotal = 0, total
			}
		}
		drainSeconds := time.Since(start).Seconds()

		complete, delivered, disconnected := 0, int64(0), 0
		for i, sub := range subs {
			got := sub.received.Load() - before[i]
			delivered += got
			if got == int64(sent) {
				complete++
			}
			if sub.closed.Load() {
				disconnected++
			}
		}
		emit(map[string]interface{}{
			"bench": "sse_burst", "clients": *clients, "stalled_clients": *stalled, "trades_sent": sent,
			"send_seconds": sendSeconds, "drain_seconds": drainSeconds,
			"clients_received_all": complete, "clients_disconnected": disconnected,
			"deliveries": delivered, "expected_deliveries": int64(sent) * int64(*clients),
			"server_dropped_clients": metric(*api, "stream_dropped_clients_total") - droppedBefore,
			"server_clients_after":   metric(*api, "stream_clients"),
		})
	}
}
