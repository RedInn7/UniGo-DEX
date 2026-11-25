package eip712

import (
	"bytes"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

var testDomain = DomainSeparator{
	Name:              "UniGo-DEX",
	Version:           "1",
	ChainID:           31337,
	VerifyingContract: "0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512",
}

const (
	tokenIn  = "0x00000000000000000000000000000000000000a1"
	tokenOut = "0x00000000000000000000000000000000000000b2"
)

func newPermit(v *Verifier, owner string) *SwapPermit {
	return v.CreatePermit(owner, tokenIn, tokenOut, "1000000000000000000", "990000000000000000")
}

// 摘要必须与标准实现（钱包 eth_signTypedData_v4 用的算法）一致，否则钱包签出来的签名验不过
func TestHashMatchesStandardTypedData(t *testing.T) {
	v := NewVerifier(testDomain, 5*time.Minute)
	permit := &SwapPermit{
		Owner: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8", TokenIn: tokenIn, TokenOut: tokenOut,
		AmountIn: "1000000000000000000", MinAmountOut: "990000000000000000", Nonce: 7, Deadline: 1900000000,
	}
	got, err := v.Hash(permit)
	if err != nil {
		t.Fatal(err)
	}

	typed := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"}, {Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"}, {Name: "verifyingContract", Type: "address"},
			},
			"SwapPermit": {
				{Name: "owner", Type: "address"}, {Name: "tokenIn", Type: "address"}, {Name: "tokenOut", Type: "address"},
				{Name: "amountIn", Type: "uint256"}, {Name: "minAmountOut", Type: "uint256"},
				{Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint256"},
			},
		},
		PrimaryType: "SwapPermit",
		Domain: apitypes.TypedDataDomain{
			Name: testDomain.Name, Version: testDomain.Version,
			ChainId:           math.NewHexOrDecimal256(int64(testDomain.ChainID)),
			VerifyingContract: testDomain.VerifyingContract,
		},
		Message: apitypes.TypedDataMessage{
			"owner": permit.Owner, "tokenIn": permit.TokenIn, "tokenOut": permit.TokenOut,
			"amountIn": permit.AmountIn, "minAmountOut": permit.MinAmountOut,
			"nonce": big.NewInt(int64(permit.Nonce)).String(), "deadline": big.NewInt(permit.Deadline).String(),
		},
	}
	want, _, err := apitypes.TypedDataAndHash(typed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("digest = %x\nwant     %x", got, want)
	}
}

func TestVerify(t *testing.T) {
	key, _ := crypto.GenerateKey()
	owner := crypto.PubkeyToAddress(key.PublicKey).Hex() // 带校验和大小写，验证时不区分
	otherKey, _ := crypto.GenerateKey()

	sign := func(v *Verifier, p *SwapPermit) *Signature {
		hash, err := v.Hash(p)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := SignPermit(key, hash)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}

	v := NewVerifier(testDomain, 5*time.Minute)
	permit := newPermit(v, owner)
	sig := sign(v, permit)

	// 十六进制往返
	parsed, err := ParseSignature(sig.Hex())
	if err != nil || *parsed != *sig {
		t.Fatalf("signature hex roundtrip: %v", err)
	}

	if err := v.Verify(permit, parsed); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if v.GetNonce(owner) != 1 || v.GetNonce(strings.ToLower(owner)) != 1 {
		t.Fatalf("nonce = %d, want 1", v.GetNonce(owner))
	}

	// 重放：同一个签名第二次使用
	if err := v.Verify(permit, sig); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("replay should be rejected by nonce, got %v", err)
	}

	// 篡改任何一个字段，恢复出的地址都会变
	for name, tamper := range map[string]func(p *SwapPermit){
		"amountIn":     func(p *SwapPermit) { p.AmountIn = "2000000000000000000" },
		"minAmountOut": func(p *SwapPermit) { p.MinAmountOut = "1" },
		"tokenOut":     func(p *SwapPermit) { p.TokenOut = tokenIn },
		"deadline":     func(p *SwapPermit) { p.Deadline++ },
	} {
		p := newPermit(v, owner)
		s := sign(v, p)
		tamper(p)
		if err := v.Verify(p, s); err == nil {
			t.Errorf("tampered %s accepted", name)
		}
	}
	if v.GetNonce(owner) != 1 {
		t.Fatalf("failed verifications must not consume the nonce, nonce = %d", v.GetNonce(owner))
	}

	// 别人的私钥签的
	p := newPermit(v, owner)
	hash, _ := v.Hash(p)
	forged, _ := SignPermit(otherKey, hash)
	if err := v.Verify(p, forged); err == nil {
		t.Error("signature from another key accepted")
	}

	// 过期
	p = newPermit(v, owner)
	p.Deadline = time.Now().Add(-time.Second).Unix()
	if err := v.Verify(p, sign(v, p)); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Errorf("expired permit: %v", err)
	}

	// 跨域重放：在另一条链 / 另一个合约的域下签的名，在本域验不过
	for name, domain := range map[string]DomainSeparator{
		"chainId":           {Name: testDomain.Name, Version: testDomain.Version, ChainID: 1, VerifyingContract: testDomain.VerifyingContract},
		"verifyingContract": {Name: testDomain.Name, Version: testDomain.Version, ChainID: testDomain.ChainID, VerifyingContract: tokenIn},
	} {
		foreign := NewVerifier(domain, 5*time.Minute)
		p := newPermit(v, owner)
		if err := v.Verify(p, sign(foreign, p)); err == nil {
			t.Errorf("signature under a different %s accepted", name)
		}
	}

	// 高 s 值的可塑签名：(r, n-s, 翻转 v) 对 ecrecover 同样有效，必须拒绝
	p = newPermit(v, owner)
	s := sign(v, p)
	n := crypto.S256().Params().N
	flipped := &Signature{V: 27 + 28 - s.V, R: s.R}
	new(big.Int).Sub(n, new(big.Int).SetBytes(s.S[:])).FillBytes(flipped.S[:])
	if err := v.Verify(p, flipped); err == nil {
		t.Error("malleable high-s signature accepted")
	}
	if err := v.Verify(p, s); err != nil {
		t.Errorf("original low-s signature rejected: %v", err)
	}
}

func TestHashRejectsBadFields(t *testing.T) {
	v := NewVerifier(testDomain, time.Minute)
	owner := "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	for name, mutate := range map[string]func(p *SwapPermit){
		"owner":    func(p *SwapPermit) { p.Owner = "0x1234" },
		"tokenIn":  func(p *SwapPermit) { p.TokenIn = "" },
		"amountIn": func(p *SwapPermit) { p.AmountIn = "-1" },
		"minOut":   func(p *SwapPermit) { p.MinAmountOut = "1e18" },
	} {
		p := newPermit(v, owner)
		mutate(p)
		if _, err := v.Hash(p); err == nil {
			t.Errorf("bad %s accepted", name)
		}
	}
	if _, err := ParseSignature("0x1234"); err == nil {
		t.Error("short signature accepted")
	}
}
