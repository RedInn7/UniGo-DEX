#!/usr/bin/env bash
# 复现 bench/REPORT.md 里的全部压测（约 25 分钟）
#
#   bench/run-all.sh
#
# 各步骤也可以单独执行；结果都落在 bench/results/，最后用 bench/report.py 汇总成表格。
# 压测期间不要同时跑别的重负载（编译、测试），否则数字会明显变差。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# 1. 环境：Hardhat 节点 + 合约 + 3000 笔历史成交 + 后端（限流、并发上限、访问日志都关闭）
bench/up.sh

# 2. HTTP 吞吐上限：8 个场景，各 100 并发 × 20 秒，重复 3 轮看波动
for round in 1 2 3; do
  LABEL="r$round" bench/run-http.sh
done

# 3. 访问日志的开销：打开逐请求日志再测一次
ACCESS_LOG=on LOG_LEVEL=info bench/backend.sh start
LABEL=accesslog bench/run-http.sh quote_cached mixed

# 4. 限流生效：默认配置（每 IP 100 请求/秒、突发 200，全局并发上限 500）
RATE_LIMIT_PER_IP=100 RATE_LIMIT_BURST=200 MAX_CONCURRENT=500 bench/backend.sh start
LABEL=ratelimit bench/run-http.sh mixed

# 5. 背压生效：关掉限流，把全局并发上限压到 20，用最重的请求打
RATE_LIMIT_PER_IP=0 MAX_CONCURRENT=20 bench/backend.sh start
LABEL=backpressure bench/run-http.sh quote_miss_3hop

# 6. 索引追块与 Reorg 重建（放在 SSE 之前：SSE 的突发测试会往链上再灌几千笔成交，数据规模就变了）
bench/backend.sh start
(cd backend && go run ./cmd/bench chase -runs 6 -reorg-depth 10 -reorg-runs 5) | tee bench/results/chase.jsonl

# 7. SSE 推送：不同订阅端数量下的延迟；再加 5 个只连不读的慢客户端做突发测试
(
  cd backend
  for clients in 50 200 500; do
    go run ./cmd/bench sse -clients "$clients" -events 60
  done
  go run ./cmd/bench sse -clients 200 -stalled 5 -events 0 -burst 6000
) | tee bench/results/sse.jsonl

# 8. Go 基准
(cd backend && go test -p 2 -run '^$' -bench . -benchmem -count 3 ./router ./kline ./mev ./indexer) |
  grep -E '^(Benchmark|pkg:|cpu:)' | tee bench/results/gobench.txt

bench/down.sh
python3 bench/report.py
