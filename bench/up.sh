#!/usr/bin/env bash
# 一条命令拉起压测环境：Hardhat 节点 → 部署全套合约和池子 → 灌入历史成交 → 启动后端
#
#   bench/up.sh
#
# 环境变量：
#   BENCH_RPC_PORT   节点端口，默认 18545
#   BENCH_API_PORT   后端端口，默认 18080
#   BENCH_SWAPS      灌入的成交笔数，默认 3000（另见 bench/seed.js）
#   其余限流 / 日志开关见 bench/backend.sh
#
# 清理：bench/down.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN="$ROOT/bench/.run"
RPC_PORT="${BENCH_RPC_PORT:-18545}"
export RPC_URL="http://127.0.0.1:$RPC_PORT"

"$ROOT/bench/down.sh" >/dev/null 2>&1 || true
mkdir -p "$RUN"

rpc() {
  curl -s -m 2 -X POST -H 'content-type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":[]}" "$RPC_URL"
}

echo "[1/4] 启动 Hardhat 节点（端口 $RPC_PORT）"
if rpc eth_chainId | grep -q result; then
  echo "端口 $RPC_PORT 上已经有节点在运行，换一个 BENCH_RPC_PORT" >&2
  exit 1
fi
(cd "$ROOT/contracts" && exec nohup node_modules/.bin/hardhat node --hostname 127.0.0.1 --port "$RPC_PORT") >"$RUN/node.log" 2>&1 &
echo $! >"$RUN/node.pid"
for _ in $(seq 1 120); do
  rpc eth_chainId | grep -q result && break
  sleep 0.5
done
rpc eth_chainId | grep -q result || { echo "节点 60 秒内没有就绪，见 $RUN/node.log" >&2; exit 1; }

echo "[2/4] 部署合约、池子和流动性"
(cd "$ROOT/contracts" && DEPLOY_OUTPUT="$RUN/deploy.json" node_modules/.bin/hardhat run scripts/deploy.js --network localhost) >"$RUN/deploy.log" 2>&1 ||
  { echo "部署失败，见 $RUN/deploy.log" >&2; exit 1; }

echo "[3/4] 灌入历史成交"
# seed.js 在 contracts/ 之外，用 NODE_PATH 让它能找到 contracts/node_modules 里的 hardhat
(cd "$ROOT/contracts" && NODE_PATH="$ROOT/contracts/node_modules" DEPLOY_FILE="$RUN/deploy.json" \
  node_modules/.bin/hardhat run ../bench/seed.js --network localhost) 2>&1 |
  tee "$RUN/seed.log" | grep "灌入" || { echo "灌入失败，见 $RUN/seed.log" >&2; exit 1; }

echo "[4/4] 启动后端"
BENCH_REBUILD=1 "$ROOT/bench/backend.sh" start
