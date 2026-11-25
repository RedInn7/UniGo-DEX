# 快速开始、环境变量与 API

> 小节编号沿用原文：1–13 见 [contracts.md](contracts.md)，14–22 见 [backend.md](backend.md)，23–26 见 [market-mev.md](market-mev.md)，27–32 见 [infrastructure.md](infrastructure.md)；测试见 [testing.md](testing.md)，压测见 [benchmark.md](benchmark.md)，部署与 API 见 [getting-started.md](getting-started.md)。

### 环境要求

- Node.js >= 18
- Go >= 1.24

### 编译与测试

```bash
# 克隆项目
git clone <repo-url>
cd UniGo-DEX

# === 智能合约 ===
cd contracts
npm install
npx hardhat compile             # 编译合约（项目自身 29 个 .sol 文件）
npx hardhat test                # 109 个合约测试
REPORT_GAS=1 npx hardhat test   # 同时打印 Gas 基准和 gas-reporter 汇总

# === Go 后端 ===
cd ../backend
go build ./...                  # 编译全部 20 个包
go test -p 2 ./...              # 单元测试（16 个包，101 个测试函数）

# 端到端集成测试（自起 Hardhat 节点，需要 contracts/ 已 npm install）
go test -p 2 -tags integration -count=1 -timeout 15m ./integration

# Goroutine 池 benchmark
go test ./workerpool/ -run '^$' -bench=. -benchmem -count=3
```

也可以用 Makefile：

| 命令 | 作用 |
|------|------|
| `make install` | 安装合约和后端依赖 |
| `make compile` | 编译合约 |
| `make test` / `make test-go` / `make test-all` | 合约测试 / Go 单元测试 / 两者 |
| `make test-integration` | 端到端集成测试 |
| `make gas` | 合约测试并打印 Gas 报告 |
| `make bench` | 全套压测（`bench/run-all.sh`，约 25 分钟，需要 k6） |
| `make abi` | 编译合约并重新生成后端内嵌的 ABI |
| `make node` / `make deploy` | 启动本地 Hardhat 节点 / 部署到本地节点 |
| `make build-backend` / `make run-backend` | 编译 / 运行后端 |

### 本地部署并启动后端

```bash
# 终端 1：启动本地 Hardhat 节点（http://127.0.0.1:8545，chainId 31337）
cd contracts && npx hardhat node

# 终端 2：部署一套可以直接交易的环境
cd contracts && npx hardhat run scripts/deploy.js --network localhost
```

部署脚本做的事：

1. 部署核心合约：UniGoFactory / UniGoManager / UniGoQuoter / FlashArbitrage
2. 部署 4 个测试代币 WETH / USDC / DAI / WBTC（都是 18 位小数），给前 4 个测试账户铸币并授权 Manager
3. 创建 6 个池子，覆盖三档费率，能组成多跳路径：WETH/USDC 0.05% 和 0.3%、USDC/DAI 0.05% 和 0.3%、WBTC/DAI 0.3% 和 1%
4. 每个池子初始化价格并注入 4 个流动性头寸（一个宽区间打底，叠加窄区间和上下各一个单边区间）
5. 部署一个模拟的 Chainlink 聚合器（ETH / USD，8 位小数，初始 2000）
6. 用链上 Quoter 自检一笔报价，最后打印后端需要的全部环境变量

在全新启动的节点上地址是固定的，输出如下（以脚本实际输出为准）：

```
池子 WETH/USDC 0.05% -> 0x93bEA4f2139418874a17B84a9b6928C8573E0330（tick -76013，4 个头寸）
池子 WETH/USDC 0.3% -> 0x8C66336e08638483C152BF67119f6D0317e7bBa6（tick -76013，4 个头寸）
池子 USDC/DAI 0.05% -> 0xe65EF356101bF9c486523719e56613C542289eE3（tick 0，4 个头寸）
池子 USDC/DAI 0.3% -> 0xcF2bE6A55ddf8FEC7E1C4a2Fc714013720E771f0（tick 0，4 个头寸）
池子 WBTC/DAI 0.3% -> 0xf0E138e1e254A187CEDd3f3208191888c1A526B1（tick 110026，4 个头寸）
池子 WBTC/DAI 1% -> 0x49C6ac9404940B0C5592BE05369964962BB8615B（tick 110026，4 个头寸）
自检：1 WETH -> 1998.996425874440730518 USDC（0.05% 池）

WETH  0xDc64a140Aa3E981100a9becA4E685f962f0cF6C9
USDC  0x610178dA211FEF7D417bC0e6FeD39F05609AD788
DAI   0x959922bE3CAee4b8Cd9a407cc3ac1C251C2007B1
WBTC  0x4ed7c70F96B99c776995fB64377f0d4aB3B0e1C1
```

```bash
# 终端 3：把脚本打印的启动命令复制过来，在 backend/ 目录下执行
cd backend
RPC_URL=http://127.0.0.1:8545 \
FACTORY_ADDRESS=0x5FbDB2315678afecb367f032d93F642f64180aa3 \
MANAGER_ADDRESS=0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512 \
QUOTER_ADDRESS=0x9fE46736679d2D9a65F0992F2272dE9f3c7fa6e0 \
FLASH_ARB_ADDRESS=0xCf7Ed3AccA5a467e9e704C703E8D87F634fB0Fc9 \
WETH_ADDRESS=0xDc64a140Aa3E981100a9becA4E685f962f0cF6C9 \
CHAINLINK_FEEDS=0xDc64a140Aa3E981100a9becA4E685f962f0cF6C9:0x610178dA211FEF7D417bC0e6FeD39F05609AD788:0x922D6956C99E12DFeB3224DEA977D0939758A1Fe:18:18:3600 \
go run .                        # API 运行在 :8080
```

验证：

```bash
curl localhost:8080/health
curl localhost:8080/api/v1/pools           # 索引器从链上事件重建出的 6 个池子
curl localhost:8080/api/v1/status

# 报价：1 WETH -> USDC（单跳）
curl "localhost:8080/api/v1/quote?token_in=0xDc64a140Aa3E981100a9becA4E685f962f0cF6C9&token_out=0x610178dA211FEF7D417bC0e6FeD39F05609AD788&amount_in=1000000000000000000"
# {"amount_out":"1998996425874440730518","min_amount_out":"1989001443745068526865","slippage":0.005,
#  "path":["0xdc64…","0x6101…"],"fees":[500],"gas_estimate":150000,"gas":{…,"source":"chain"},
#  "price_checks":[{"pool":"0x93be…","spot":0.0005,"sources":["spot","chainlink"],"status":"unavailable",
#                   "detail":"预言机观测不足","chainlink_status":"ok","chainlink":0.0005}], …}

# 报价：1 WETH -> WBTC（三跳 WETH → USDC → DAI → WBTC，fees = [500, 500, 3000]）
curl "localhost:8080/api/v1/quote?token_in=0xDc64a140Aa3E981100a9becA4E685f962f0cF6C9&token_out=0x4ed7c70F96B99c776995fB64377f0d4aB3B0e1C1&amount_in=1000000000000000000"

curl "localhost:8080/api/v1/twap?pool=0x93bea4f2139418874a17b84a9b6928c8573e0330"   # 现价校验明细
curl -i -H "X-Trace-Id: my-trace-12345" localhost:8080/health                       # 响应头带回同一个 X-Trace-Id
curl -N localhost:8080/api/v1/stream                                                # 订阅实时推送
curl localhost:8080/metrics
```

链下报价 `1998996425874440730518` 与部署脚本里链上 Quoter 自检的 `1998.996425874440730518 USDC` 是同一个数。池子刚创建时 TWAP 观测还不足，`price_checks` 里是“现价 + Chainlink”两方校验；链上继续出块、最老一条观测距最新区块满 60 秒之后，TWAP 开始参与，变为三方校验。

> 使用说明：不配置任何环境变量直接 `go run .` 时，索引器使用模拟源，服务同样可以启动。部署到其它网络时，`CHAINLINK_FEEDS` 填该网络上真实的聚合器地址和代币的 decimals。

### 构建子图

```bash
cd subgraph
npm install
npm run abis       # 从 contracts/artifacts 提取 Factory 和 Pool 的 ABI（需先编译合约）
npm run codegen    # 由 schema 和 ABI 生成 AssemblyScript 类型
npm run build      # 编译成 WASM
```

构建产物部署到自行运行的 graph-node 上即可查询（见第 13 节）。

### 环境变量

**后端**（`backend/main.go`）：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `RPC_URL` | 空 | 以太坊节点 RPC 地址。未配置或连不上时索引器使用模拟源 |
| `FACTORY_ADDRESS` | 空 | Factory 合约地址，用于发现池子（只采信它发出的 `PoolCreated`）。未配置时索引器使用模拟源 |
| `START_BLOCK` | `0` | 开始索引的区块号 |
| `CONFIRMATION_BLOCKS` | `12` | 交易最终确认所需的区块数；Reorg 回滚窗口为它的 2 倍 |
| `POLL_INTERVAL_MS` | `2000` | 索引器轮询新区块的间隔（毫秒） |
| `CHAIN_ID` | `31337` | 未连接节点时 EIP-712 域使用的链 ID；连上节点后以节点返回的为准 |
| `MANAGER_ADDRESS` | 空 | UniGoManager 地址：待签名交易的目标，也是 EIP-712 的 `verifyingContract`。未配置时 `/api/v1/swap/build` 返回 503 |
| `QUOTER_ADDRESS` | 空 | UniGoQuoter 地址。配置后对链下报价做链上一致性抽检 |
| `FLASH_ARB_ADDRESS` | 空 | FlashArbitrage 地址，填充套利机会响应里的 `to` |
| `WETH_ADDRESS` | 空 | WETH 地址，用于把 gas 成本折算成借入 token |
| `CHAINLINK_FEEDS` | 空 | Chainlink 喂价配置，多个用逗号分隔：`base:quote:聚合器地址:baseDecimals:quoteDecimals[:心跳秒数]`，心跳缺省 3600。需要已连接节点 |
| `CHAINLINK_REFRESH_SECONDS` | `15` | 喂价读数的缓存时间（秒） |
| `TWAP_WINDOW` | `600` | TWAP 窗口（秒） |
| `RATE_LIMIT_PER_IP` | `100` | 每 IP 的持续速率（请求/秒），`0` = 不限流 |
| `RATE_LIMIT_BURST` | `200` | 每 IP 的突发容量，`0` = 与速率相同 |
| `MAX_CONCURRENT` | `500` | 全局并发上限，`0` = 不限 |
| `LOG_LEVEL` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |
| `LOG_FORMAT` | `text` | 日志格式：`text` / `json` |
| `ACCESS_LOG` | 开启 | 设为 `off` 关闭逐请求的访问日志 |
| `PORT` | `8080` | HTTP 监听端口 |

**部署脚本**（`contracts/scripts/deploy.js`）：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `RPC_URL` | `http://127.0.0.1:8545` | `--network localhost` 连接的节点地址 |
| `DEPLOY_OUTPUT` | 未设置 | 把部署结果（地址清单）写成 JSON 文件的路径 |

**合约测试**：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `REPORT_GAS` | 未设置 | 设置后打印 Gas 基准表并启用 hardhat-gas-reporter |
| `INVARIANT_SEED` | `42` | 不变量随机测试的种子 |
| `INVARIANT_STEPS` | `300` | 不变量随机测试的步数 |

### API 端点

以 `backend/api/server.go` 注册的路由为准。数量一律用十进制字符串传递，避免 JSON 精度丢失。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/quote?token_in=&token_out=&amount_in=[&slippage=]` | 报价：最优路径、产出、价格冲击、推荐滑点与 `min_amount_out`、Gas 成本、每个池子的现价校验明细 `price_checks`、价格安全警告。找不到可成交的路径返回 404 |
| POST | `/api/v1/swap/build` | 把路由编码成 Manager 的调用数据，返回待签名交易。body：`token_in` `token_out` `recipient`，`amount_in` 与 `amount_out` 二选一，可选 `slippage` `deadline` `from`。响应带 `price_checks`；现价偏离 TWAP 或 Chainlink 喂价时返回 403 |
| POST | `/api/v1/swap` | 提交交易：EIP-712 验签 + 价格安全检查 + MEV 检查；带 `raw_tx` 时广播并返回交易哈希（节点明确拒绝返回 400，连不上节点返回 502） |
| GET | `/api/v1/nonce/{address}` | EIP-712 签名用的 nonce |
| GET | `/api/v1/tx/{hash}` | 交易状态：`pending` / `pending_confirmation` / `confirmed` / `reverted` |
| GET | `/api/v1/pools` | 所有池子的价格、tick、活跃流动性 |
| GET | `/api/v1/klines?pool=[&interval=1m][&limit=100]` | K 线，周期 `1m` `5m` `15m` `1h` `4h` `1d`，`limit` 最大 1000；`volume0` / `volume1` 是十进制字符串 |
| GET | `/api/v1/trades?pool=[&limit=50]` | 最近成交，最新的在前 |
| GET | `/api/v1/twap?pool=` | 池子的现价校验明细：TWAP 与 Chainlink 喂价各自的读数、偏离和状态，结构与 `price_checks` 的元素相同 |
| GET | `/api/v1/mev/alerts[?limit=100]` | 三明治攻击告警，最新的在前 |
| GET | `/api/v1/arbitrage/opportunities` | 当前套利机会与调用数据，净利润高的在前 |
| GET | `/api/v1/stream[?types=trade,kline,mev_alert,arbitrage]` | SSE 实时推送 |
| GET | `/api/v1/status` | 系统状态（索引器进度、Reorg 次数、路由统计、处于保护模式的交易对 `protected_pairs`） |
| GET | `/metrics` | Prometheus 文本格式指标（含 P50 / P95 / P99） |
| GET | `/health` | 健康检查 |

`/api/v1/tx/{hash}` 和 `/api/v1/twap` 需要已连接节点，否则返回 503。每个响应都带 `X-Trace-Id` 响应头；请求带了合法的 `X-Trace-Id` 就沿用它。

