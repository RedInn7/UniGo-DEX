//go:build ignore

// gen.go — 从 Hardhat 编译产物提取 ABI
//
// 用法（先在 contracts/ 下执行 npx hardhat compile）：
//
//	cd backend && go generate ./chain/...
//
// 只保留产物里的 abi 字段，写到本目录的 <合约名>.json，由 contractabi.go 内嵌进二进制。
// 合约接口变了重新生成即可，不需要手工维护 ABI。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// 合约名 -> 编译产物相对 artifacts/contracts 的目录
var contracts = map[string]string{
	"UniGoPool":             "UniGoPool.sol",
	"UniGoFactory":          "UniGoFactory.sol",
	"UniGoManager":          "UniGoManager.sol",
	"UniGoQuoter":           "UniGoQuoter.sol",
	"FlashArbitrage":        "FlashArbitrage.sol",
	"AggregatorV3Interface": filepath.Join("test", "AggregatorV3Interface.sol"),
}

func main() {
	artifacts := filepath.Join("..", "..", "..", "contracts", "artifacts", "contracts")
	for name, dir := range contracts {
		raw, err := os.ReadFile(filepath.Join(artifacts, dir, name+".json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取编译产物失败（先执行 npx hardhat compile）:", err)
			os.Exit(1)
		}
		var artifact struct {
			ABI json.RawMessage `json:"abi"`
		}
		if err := json.Unmarshal(raw, &artifact); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var out bytes.Buffer
		if err := json.Indent(&out, artifact.ABI, "", "  "); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		out.WriteByte('\n')
		if err := os.WriteFile(name+".json", out.Bytes(), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("已生成", name+".json")
	}
}
