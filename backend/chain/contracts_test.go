package chain

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"unigo-dex/chain/contractabi"
)

func TestEncodePath(t *testing.T) {
	a := "0x00000000000000000000000000000000000000aa"
	b := "0x00000000000000000000000000000000000000bb"
	c := "0x00000000000000000000000000000000000000cc"

	path, err := EncodePath([]string{a, b, c}, []uint32{3000, 500})
	if err != nil {
		t.Fatal(err)
	}
	want := common.FromHex("0x" +
		"00000000000000000000000000000000000000aa" + "000bb8" +
		"00000000000000000000000000000000000000bb" + "0001f4" +
		"00000000000000000000000000000000000000cc")
	if !bytes.Equal(path, want) {
		t.Fatalf("path = %x\nwant   %x", path, want)
	}

	for _, bad := range []struct {
		tokens []string
		fees   []uint32
	}{
		{[]string{a}, nil},
		{[]string{a, b}, []uint32{3000, 500}},
		{[]string{a, "not-an-address"}, []uint32{3000}},
		{[]string{a, b}, []uint32{1 << 24}},
	} {
		if _, err := EncodePath(bad.tokens, bad.fees); err == nil {
			t.Errorf("EncodePath(%v, %v) should fail", bad.tokens, bad.fees)
		}
	}
}

// 编码结果用同一份 ABI 解回来，字段要原样对上
func TestEncodeManagerCalls(t *testing.T) {
	pool := "0x0000000000000000000000000000000000001001"
	recipient := "0x0000000000000000000000000000000000002002"

	data, err := EncodeSwap(pool, true, big.NewInt(1000), recipient, big.NewInt(990), 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	method, err := contractabi.Manager.MethodById(data[:4])
	if err != nil || method.Name != "swap" {
		t.Fatalf("method = %v, err = %v", method, err)
	}
	args, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Params swapParams }
	if err := method.Inputs.Copy(&got, args); err != nil {
		t.Fatal(err)
	}
	p := got.Params
	if p.Pool != common.HexToAddress(pool) || !p.ZeroForOne || p.AmountSpecified.Int64() != 1000 ||
		p.SqrtPriceLimitX96.String() != "4295128740" || p.Recipient != common.HexToAddress(recipient) ||
		p.AmountOutMinimum.Int64() != 990 || p.Deadline.Int64() != 1700000000 {
		t.Fatalf("decoded swap params: %+v", p)
	}

	// 反方向用价格上限
	data, _ = EncodeSwap(pool, false, big.NewInt(1), recipient, big.NewInt(0), 1)
	args, _ = method.Inputs.Unpack(data[4:])
	method.Inputs.Copy(&got, args)
	if got.Params.SqrtPriceLimitX96.String() != "1461446703485210103287273052203988822378723970341" {
		t.Fatalf("limit = %s", got.Params.SqrtPriceLimitX96)
	}

	path, _ := EncodePath([]string{pool, recipient}, []uint32{3000})
	data, err = EncodeExactInput(path, recipient, big.NewInt(5), big.NewInt(4), 99)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := contractabi.Manager.MethodById(data[:4]); m == nil || m.Name != "exactInput" {
		t.Fatalf("exactInput selector mismatch")
	}

	data, err = EncodeExactOutputSingle(pool, recipient, 3000, recipient, big.NewInt(5), big.NewInt(6), 99)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := contractabi.Manager.MethodById(data[:4]); m == nil || m.Name != "exactOutputSingle" {
		t.Fatalf("exactOutputSingle selector mismatch")
	}

	data, err = EncodeArbitrage(pool, pool, pool, recipient, big.NewInt(7), big.NewInt(1))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := contractabi.FlashArbitrage.MethodById(data[:4]); m == nil || m.Name != "arbitrage" {
		t.Fatalf("arbitrage selector mismatch")
	}
}

func TestAverageTick(t *testing.T) {
	for _, tc := range []struct {
		start, end int64
		window     uint32
		want       int32
	}{
		{0, 6000, 60, 100},
		{1000, 1059, 60, 0}, // 59/60 向下取整
		{0, -6000, 60, -100},
		{0, -1, 60, -1}, // 负数向负无穷取整，与链上参考实现一致
		{-500, -441, 60, 0},
		{-500, -561, 60, -2},
	} {
		if got := AverageTick(big.NewInt(tc.start), big.NewInt(tc.end), tc.window); got != tc.want {
			t.Errorf("AverageTick(%d, %d, %d) = %d, want %d", tc.start, tc.end, tc.window, got, tc.want)
		}
	}
}
