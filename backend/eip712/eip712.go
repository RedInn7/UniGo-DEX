package eip712

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/sha3"
)

// eip712.go — EIP-712 类型化数据签名（含重放攻击防护）
//
// === 什么是 EIP-712？ ===
//
// EIP-712 定义了结构化数据的签名标准，让用户在钱包中看到可读的签名内容
// （而非一串不可读的 hex），提高安全性。
//
// === 三层重放攻击防护 ===
//
//   1. Domain Separator（域分隔符）
//      - 包含 ChainID 和 VerifyingContract 地址
//      - 防止签名被跨链重放（如 ETH mainnet 签名不能在 Polygon 上使用）
//      - 防止签名被跨合约重放
//
//   2. Nonce 机制
//      - 每个地址维护单调递增的 nonce
//      - 签名一旦使用即失效（nonce 已消费）
//      - 防止同一签名被多次使用
//
//   3. Deadline（过期时间）
//      - 签名包含过期时间戳
//      - 超时自动失效
//      - 默认 5 分钟有效期
//      - 防止离线签名被长期保存后恶意使用

// TypedData EIP-712 类型化数据结构
type TypedData struct {
	Domain  DomainSeparator
	Message SwapPermit
}

// DomainSeparator EIP-712 域分隔符
//
// 每个域由以下字段唯一标识：
//   - Name: 应用名称
//   - Version: 应用版本
//   - ChainID: 链 ID（防跨链重放）
//   - VerifyingContract: 验证合约地址（防跨合约重放）
type DomainSeparator struct {
	Name              string // "UniGo-DEX"
	Version           string // "1"
	ChainID           uint64 // 1 = ETH mainnet, 31337 = Hardhat
	VerifyingContract string // Manager 合约地址
}

// SwapPermit Swap 许可消息（用户签名的内容）
type SwapPermit struct {
	Owner           string // 签名者地址
	TokenIn         string // 输入 token
	TokenOut        string // 输出 token
	AmountIn        string // 输入数量（字符串避免精度问题）
	MinAmountOut    string // 最小输出（滑点保护）
	Nonce           uint64 // 防重放 nonce
	Deadline        int64  // 过期时间戳（Unix 秒）
}

// Signature 签名数据
type Signature struct {
	V uint8
	R [32]byte
	S [32]byte
}

// Verifier EIP-712 签名验证器（含 Nonce 管理）
type Verifier struct {
	domain DomainSeparator

	// Nonce 管理：address -> next expected nonce
	nonces map[string]uint64

	// 默认签名有效期
	defaultDeadline time.Duration

	mu sync.RWMutex
}

// NewVerifier 创建签名验证器
func NewVerifier(domain DomainSeparator, defaultDeadline time.Duration) *Verifier {
	return &Verifier{
		domain:          domain,
		nonces:          make(map[string]uint64),
		defaultDeadline: defaultDeadline,
	}
}

// GetNonce 获取地址的下一个 nonce
func (v *Verifier) GetNonce(address string) uint64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.nonces[address]
}

// Verify 验证 EIP-712 签名
//
// 验证流程（三层防护）：
//
//   Layer 1: Deadline 检查
//     if time.Now() > permit.Deadline → 签名已过期，拒绝
//
//   Layer 2: Nonce 检查
//     if permit.Nonce != expectedNonce[owner] → 重放攻击或序号错误，拒绝
//
//   Layer 3: 签名验证
//     1. 根据 domain + message 计算 EIP-712 哈希
//     2. 从签名中恢复签名者地址
//     3. 验证恢复的地址 == permit.Owner
//
//   全部通过后，消费 nonce（递增），防止重放
func (v *Verifier) Verify(permit *SwapPermit, sig *Signature) error {
	// === Layer 1: Deadline 检查 ===
	if time.Now().Unix() > permit.Deadline {
		return errors.New("签名已过期（deadline exceeded）")
	}

	// === Layer 2: Nonce 检查 ===
	v.mu.Lock()
	expectedNonce := v.nonces[permit.Owner]
	if permit.Nonce != expectedNonce {
		v.mu.Unlock()
		return fmt.Errorf("nonce 不匹配：期望 %d，收到 %d（可能为重放攻击）",
			expectedNonce, permit.Nonce)
	}

	// === Layer 3: 签名验证 ===
	hash := v.hashTypedData(permit)
	recoveredAddr := recoverAddress(hash, sig)
	if recoveredAddr != permit.Owner {
		v.mu.Unlock()
		return fmt.Errorf("签名验证失败：期望签名者 %s，恢复出 %s",
			permit.Owner, recoveredAddr)
	}

	// 全部通过，消费 nonce
	v.nonces[permit.Owner] = expectedNonce + 1
	v.mu.Unlock()

	return nil
}

// hashTypedData 计算 EIP-712 哈希
//
// 结构：keccak256("\x19\x01" || domainSeparatorHash || messageHash)
//
// 前缀 "\x19\x01" 是 EIP-712 规范要求的，用于区分不同类型的以太坊签名
func (v *Verifier) hashTypedData(permit *SwapPermit) []byte {
	// 计算 Domain Separator Hash
	domainHash := keccak256(
		[]byte(v.domain.Name),
		[]byte(v.domain.Version),
		uint64ToBytes(v.domain.ChainID),
		[]byte(v.domain.VerifyingContract),
	)

	// 计算 Message Hash
	messageHash := keccak256(
		[]byte(permit.Owner),
		[]byte(permit.TokenIn),
		[]byte(permit.TokenOut),
		[]byte(permit.AmountIn),
		[]byte(permit.MinAmountOut),
		uint64ToBytes(permit.Nonce),
		int64ToBytes(permit.Deadline),
	)

	// EIP-712 最终哈希
	return keccak256(
		[]byte("\x19\x01"),
		domainHash,
		messageHash,
	)
}

// CreatePermit 创建签名许可（方便测试）
func (v *Verifier) CreatePermit(owner, tokenIn, tokenOut, amountIn, minAmountOut string) *SwapPermit {
	v.mu.RLock()
	nonce := v.nonces[owner]
	v.mu.RUnlock()

	return &SwapPermit{
		Owner:        owner,
		TokenIn:      tokenIn,
		TokenOut:     tokenOut,
		AmountIn:     amountIn,
		MinAmountOut: minAmountOut,
		Nonce:        nonce,
		Deadline:     time.Now().Add(v.defaultDeadline).Unix(),
	}
}

// ===== 辅助函数 =====

// keccak256 计算 Keccak-256 哈希
func keccak256(data ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, d := range data {
		h.Write(d)
	}
	return h.Sum(nil)
}

// recoverAddress 从签名中恢复地址（简化实现）
// 生产环境应使用 go-ethereum 的 crypto.Ecrecover
func recoverAddress(hash []byte, sig *Signature) string {
	// 简化实现：实际项目中使用 crypto.SigToPub + crypto.PubkeyToAddress
	_ = hash
	_ = sig
	return "" // 需要 go-ethereum 库才能实际实现
}

// SignPermit 使用私钥签名许可（简化实现）
func SignPermit(key *ecdsa.PrivateKey, hash []byte) *Signature {
	// 简化实现：实际项目中使用 crypto.Sign
	_ = key
	_ = hash
	return &Signature{}
}

func uint64ToBytes(v uint64) []byte {
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	return b
}

func int64ToBytes(v int64) []byte {
	return uint64ToBytes(uint64(v))
}
