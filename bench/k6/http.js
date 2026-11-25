// HTTP 压测脚本（k6）
//
//   k6 run -e SCENARIO=quote_cached -e VUS=100 -e DURATION=20s bench/k6/http.js
//
// 环境变量：
//   SCENARIO  场景名，见下面的 scenarios
//   VUS       并发虚拟用户数，默认 100
//   DURATION  持续时间，默认 20s
//   API       后端地址，默认 http://127.0.0.1:18080
//   LABEL     结果文件名后缀（区分同一场景的不同后端配置），默认空
//
// 每个虚拟用户不停地发请求、不加思考时间，量的是后端在这个并发数下的吞吐和延迟。
// 结果写到 bench/results/http_<场景>[_<LABEL>].json。
import http from "k6/http";
import { Counter, Rate } from "k6/metrics";

const deployment = JSON.parse(open("../.run/deploy.json"));
const API = __ENV.API || "http://127.0.0.1:18080";
const SCENARIO = __ENV.SCENARIO || "quote_cached";
const LABEL = __ENV.LABEL || "";
const { WETH, USDC, WBTC } = deployment.tokens;
const pools = deployment.pools.map((p) => p.address);

export const options = {
  vus: Number(__ENV.VUS || 100),
  duration: __ENV.DURATION || "20s",
  summaryTrendStats: ["avg", "min", "med", "p(95)", "p(99)", "max"],
  // 限流场景里 429 / 503 是预期结果，不算 k6 的失败
  thresholds: {},
};

// 按状态码分类计数
const ok = new Counter("status_200");
const rateLimited = new Counter("status_429");
const overloaded = new Counter("status_503");
const otherErrors = new Counter("status_other");
const errorRate = new Rate("errors"); // 既不是 200，也不是预期中的 429 / 503

function record(res) {
  if (res.status === 200) ok.add(1);
  else if (res.status === 429) rateLimited.add(1);
  else if (res.status === 503) overloaded.add(1);
  else otherErrors.add(1);
  errorRate.add(res.status !== 200 && res.status !== 429 && res.status !== 503);
}

// 每次请求一个不同的金额：缓存 key 里带金额，所以一定不命中缓存
function uniqueAmount(base) {
  return (BigInt(base) + BigInt(__VU) * 1000000007n + BigInt(__ITER) * 1009n).toString();
}

const quoteURL = (tokenIn, tokenOut, amount) =>
  `${API}/api/v1/quote?token_in=${tokenIn}&token_out=${tokenOut}&amount_in=${amount}`;
const pick = (list) => list[(__VU * 31 + __ITER) % list.length];

const requests = {
  // 报价，缓存命中：所有请求同一个币对、同一个金额
  quote_cached: () => http.get(quoteURL(WETH, USDC, "1000000000000000000")),
  // 报价，缓存未命中，单跳：WETH → USDC，两个费率档的直连池各模拟一次
  quote_miss_1hop: () => http.get(quoteURL(WETH, USDC, uniqueAmount("1000000000000000000"))),
  // 报价，缓存命中，三跳
  quote_cached_3hop: () => http.get(quoteURL(WETH, WBTC, "1000000000000000000")),
  // 报价，缓存未命中，三跳：WETH → USDC → DAI → WBTC，8 条候选路径，每条模拟 3 次 swap
  quote_miss_3hop: () => http.get(quoteURL(WETH, WBTC, uniqueAmount("1000000000000000000"))),
  // K 线：六个池子轮流，最近 100 根 1m K 线
  klines: () => http.get(`${API}/api/v1/klines?pool=${pick(pools)}&interval=1m&limit=100`),
  // 成交：六个池子轮流，最近 50 笔
  trades: () => http.get(`${API}/api/v1/trades?pool=${pick(pools)}&limit=50`),
  // 构造待签名交易：每次不同金额，要重新搜路由并对链上 Quoter 做一次 eth_call
  build: () =>
    http.post(
      `${API}/api/v1/swap/build`,
      JSON.stringify({
        token_in: WETH,
        token_out: USDC,
        amount_in: uniqueAmount("1000000000000000000"),
        slippage: 0.005,
        recipient: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
      }),
      { headers: { "Content-Type": "application/json" } }
    ),
};

// 混合读：按权重随机选一种请求
const mix = [
  [30, requests.quote_cached],
  [15, requests.quote_miss_1hop],
  [5, requests.quote_miss_3hop],
  [20, requests.klines],
  [15, requests.trades],
  [5, () => http.get(`${API}/api/v1/pools`)],
  [5, () => http.get(`${API}/api/v1/mev/alerts?limit=50`)],
  [5, () => http.get(`${API}/api/v1/status`)],
];
requests.mixed = () => {
  let r = Math.random() * 100;
  for (const [weight, fn] of mix) {
    if ((r -= weight) < 0) return fn();
  }
  return mix[0][1]();
};

export default function () {
  const fn = requests[SCENARIO];
  if (!fn) throw new Error(`未知场景 ${SCENARIO}`);
  record(fn());
}

export function handleSummary(data) {
  const m = data.metrics;
  const count = (name) => (m[name] ? m[name].values.count : 0);
  const duration = m.http_req_duration.values;
  const total = m.http_reqs.values.count;
  const result = {
    scenario: SCENARIO,
    label: LABEL,
    vus: options.vus,
    duration: options.duration,
    requests: total,
    rps: m.http_reqs.values.rate,
    p50_ms: duration.med,
    p95_ms: duration["p(95)"],
    p99_ms: duration["p(99)"],
    max_ms: duration.max,
    status_200: count("status_200"),
    status_429: count("status_429"),
    status_503: count("status_503"),
    status_other: count("status_other"),
    error_rate: m.errors ? m.errors.values.rate : 0,
  };
  const name = `http_${SCENARIO}${LABEL ? "_" + LABEL : ""}`;
  return {
    [`../results/${name}.json`]: JSON.stringify(result, null, 2) + "\n",
    stdout: JSON.stringify(result) + "\n",
  };
}
