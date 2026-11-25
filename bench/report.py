#!/usr/bin/env python3
"""把 bench/results/ 下的压测结果汇总成 bench/REPORT.md

    python3 bench/report.py

报告里的每个数字都直接取自结果文件，不手工誊抄。环境信息在生成报告时从本机读取。
"""
import glob
import json
import os
import re
import statistics
import subprocess
from collections import defaultdict

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RESULTS = os.path.join(ROOT, "bench", "results")


def sh(cmd):
    try:
        return subprocess.check_output(cmd, shell=True, stderr=subprocess.DEVNULL, text=True).strip()
    except subprocess.CalledProcessError:
        return "?"


def load(name):
    path = os.path.join(RESULTS, name)
    return json.load(open(path)) if os.path.exists(path) else None


def jsonl(name):
    path = os.path.join(RESULTS, name)
    if not os.path.exists(path):
        return []
    return [json.loads(line) for line in open(path) if line.strip().startswith("{")]


def table(headers, rows):
    out = ["| " + " | ".join(headers) + " |", "|" + "|".join("---" for _ in headers) + "|"]
    out += ["| " + " | ".join(str(c) for c in row) + " |" for row in rows]
    return "\n".join(out)


def n(value, digits=0):
    """千分位格式"""
    return f"{value:,.{digits}f}"


def ms(value):
    return f"{value:.2f}"


def pct(value):
    return f"{value * 100:.2f}%"


def median_run(runs, key="rps"):
    """按 key 排序取中间那一轮"""
    return sorted(runs, key=lambda r: r[key])[len(runs) // 2]


SCENARIOS = [
    ("quote_cached", "报价，缓存命中，单跳"),
    ("quote_miss_1hop", "报价，缓存未命中，单跳（2 条候选路径）"),
    ("quote_cached_3hop", "报价，缓存命中，三跳"),
    ("quote_miss_3hop", "报价，缓存未命中，三跳（8 条候选路径 × 3 次 swap 模拟）"),
    ("klines", "K 线查询（最近 100 根 1m）"),
    ("trades", "成交查询（最近 50 笔）"),
    ("build", "构造待签名交易（重新搜路由 + 链上 Quoter eth_call）"),
    ("mixed", "混合读（报价 50% / K 线 20% / 成交 15% / 其它 15%）"),
]


def http_section():
    rows, checks, loads = [], [], []
    for scenario, desc in SCENARIOS:
        runs = [json.load(open(p)) for p in sorted(glob.glob(os.path.join(RESULTS, f"http_{scenario}_r[0-9].json")))]
        if not runs:
            continue
        mid = median_run(runs)
        rps = [r["rps"] for r in runs]
        rows.append([
            scenario, desc, mid["vus"], mid["duration"], len(runs), n(mid["rps"]), f"{n(min(rps))} – {n(max(rps))}",
            ms(mid["p50_ms"]), ms(mid["p95_ms"]), ms(mid["p99_ms"]),
            pct(max(r["error_rate"] for r in runs)),
        ])
        checks.append([
            scenario, n(sum(r["requests"] for r in runs)), n(sum(r["server_requests"] for r in runs)),
            "一致" if all(r["requests"] == r["server_requests"] for r in runs) else "不一致",
            n(max(r["backend_rss_mb"] for r in runs), 1),
        ])
        loads += [r["load_avg_1m"] for r in runs]
    text = table(
        ["场景", "说明", "并发", "时长", "轮数", "RPS（中位轮）", "RPS 范围", "p50 ms", "p95 ms", "p99 ms", "错误率（最差轮）"], rows)
    text += "\n\n延迟三列取自 RPS 居中的那一轮。错误率 = 非 200 响应占比。\n\n"
    text += "**与后端指标交叉核对**（`http_requests_total` 在每轮压测前后的增量，各轮合计）：\n\n"
    text += table(["场景", "k6 发出的请求数", "后端 http_requests_total 增量", "核对", "后端常驻内存峰值 MB"], checks)
    if loads:
        text += f"\n\n压测期间的 1 分钟系统负载：{min(loads):.1f} – {max(loads):.1f}（8 核）。"
    return text


def compare(label, scenarios, title_a, title_b, label_a=None):
    rows = []
    for scenario in scenarios:
        base = [json.load(open(p)) for p in sorted(glob.glob(os.path.join(RESULTS, f"http_{scenario}_{label_a or 'r[0-9]'}.json")))]
        other = load(f"http_{scenario}_{label}.json")
        if not base or not other:
            continue
        a = median_run(base)
        rows.append([scenario, title_a, n(a["rps"]), ms(a["p50_ms"]), ms(a["p95_ms"]), ms(a["p99_ms"]), n(a["backend_rss_mb"], 1)])
        rows.append([scenario, title_b, n(other["rps"]), ms(other["p50_ms"]), ms(other["p95_ms"]), ms(other["p99_ms"]), n(other["backend_rss_mb"], 1)])
    return table(["场景", "配置", "RPS", "p50 ms", "p95 ms", "p99 ms", "后端常驻内存 MB"], rows)


def protection_section():
    rows = []
    for label, scenario, config in [
        ("ratelimit", "mixed", "每 IP 100 请求/秒、突发 200；全局并发上限 500"),
        ("backpressure", "quote_miss_3hop", "限流关闭；全局并发上限 20"),
    ]:
        r = load(f"http_{scenario}_{label}.json")
        if not r:
            continue
        total = r["requests"]
        seconds = float(r["duration"].rstrip("s"))
        rows.append([
            config, scenario, r["vus"], r["duration"], n(total), n(r["rps"]),
            f'{n(r["status_200"])}（{pct(r["status_200"] / total)}，{n(r["status_200"] / seconds, 1)}/s）',
            f'{n(r["status_429"])}（{pct(r["status_429"] / total)}）',
            f'{n(r["status_503"])}（{pct(r["status_503"] / total)}）',
            n(r["status_other"]),
            f'{n(r["server_rate_limited"])} / {n(r["server_concurrency_rejected"])}',
            ms(r["p99_ms"]),
        ])
    return table(["后端配置", "场景", "并发", "时长", "请求总数", "总 RPS", "放行（200）", "限流拒绝（429）", "背压拒绝（503）",
                  "其它", "后端计数 429 / 503", "p99 ms"], rows)


def sse_section():
    rows, burst = [], []
    for r in jsonl("sse.jsonl"):
        if r["bench"] == "sse_latency":
            rows.append([r["clients"], r["events"], n(r["deliveries"]), r["missing"],
                         ms(r["e2e_ms_p50"]), ms(r["e2e_ms_p95"]), ms(r["e2e_ms_p99"]), ms(r["e2e_ms_max"]),
                         ms(r["fanout_ms_p50"]), ms(r["fanout_ms_p95"]), ms(r["fanout_ms_max"])])
        else:
            burst.append([r["clients"], r["stalled_clients"], n(r["trades_sent"]), f'{r["send_seconds"]:.1f}', f'{r["drain_seconds"]:.1f}',
                          f'{n(r["deliveries"])} / {n(r["expected_deliveries"])}', f'{r["clients_received_all"]} / {r["clients"]}',
                          r["clients_disconnected"], r["server_dropped_clients"], r["server_clients_after"]])
    text = table(["订阅端数", "成交笔数", "送达次数", "未送达", "端到端 p50 ms", "p95", "p99", "最大", "扇出 p50 ms", "扇出 p95", "扇出最大"], rows)
    if burst:
        text += "\n\n**突发与慢客户端**：\n\n" + table(
            ["正常订阅端", "只连不读的慢客户端", "灌入成交笔数", "发送耗时 s", "全部送达耗时 s", "送达次数 / 应送达", "收全的正常订阅端",
             "被断开的正常订阅端", "后端断开的慢客户端数", "测试结束时在线订阅端"], burst)
    return text


BENCH_LINE = re.compile(r"^(Benchmark\S+?)(?:-\d+)?\s+(\d+)\s+(.*)$")


def gobench(name, pkg=""):
    """解析 go test -bench 输出：同名基准取各指标的中位数。

    基准名前面加上包名（kline 和 mev 都有叫 OnSwap 的基准）；文件里没有 pkg: 行时用参数 pkg。
    """
    samples = defaultdict(lambda: defaultdict(list))
    order = []
    path = os.path.join(RESULTS, name)
    if not os.path.exists(path):
        return {}, [], {}
    for line in open(path):
        if line.startswith("pkg:"):
            pkg = line.split("/")[-1].strip()
            continue
        m = BENCH_LINE.match(line.strip())
        if not m:
            continue
        bench, fields = pkg + "." + m.group(1).replace("Benchmark", "", 1), m.group(3).split()
        if bench not in order:
            order.append(bench)
        for value, unit in zip(fields[0::2], fields[1::2]):
            samples[bench][unit].append(float(value))
    medians = {b: {u: statistics.median(v) for u, v in units.items()} for b, units in samples.items()}
    spread = {b: (min(units["ns/op"]), max(units["ns/op"]), len(units["ns/op"])) for b, units in samples.items()}
    return medians, order, spread


def gobench_section():
    parsed = gobench("gobench.txt")
    if not parsed or not parsed[0]:
        return "（没有结果）"
    medians, order, spread = parsed
    rows = []
    for bench in order:
        m = medians[bench]
        lo, hi, count = spread[bench]
        extra = ""
        if "events/s" in m:
            extra = f'{n(m["events/s"])} 事件/秒，{n(m["blocks/s"])} 区块/秒'
        rows.append([bench, n(m["ns/op"]), f"{n(lo)} – {n(hi)}", n(m.get("B/op", 0)), n(m.get("allocs/op", 0)),
                     n(1e9 / m["ns/op"]), extra, count])
    return table(["基准", "ns/op（中位）", "ns/op 范围", "B/op", "allocs/op", "换算吞吐（次/秒）", "备注", "次数"], rows)


def route_speedup():
    parsed = gobench("gobench.txt")
    if not parsed or not parsed[0]:
        return ""
    medians = parsed[0]
    rows = []
    for count in (8, 32, 64):
        serial = medians.get(f"router.RouteSearch/candidates={count}/serial")
        pool = medians.get(f"router.RouteSearch/candidates={count}/workerpool")
        if serial and pool:
            rows.append([count, n(serial["ns/op"] / 1000, 1), n(pool["ns/op"] / 1000, 1), f'{serial["ns/op"] / pool["ns/op"]:.2f}×'])
    return table(["候选路径数", "串行评估 µs/次", "Worker Pool 并发评估 µs/次", "加速比"], rows)


def before_after_gobench(before, after, names, pkg):
    b = gobench(before, pkg)
    a = gobench(after)
    if not b or not a or not b[0] or not a[0]:
        return "（没有结果）"
    rows = []
    for bench in names:
        if bench in b[0] and bench in a[0]:
            x, y = b[0][bench], a[0][bench]
            row = [bench, n(x["ns/op"]), n(y["ns/op"]), f'{x["ns/op"] / y["ns/op"]:.2f}×']
            if "events/s" in x:
                row += [n(x["events/s"]), n(y["events/s"])]
            rows.append(row)
    headers = ["基准", "修改前 ns/op", "修改后 ns/op", "倍数"]
    if rows and len(rows[0]) == 6:
        headers += ["修改前 事件/秒", "修改后 事件/秒"]
    return table(headers, rows)


def chase_section():
    def summarize(name):
        runs = jsonl(name)
        chase = [r for r in runs if r["bench"] == "chase"]
        reorg = [r for r in runs if r["bench"] == "reorg"]
        return chase, reorg

    rows, reorg_rows = [], []
    for title, name in [("修改前：每个区块一次 eth_getLogs", "chase.before.jsonl"), ("修改后：按区块范围批量 eth_getLogs", "chase.jsonl")]:
        chase, reorg = summarize(name)
        if chase:
            bps = [r["blocks_per_s"] for r in chase]
            eps = [r["events_per_s"] for r in chase]
            secs = [r["seconds"] for r in chase]
            rows.append([title, len(chase), chase[0]["blocks"], chase[0]["events"], f"{statistics.median(secs) * 1000:.0f}",
                         n(statistics.median(bps)), f"{n(min(bps))} – {n(max(bps))}", n(statistics.median(eps)), f"{n(min(eps))} – {n(max(eps))}"])
        if reorg:
            t = [r["ms"] for r in reorg]
            reorg_rows.append([title, len(reorg), reorg[0]["depth"], reorg[0]["blocks_reindexed"], reorg[0]["events_reindexed"],
                               ms(statistics.median(t)), f"{ms(min(t))} – {ms(max(t))}"])
    text = table(["实现", "次数", "区块数", "事件数", "耗时 ms（中位）", "区块/秒（中位）", "区块/秒 范围", "事件/秒（中位）", "事件/秒 范围"], rows)
    text += "\n\n**一次 Reorg 的回滚并重建**（发现父哈希不一致 → 找分叉点 → 撤销池子状态、K 线、告警 → 重新索引到新链头）：\n\n"
    text += table(["实现", "次数", "Reorg 深度（区块）", "重新索引的区块数", "重新索引的事件数", "耗时 ms（中位）", "范围 ms"], reorg_rows)
    return text


def main():
    seed = json.load(open(os.path.join(ROOT, "bench", "results", "seed.json"))) if os.path.exists(os.path.join(RESULTS, "seed.json")) else {}
    mem_gb = int(sh("sysctl -n hw.memsize")) / 2**30
    env = table(["项目", "值"], [
        ["机型 / CPU", f'{sh("sysctl -n hw.model")} / {sh("sysctl -n machdep.cpu.brand_string")}，{sh("sysctl -n hw.ncpu")} 核'],
        ["内存", f"{mem_gb:.0f} GB"],
        ["操作系统", f'macOS {sh("sw_vers -productVersion")}'],
        ["Go", sh("go version").replace("go version ", "")],
        ["Node.js / Hardhat", f'{sh("node --version")} / {sh("cd " + ROOT + "/contracts && node_modules/.bin/hardhat --version 2>/dev/null | tail -1")}'],
        ["k6", sh("k6 version").split(" (")[0]],
        ["链节点", "Hardhat 本地节点（与后端、k6 在同一台机器上）"],
    ])

    precap = load("http_quote_miss_1hop_precap.json")
    parts = {
        "ENV": env,
        "SEED_SWAPS": n(seed.get("swaps", 0)),
        "SEED_BLOCKS": seed.get("headBlock", "?"),
        "SEED_PER_BLOCK": seed.get("perBlock", "?"),
        "SEED_SECS": seed.get("blockSeconds", "?"),
        "HTTP": http_section(),
        "ACCESSLOG": compare("accesslog", ["quote_cached", "mixed"], "访问日志关闭", "访问日志开启（每个请求一行结构化日志）"),
        "PROTECTION": protection_section(),
        "BP_BEFORE": n((load("http_quote_miss_3hop_backpressure_warnlog.json") or {}).get("status_200", 0)),
        "BP_AFTER": n((load("http_quote_miss_3hop_backpressure.json") or {}).get("status_200", 0)),
        "CACHECAP": compare("precap", ["quote_miss_1hop"], "修改后：缓存最多 10000 条", "修改前：缓存不设上限") if precap else "（没有结果）",
        "SSE": sse_section(),
        "GOBENCH": gobench_section(),
        "ROUTE": route_speedup(),
        "REPLAY": before_after_gobench("replay.before.txt", "gobench.txt",
                                       ["indexer.Replay/indexer_only", "indexer.Replay/with_kline_mev_oracle"], "indexer"),
        "MEV": before_after_gobench("mev.before.txt", "gobench.txt",
                                    ["mev.OnSwap/swaps_per_block=5", "mev.OnSwap/swaps_per_block=20", "mev.OnSwap/swaps_per_block=100",
                                     "mev.OnSwapWorstCase/swaps_per_block=64", "mev.OnSwapWorstCase/swaps_per_block=256"], "mev"),
        "CHASE": chase_section(),
    }
    template = open(os.path.join(ROOT, "bench", "report.tmpl")).read()
    for key, value in parts.items():
        template = template.replace("{{" + key + "}}", str(value))
    open(os.path.join(ROOT, "bench", "REPORT.md"), "w").write(template)
    print("已生成 bench/REPORT.md")


if __name__ == "__main__":
    main()
