#!/usr/bin/env bash
# 跑一组 HTTP 压测场景，并用后端自己的指标核对 k6 的请求数
#
#   bench/run-http.sh [场景...]            # 不给场景就跑全部
#   LABEL=limited bench/run-http.sh mixed  # 结果文件名带后缀，区分不同的后端配置
#
# 环境变量：VUS（默认 100）、DURATION（默认 20s）、BENCH_API_PORT（默认 18080）
#
# 每个场景结束后，在 bench/results/http_<场景>[_<LABEL>].json 里补上：
#   server_requests / server_rate_limited / server_concurrency_rejected  后端 /metrics 在这段时间的增量
#   backend_rss_mb  场景结束时后端进程的常驻内存
#   load_avg_1m     场景结束时的 1 分钟系统负载
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
API="http://127.0.0.1:${BENCH_API_PORT:-18080}"
LABEL="${LABEL:-}"
SCENARIOS=("$@")
if [[ ${#SCENARIOS[@]} -eq 0 ]]; then
  SCENARIOS=(quote_cached quote_miss_1hop quote_cached_3hop quote_miss_3hop klines trades build mixed)
fi
mkdir -p "$ROOT/bench/results"

metric() { curl -s "$API/metrics" | awk -v name="$1" '$1 == name { print $2 }'; }

for scenario in "${SCENARIOS[@]}"; do
  before_total="$(metric http_requests_total)"
  before_429="$(metric http_rate_limited_total)"
  before_503="$(metric http_concurrency_rejected_total)"

  (cd "$ROOT/bench/k6" && k6 run --quiet -e SCENARIO="$scenario" -e LABEL="$LABEL" -e API="$API" \
    -e VUS="${VUS:-100}" -e DURATION="${DURATION:-20s}" http.js) | tail -1

  sleep 1 # 等在途请求结束
  file="$ROOT/bench/results/http_${scenario}${LABEL:+_$LABEL}.json"
  python3 - "$file" "$(($(metric http_requests_total) - before_total))" \
    "$(($(metric http_rate_limited_total) - before_429))" "$(($(metric http_concurrency_rejected_total) - before_503))" \
    "$(ps -o rss= -p "$(cat "$ROOT/bench/.run/backend.pid")")" "$(sysctl -n vm.loadavg | awk '{print $2}')" <<'PY'
import json, sys
path, total, limited, rejected, rss_kb, load = sys.argv[1:]
result = json.load(open(path))
# 采集指标本身的那几次 /metrics 请求不算
result["server_requests"] = int(total) - 3
result["server_rate_limited"] = int(limited)
result["server_concurrency_rejected"] = int(rejected)
result["backend_rss_mb"] = round(int(rss_kb) / 1024, 1)
result["load_avg_1m"] = float(load)
json.dump(result, open(path, "w"), indent=2)
open(path, "a").write("\n")
PY
  sleep 2
done
