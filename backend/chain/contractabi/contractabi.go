// Package contractabi 内嵌合约 ABI
//
// 各 JSON 文件由 gen.go 从 Hardhat 编译产物（contracts/artifacts）提取，不要手工修改。
package contractabi

//go:generate go run gen.go

import (
	_ "embed"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

var (
	//go:embed UniGoPool.json
	poolJSON string
	//go:embed UniGoFactory.json
	factoryJSON string
	//go:embed UniGoManager.json
	managerJSON string
	//go:embed UniGoQuoter.json
	quoterJSON string
	//go:embed FlashArbitrage.json
	flashArbitrageJSON string
	//go:embed AggregatorV3Interface.json
	aggregatorJSON string
	//go:embed IERC20.json
	erc20JSON string

	Pool           = mustParse(poolJSON)
	Factory        = mustParse(factoryJSON)
	Manager        = mustParse(managerJSON)
	Quoter         = mustParse(quoterJSON)
	FlashArbitrage = mustParse(flashArbitrageJSON)
	Aggregator     = mustParse(aggregatorJSON) // Chainlink AggregatorV3Interface
	ERC20          = mustParse(erc20JSON)      // 标准 ERC20 接口
)

func mustParse(raw string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(raw))
	if err != nil {
		panic(err)
	}
	return parsed
}
