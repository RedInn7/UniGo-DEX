#!/usr/bin/env bash
# 启动 / 停止压测用的后端进程（节点和合约由 up.sh 负责）
#
#   bench/backend.sh start    # 按 bench/.run/deploy.json 里的地址启动，等索引器追到链头
#   bench/backend.sh stop
#
# 行为由环境变量控制（都有默认值）：
#   RATE_LIMIT_PER_IP  每 IP 限流速率，默认 0（关闭，测吞吐上限）
#   RATE_LIMIT_BURST   每 IP 突发容量，默认 0
#   MAX_CONCURRENT     全局并发上限，默认 0（关闭）
#   ACCESS_LOG         访问日志，默认 off
#   POLL_INTERVAL_MS   索引器轮询间隔，默认 200
#   BENCH_API_PORT     后端端口，默认 18080
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN="$ROOT/bench/.run"
API_PORT="${BENCH_API_PORT:-18080}"

stop() {
  if [[ -f "$RUN/backend.pid" ]]; then
    pid="$(cat "$RUN/backend.pid")"
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 50); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$pid" 2>/dev/null || true
    rm -f "$RUN/backend.pid"
  fi
}

start() {
  [[ -f "$RUN/deploy.json" ]] || { echo "没有 $RUN/deploy.json，先执行 bench/up.sh" >&2; exit 1; }
  stop
  json() { python3 -c "import json,sys; d=json.load(open('$RUN/deploy.json')); print(eval('d'+sys.argv[1]))" "$1"; }

  if [[ ! -x "$RUN/unigo-dex" || -n "${BENCH_REBUILD:-}" ]]; then
    (cd "$ROOT/backend" && go build -p 2 -o "$RUN/unigo-dex" .)
  fi

  RPC_URL="$(json "['rpcUrl']")" \
  FACTORY_ADDRESS="$(json "['factory']")" \
  MANAGER_ADDRESS="$(json "['manager']")" \
  QUOTER_ADDRESS="$(json "['quoter']")" \
  FLASH_ARB_ADDRESS="$(json "['flashArbitrage']")" \
  WETH_ADDRESS="$(json "['tokens']['WETH']")" \
  CHAINLINK_FEEDS="$(json "['chainlinkFeeds']")" \
  PORT="$API_PORT" \
  RATE_LIMIT_PER_IP="${RATE_LIMIT_PER_IP:-0}" \
  RATE_LIMIT_BURST="${RATE_LIMIT_BURST:-0}" \
  MAX_CONCURRENT="${MAX_CONCURRENT:-0}" \
  ACCESS_LOG="${ACCESS_LOG:-off}" \
  POLL_INTERVAL_MS="${POLL_INTERVAL_MS:-200}" \
  LOG_LEVEL="${LOG_LEVEL:-warn}" \
    nohup "$RUN/unigo-dex" >"$RUN/backend.log" 2>&1 &
  echo $! >"$RUN/backend.pid"

  # 等索引器追到链头
  head_hex="$(curl -s -X POST -H 'content-type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' "$(json "['rpcUrl']")" |
    python3 -c "import json,sys; print(int(json.load(sys.stdin)['result'],16))")"
  for _ in $(seq 1 600); do
    last="$(curl -s -m 2 "http://127.0.0.1:$API_PORT/api/v1/status" 2>/dev/null |
      python3 -c "import json,sys; print(json.load(sys.stdin)['indexer_last_block'])" 2>/dev/null || echo -1)"
    if [[ "$last" -ge "$head_hex" ]]; then
      echo "后端已就绪：http://127.0.0.1:$API_PORT（索引到区块 $last，限流 ${RATE_LIMIT_PER_IP:-0}/s，并发上限 ${MAX_CONCURRENT:-0}，访问日志 ${ACCESS_LOG:-off}）"
      return 0
    fi
    kill -0 "$(cat "$RUN/backend.pid")" 2>/dev/null || { echo "后端进程已退出，见 $RUN/backend.log" >&2; exit 1; }
    sleep 0.5
  done
  echo "后端 300 秒内没有追到链头" >&2
  exit 1
}

case "${1:-}" in
  start) start ;;
  stop) stop ;;
  *) echo "用法: $0 start|stop" >&2; exit 1 ;;
esac
