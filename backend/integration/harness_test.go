//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// harness_test.go — 集成测试的脚手架：拉起 Hardhat 节点、部署合约、发交易
//
// 节点进程的回收有三层保险：
//   1. 正常结束：TestMain 里关掉节点并等它退出
//   2. 节点跑在独立进程组里，退出时整组 SIGKILL，不留孙进程
//   3. 测试进程被强杀（超时、Ctrl+C、kill -9）：包装脚本一直在读一根管道，
//      管道的写端只有测试进程持有，测试进程一死读端就 EOF，脚本随即杀掉节点

const contractsDir = "../../contracts"

// Hardhat 节点内置的测试账户私钥（公开的固定值，只在本地链上有余额）
var hardhatKeys = []string{
	"ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
	"59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d",
	"5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a",
	"7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6",
}

// account 一个测试账户
type account struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

func loadAccounts() []account {
	accounts := make([]account, len(hardhatKeys))
	for i, hex := range hardhatKeys {
		key, err := crypto.HexToECDSA(hex)
		if err != nil {
			panic(err)
		}
		accounts[i] = account{key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}
	}
	return accounts
}

// node 一个正在运行的 Hardhat 节点
type node struct {
	url   string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	eth   *ethclient.Client
	rpc   *rpc.Client
}

// startNode 在空闲端口上拉起 Hardhat 节点并等它就绪
func startNode() (*node, error) {
	if _, err := os.Stat(filepath.Join(contractsDir, "artifacts", "contracts", "UniGoPool.sol", "UniGoPool.json")); err != nil {
		compile := exec.Command("node_modules/.bin/hardhat", "compile")
		compile.Dir = contractsDir
		if out, err := compile.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("hardhat compile 失败: %v\n%s", err, out)
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	// 管道读到 EOF（测试进程退出）或被显式关闭时杀掉节点
	script := fmt.Sprintf(`node_modules/.bin/hardhat node --hostname 127.0.0.1 --port %d >/dev/null 2>&1 &
pid=$!
cat >/dev/null
kill -9 $pid 2>/dev/null
wait $pid 2>/dev/null`, port)
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = contractsDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	n := &node{url: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd, stdin: stdin}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if client, err := rpc.Dial(n.url); err == nil {
			var chainID hexutil.Big
			if err := client.Call(&chainID, "eth_chainId"); err == nil {
				n.rpc = client
				n.eth = ethclient.NewClient(client)
				return n, nil
			}
			client.Close()
		}
		if time.Now().After(deadline) {
			n.stop()
			return nil, fmt.Errorf("Hardhat 节点 90 秒内没有就绪")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// stop 关闭节点并等待进程退出
func (n *node) stop() {
	if n.rpc != nil {
		n.rpc.Close()
	}
	n.stdin.Close() // 包装脚本读到 EOF 后杀掉节点
	done := make(chan struct{})
	go func() { n.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	// 兜底：整个进程组一起杀
	syscall.Kill(-n.cmd.Process.Pid, syscall.SIGKILL)
	<-done
}

// ===== EVM 控制 =====

func (n *node) evm(t testing.TB, method string, args ...interface{}) {
	t.Helper()
	if err := n.rpc.Call(nil, method, args...); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// mine 出一个空区块
func (n *node) mine(t testing.TB) { n.evm(t, "evm_mine") }

// setNextTimestamp 指定下一个区块的时间戳
func (n *node) setNextTimestamp(t testing.TB, ts uint64) {
	n.evm(t, "evm_setNextBlockTimestamp", hexutil.Uint64(ts))
}

func (n *node) latestTimestamp(t testing.TB) uint64 {
	t.Helper()
	header, err := n.eth.HeaderByNumber(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return header.Time
}

// ===== 合约产物、部署与调用 =====

type artifact struct {
	ABI      abi.ABI
	Bytecode []byte
}

func loadArtifact(t testing.TB, path, name string) artifact {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractsDir, "artifacts", "contracts", path, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		ABI      json.RawMessage `json:"abi"`
		Bytecode string          `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	contractABI, err := abi.JSON(strings.NewReader(string(parsed.ABI)))
	if err != nil {
		t.Fatal(err)
	}
	return artifact{ABI: contractABI, Bytecode: common.FromHex(parsed.Bytecode)}
}

var chainID = big.NewInt(31337)

// signTx 构造并签名一笔交易；tipGwei 用来控制同一区块内的排序（小费高的在前）
func (n *node) signTx(t testing.TB, from account, to *common.Address, data []byte, tipGwei int64) *types.Transaction {
	t.Helper()
	nonce, err := n.eth.PendingNonceAt(context.Background(), from.addr)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := types.SignNewTx(from.key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(tipGwei * 1e9),
		GasFeeCap: big.NewInt(100e9),
		Gas:       9_000_000,
		To:        to,
		Data:      data,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// waitReceipt 等交易上链
func (n *node) waitReceipt(t testing.TB, hash common.Hash) *types.Receipt {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		receipt, err := n.eth.TransactionReceipt(context.Background(), hash)
		if err == nil {
			return receipt
		}
		if time.Now().After(deadline) {
			t.Fatalf("交易 %s 没有上链: %v", hash.Hex(), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// send 发送交易并等它成功上链（节点默认自动出块）
func (n *node) send(t testing.TB, from account, to common.Address, data []byte) *types.Receipt {
	t.Helper()
	tx := n.signTx(t, from, &to, data, 1)
	if err := n.eth.SendTransaction(context.Background(), tx); err != nil {
		t.Fatalf("发送交易失败: %v", err)
	}
	receipt := n.waitReceipt(t, tx.Hash())
	if receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("交易 %s 执行失败（revert）", tx.Hash().Hex())
	}
	return receipt
}

// deploy 部署合约，返回合约地址
func (n *node) deploy(t testing.TB, from account, a artifact, args ...interface{}) common.Address {
	t.Helper()
	ctor, err := a.ABI.Pack("", args...)
	if err != nil {
		t.Fatal(err)
	}
	tx := n.signTx(t, from, nil, append(append([]byte{}, a.Bytecode...), ctor...), 1)
	if err := n.eth.SendTransaction(context.Background(), tx); err != nil {
		t.Fatalf("部署失败: %v", err)
	}
	receipt := n.waitReceipt(t, tx.Hash())
	if receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatal("部署交易执行失败")
	}
	return receipt.ContractAddress
}

// invoke 调用合约方法（发交易）
func (n *node) invoke(t testing.TB, from account, to common.Address, contract abi.ABI, method string, args ...interface{}) *types.Receipt {
	t.Helper()
	data, err := contract.Pack(method, args...)
	if err != nil {
		t.Fatalf("编码 %s 失败: %v", method, err)
	}
	return n.send(t, from, to, data)
}

// read 只读调用合约方法
func (n *node) read(t testing.TB, to common.Address, contract abi.ABI, method string, args ...interface{}) []interface{} {
	t.Helper()
	data, err := contract.Pack(method, args...)
	if err != nil {
		t.Fatalf("编码 %s 失败: %v", method, err)
	}
	out, err := n.eth.CallContract(context.Background(), ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		t.Fatalf("调用 %s 失败: %v", method, err)
	}
	values, err := contract.Unpack(method, out)
	if err != nil {
		t.Fatalf("解码 %s 返回值失败: %v", method, err)
	}
	return values
}

// waitFor 轮询直到条件成立
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func lower(a common.Address) string { return strings.ToLower(a.Hex()) }

func e18(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
