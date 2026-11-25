package eip712

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
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
	Owner        string // 签名者地址
	TokenIn      string // 输入 token
	TokenOut     string // 输出 token
	AmountIn     string // 输入数量（字符串避免精度问题）
	MinAmountOut string // 最小输出（滑点保护）
	Nonce        uint64 // 防重放 nonce
	Deadline     int64  // 过期时间戳（Unix 秒）
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
	return v.nonces[strings.ToLower(address)]
}

// Verify 验证 EIP-712 签名
//
// 依次检查：
//  1. deadline 未过期
//  2. 从签名恢复出的地址就是 permit.Owner（域分隔符不同的签名恢复不出同一个地址）
//  3. nonce 等于该地址当前的 nonce
//
// 全部通过后消费 nonce，同一个签名不能再用第二次。
func (v *Verifier) Verify(permit *SwapPermit, sig *Signature) error {
	if time.Now().Unix() > permit.Deadline {
		return errors.New("签名已过期（deadline exceeded）")
	}

	hash, err := v.Hash(permit)
	if err != nil {
		return err
	}
	recovered, err := recoverAddress(hash, sig)
	if err != nil {
		return fmt.Errorf("签名验证失败：%w", err)
	}
	owner := strings.ToLower(permit.Owner)
	if recovered != owner {
		return fmt.Errorf("签名验证失败：期望签名者 %s，恢复出 %s", owner, recovered)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if expected := v.nonces[owner]; permit.Nonce != expected {
		return fmt.Errorf("nonce 不匹配：期望 %d，收到 %d（可能为重放攻击）", expected, permit.Nonce)
	}
	v.nonces[owner]++
	return nil
}

// 类型哈希：keccak256(类型的规范字符串)
var (
	domainTypeHash = crypto.Keccak256([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	permitTypeHash = crypto.Keccak256([]byte(
		"SwapPermit(address owner,address tokenIn,address tokenOut,uint256 amountIn,uint256 minAmountOut,uint256 nonce,uint256 deadline)"))
)

// Hash 计算 permit 的 EIP-712 签名摘要
//
//	digest = keccak256(0x19 0x01 ‖ domainSeparator ‖ hashStruct(permit))
//
// 其中 hashStruct(s) = keccak256(typeHash ‖ 各字段按 32 字节编码)，
// 与钱包的 eth_signTypedData_v4 算出的摘要相同。
func (v *Verifier) Hash(permit *SwapPermit) ([]byte, error) {
	contract, err := addressWord(v.domain.VerifyingContract)
	if err != nil {
		return nil, fmt.Errorf("verifyingContract: %w", err)
	}
	domainSeparator := crypto.Keccak256(
		domainTypeHash,
		crypto.Keccak256([]byte(v.domain.Name)),
		crypto.Keccak256([]byte(v.domain.Version)),
		uintWord(new(big.Int).SetUint64(v.domain.ChainID)),
		contract,
	)

	owner, err := addressWord(permit.Owner)
	if err != nil {
		return nil, fmt.Errorf("owner: %w", err)
	}
	tokenIn, err := addressWord(permit.TokenIn)
	if err != nil {
		return nil, fmt.Errorf("tokenIn: %w", err)
	}
	tokenOut, err := addressWord(permit.TokenOut)
	if err != nil {
		return nil, fmt.Errorf("tokenOut: %w", err)
	}
	amountIn, err := decimalWord(permit.AmountIn)
	if err != nil {
		return nil, fmt.Errorf("amountIn: %w", err)
	}
	minAmountOut, err := decimalWord(permit.MinAmountOut)
	if err != nil {
		return nil, fmt.Errorf("minAmountOut: %w", err)
	}
	if permit.Deadline < 0 {
		return nil, errors.New("deadline 不能为负")
	}
	words := [][]byte{
		permitTypeHash, owner, tokenIn, tokenOut, amountIn, minAmountOut,
		uintWord(new(big.Int).SetUint64(permit.Nonce)),
		uintWord(big.NewInt(permit.Deadline)),
	}
	structHash := crypto.Keccak256(words...)

	return crypto.Keccak256([]byte("\x19\x01"), domainSeparator, structHash), nil
}

// CreatePermit 创建签名许可（方便测试）
func (v *Verifier) CreatePermit(owner, tokenIn, tokenOut, amountIn, minAmountOut string) *SwapPermit {
	v.mu.RLock()
	nonce := v.nonces[strings.ToLower(owner)]
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

// uintWord 把 uint256 编码成 32 字节大端
func uintWord(n *big.Int) []byte {
	return common.LeftPadBytes(n.Bytes(), 32)
}

// decimalWord 把十进制字符串表示的 uint256 编码成 32 字节
func decimalWord(decimal string) ([]byte, error) {
	n, ok := new(big.Int).SetString(decimal, 10)
	if !ok || n.Sign() < 0 || n.BitLen() > 256 {
		return nil, fmt.Errorf("不是合法的 uint256: %q", decimal)
	}
	return uintWord(n), nil
}

// addressWord 把地址编码成 32 字节（左补零）
func addressWord(addr string) ([]byte, error) {
	if !common.IsHexAddress(addr) {
		return nil, fmt.Errorf("不是合法地址: %q", addr)
	}
	return common.LeftPadBytes(common.HexToAddress(addr).Bytes(), 32), nil
}

// ParseSignature 解析 65 字节的十六进制签名（r ‖ s ‖ v，v 为 27/28 或 0/1）
func ParseSignature(sigHex string) (*Signature, error) {
	raw, err := hexutil.Decode(sigHex)
	if err != nil || len(raw) != 65 {
		return nil, errors.New("签名必须是 65 字节的十六进制串")
	}
	sig := &Signature{V: raw[64]}
	copy(sig.R[:], raw[:32])
	copy(sig.S[:], raw[32:64])
	if sig.V < 27 {
		sig.V += 27
	}
	return sig, nil
}

// Hex 把签名编码成 r ‖ s ‖ v 的十六进制串
func (s *Signature) Hex() string {
	raw := append(append(append([]byte{}, s.R[:]...), s.S[:]...), s.V)
	return hexutil.Encode(raw)
}

// recoverAddress 从签名恢复签名者地址（小写十六进制）
//
// 拒绝 s 在曲线阶上半区的签名：同一条消息的 (r, s) 和 (r, n-s) 都能通过 ecrecover，
// 只接受低 s 才能保证一个签名只有一种写法。
func recoverAddress(hash []byte, sig *Signature) (string, error) {
	if sig == nil || (sig.V != 27 && sig.V != 28) {
		return "", errors.New("签名的 v 值非法")
	}
	r, s := new(big.Int).SetBytes(sig.R[:]), new(big.Int).SetBytes(sig.S[:])
	if !crypto.ValidateSignatureValues(sig.V-27, r, s, true) {
		return "", errors.New("签名的 r/s 值非法")
	}
	raw := append(append(append([]byte{}, sig.R[:]...), sig.S[:]...), sig.V-27)
	pub, err := crypto.SigToPub(hash, raw)
	if err != nil {
		return "", err
	}
	return strings.ToLower(crypto.PubkeyToAddress(*pub).Hex()), nil
}

// SignPermit 用私钥对摘要签名（测试和后端自有账户使用；用户侧由钱包签名）
func SignPermit(key *ecdsa.PrivateKey, hash []byte) (*Signature, error) {
	raw, err := crypto.Sign(hash, key)
	if err != nil {
		return nil, err
	}
	sig := &Signature{V: raw[64] + 27}
	copy(sig.R[:], raw[:32])
	copy(sig.S[:], raw[32:64])
	return sig, nil
}
