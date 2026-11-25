package contractabi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 内嵌的 ABI 必须与 Hardhat 编译产物一致；本地没有编译产物时跳过
func TestEmbeddedABIMatchesArtifacts(t *testing.T) {
	artifacts := filepath.Join("..", "..", "..", "contracts", "artifacts", "contracts")
	for name, embedded := range map[string]string{
		"UniGoPool": poolJSON, "UniGoFactory": factoryJSON, "UniGoManager": managerJSON,
		"UniGoQuoter": quoterJSON, "FlashArbitrage": flashArbitrageJSON,
		"test/AggregatorV3Interface": aggregatorJSON,
	} {
		raw, err := os.ReadFile(filepath.Join(artifacts, name+".sol", filepath.Base(name)+".json"))
		if err != nil {
			t.Skipf("没有编译产物，跳过（%v）", err)
		}
		var artifact struct {
			ABI json.RawMessage `json:"abi"`
		}
		if err := json.Unmarshal(raw, &artifact); err != nil {
			t.Fatal(err)
		}
		var want, got bytes.Buffer
		json.Compact(&want, artifact.ABI)
		json.Compact(&got, []byte(embedded))
		if want.String() != got.String() {
			t.Errorf("%s 的内嵌 ABI 与编译产物不一致，请执行 go generate ./chain/...", name)
		}
	}
}

func TestEventsPresent(t *testing.T) {
	for _, name := range []string{"Initialize", "Mint", "Burn", "Swap", "Collect", "Flash"} {
		if _, ok := Pool.Events[name]; !ok {
			t.Errorf("Pool ABI 缺少事件 %s", name)
		}
	}
	if _, ok := Factory.Events["PoolCreated"]; !ok {
		t.Error("Factory ABI 缺少事件 PoolCreated")
	}
}
