# UniGo-DEX Makefile
# 提供常用操作的快捷命令

.PHONY: install compile test test-go deploy clean build-backend run-backend help

# 默认目标：显示帮助
help:
	@echo "UniGo-DEX 项目命令："
	@echo ""
	@echo "  make install        - 安装依赖"
	@echo "  make compile        - 编译 Solidity 合约"
	@echo "  make test           - 运行合约测试"
	@echo "  make test-go        - 运行 Go 后端测试"
	@echo "  make test-all       - 运行所有测试"
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

# 运行所有测试
test-all: test test-go

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
