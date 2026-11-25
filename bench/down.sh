#!/usr/bin/env bash
# 清理压测环境：停掉后端和 Hardhat 节点，确认没有残留进程
#
#   bench/down.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN="$ROOT/bench/.run"

"$ROOT/bench/backend.sh" stop 2>/dev/null || true

if [[ -f "$RUN/node.pid" ]]; then
  pid="$(cat "$RUN/node.pid")"
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 30); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  kill -9 "$pid" 2>/dev/null || true
  rm -f "$RUN/node.pid"
fi

left=0
for name in "$RUN/unigo-dex" "hardhat node --hostname 127.0.0.1 --port ${BENCH_RPC_PORT:-18545}"; do
  if pgrep -f "$name" >/dev/null 2>&1; then
    echo "仍有残留进程: $name" >&2
    pgrep -fl "$name" >&2
    left=1
  fi
done
[[ $left -eq 0 ]] && echo "压测环境已清理"
exit $left
