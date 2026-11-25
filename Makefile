# UniGo-DEX Makefile
# 提供常用操作的快捷命令

.PHONY: install compile test test-go test-integration test-all gas bench abi node deploy clean build-backend run-backend all help

# 默认目标：显示帮助
help:
	@echo "UniGo-DEX 项目命令："
	@echo ""
	@echo "  make install        - 安装依赖"
	@echo "  make compile        - 编译 Solidity 合约"
	@echo "  make test           - 运行合约测试"
	@echo "  make test-go        - 运行 Go 后端测试"
	@echo "  make test-integration - 运行端到端集成测试（自起 Hardhat 节点）"
	@echo "  make test-all       - 运行合约测试和 Go 单元测试"
	@echo "  make gas            - 运行合约测试并打印 Gas 报告"
	@echo "  make bench          - 运行全套压测并生成 bench/REPORT.md（约 25 分钟，需要 k6）"
	@echo "  make abi            - 由编译产物重新生成后端内嵌的 ABI"
	@echo "  make deploy         - 部署合约到本地网络"
	@echo "  make node           - 启动本地 Hardhat 节点"
	@echo "  make build-backend  - 编译 Go 后端"
	@echo "  make run-backend    - 运行 Go 后端 API 服务"
	@echo "  make clean          - 清理编译产物"
	@echo "  make all            - 安装依赖 + 编译 + 测试"

# 安装依赖
install:
	cd contracts && npm install
	cd backend && go mod download

# 编译合约
compile:
	cd contracts && npx hardhat compile

# 运行合约测试
test:
	cd contracts && npx hardhat test

# 运行 Go 后端测试
test-go:
	cd backend && go test ./... -v

# 运行端到端集成测试（需要先 make install；测试自己拉起 Hardhat 节点）
test-integration:
	cd backend && go test -p 2 -tags integration -count=1 -timeout 15m ./integration

# 运行合约测试和 Go 单元测试
test-all: test test-go

# 运行合约测试并打印 Gas 报告
gas:
	cd contracts && REPORT_GAS=1 npx hardhat test

# 运行全套压测（Hardhat 节点 + 后端 + k6），结果写入 bench/results/ 并生成 bench/REPORT.md
bench:
	bench/run-all.sh

# 由 Hardhat 编译产物重新生成后端内嵌的 ABI
abi: compile
	cd backend && go generate ./chain/...

# 启动本地节点
node:
	cd contracts && npx hardhat node

# 部署到本地网络（需要先启动节点）
deploy:
	cd contracts && npx hardhat run scripts/deploy.js --network localhost

# 编译 Go 后端
build-backend:
	cd backend && go build -o unigo-dex .

# 运行 Go 后端
run-backend:
	cd backend && go run main.go

# 清理编译产物
clean:
	cd contracts && npx hardhat clean
	rm -rf contracts/cache contracts/artifacts
	rm -f backend/unigo-dex

# 完整构建流程
all: install compile test build-backend test-go
	@echo "所有构建步骤完成！"
