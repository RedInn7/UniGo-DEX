# Go 后端：行情与 MEV

> 小节编号沿用原文：1–13 见 [contracts.md](contracts.md)，14–22 见 [backend.md](backend.md)，23–26 见 [market-mev.md](market-mev.md)，27–32 见 [infrastructure.md](infrastructure.md)；测试见 [testing.md](testing.md)，压测见 [benchmark.md](benchmark.md)，部署与 API 见 [getting-started.md](getting-started.md)。

### 23. K 线与成交记录（Reorg 重算）

**问题**：前端要画 K 线、展示最近成交。这些数据都来自 Swap 事件，而 Swap 事件所在的区块可能被 Reorg 掉，已经画出去的 K 线就包含了不存在的成交。

`kline.Aggregator` 订阅索引器的 Swap 事件流，按池子聚合出 **1m / 5m / 15m / 1h / 4h / 1d** 六个周期的 OHLCV，并保留最近的逐笔成交：

- **价格**：由成交后的 `sqrtPriceX96` 换算，`price = (sqrtPriceX96 / 2^96)²`
- **成交量**：`amount0 / amount1` 的绝对值，用定点整数（`big.Int`，最小单位）累加，不丢精度；接口里的 `volume0 / volume1` 是十进制字符串，避免超出 JavaScript 的安全整数范围
- **时间**：区块时间戳

每笔成交依次并入六个周期的最后一根 K 线（或开一根新的）：

```go
// kline/kline.go
func (a *Aggregator) merge(series []candle, secs int64, c candle) []candle {
    c.openTime = bucket(c.openTime, secs)             // 对齐到周期起点
    if n := len(series); n > 0 && series[n-1].openTime == c.openTime {
        last := &series[n-1]
        last.high = max(last.high, c.high)
        last.low = min(last.low, c.low)
        last.close = c.close
        last.volume0.Add(last.volume0, c.volume0)     // 定点整数原地累加
        last.volume1.Add(last.volume1, c.volume1)
        last.trades += c.trades
        return series
    }
    // ... 追加新的一根，超过 MaxCandles 时淘汰最老的
}
```

**Reorg 回滚：按区块检查点恢复**

一根 K 线的最高价、最低价不能“减去”一笔成交，所以回滚不能靠逆运算。做法是检查点：每个区块的第一笔成交进来之前，先把当时最新的那根 1m K 线存一份。

```
区块:        100        101        102 │ 103        104        │ = 分叉点（forkBlock = 103）
检查点:      cp100      cp101      cp102 │ cp103      cp104
                                         │   ▲
                                         │   └ 区块 103 的成交进来之前，最新那根 1m K 线的样子

回滚 >= 103：
1. 取第一个被回滚区块的检查点 cp103，丢弃 cp103 及之后的检查点和成交
2. 1m：cp103 之后的 K 线全部删除，cp103 那一根恢复成存下来的样子
3. 5m / 15m / 1h / 4h / 1d：受影响的那一根由 1m K 线重新合并，之后的删除
```

这样回滚后的 K 线与“从没见过被回滚的成交”完全一致，与内存里还保留着多少笔成交无关。每个池子保留最近 256 个有成交的区块的检查点，远大于索引器的回滚窗口（默认 24 个区块）；回滚得比这还深时，清空这个池子的 K 线，用保留的成交重建。随后索引器从分叉点重新索引，新链上的成交按正常流程再次进入聚合器。

**内存上限**：每个池子每个周期最多保留 1500 根 K 线、1000 笔成交，最多跟踪 1024 个池子。

查询接口：`GET /api/v1/klines?pool=&interval=&limit=`、`GET /api/v1/trades?pool=&limit=`；每笔成交和更新后的 1m K 线同时经 SSE 推送（第 26 节）。链上历史的持久化索引由子图承担（第 13 节）。

Go 基准：写入一笔成交并更新六个周期的 K 线约 1.0 µs（单池）；查询最近 100 根 1m K 线约 16.9 µs（见[压测](benchmark.md#压测)）。

**验证**：`kline_test.go` 覆盖多周期聚合、成交量精确（`TestVolumeIsExact`）、Reorg 重算、回滚范围超出成交保留条数时仍然精确（`TestReorgExactBeyondTradeRetention`）、回滚深度超过检查点时重建（`TestReorgDeeperThanCheckpoints`）、内存上限、并发访问。

**代码位置**：`backend/kline/kline.go`, `backend/kline/kline_test.go`

---

### 24. 三明治攻击检测与交易前保护

**三明治攻击**：攻击者看到受害者的大额交易后，抢在前面同向买入推高价格，等受害者以更差的价格成交，再反向卖出获利。受害者能被夹多狠，取决于他愿意接受多差的成交价（`min_amount_out` 设得多松）。

```
同一区块、同一池子内，按 logIndex 排序：

  攻击者  买入 token1  ──→ 价格被推高        （抢跑）
  受害者  买入 token1  ──→ 以更差的价格成交   （同向）
  攻击者  卖出 token1  ──→ 价格回落，差价入袋 （反向平仓）
```

**检测**

`mev.Guard` 订阅 Swap 事件流，按“当前区块 × 池子”维护区块内的 swap 序列。配对条件：

- 抢跑交易与平仓交易来自同一交易方（`sender` 和 `recipient` 都相同）、方向相反
- 两者之间至少有一笔别人的、与抢跑同向的交易（受害者）

攻击者按 `(sender, recipient)` 一起识别：经路由合约下单时 `sender` 都是路由合约，要靠 `recipient` 才能区分是谁。

**怎样不做 O(n²) 的向前扫描**

每来一笔新的 swap B（交易方 X、方向 d），把它当作“反向平仓”，要回答两个问题。朴素做法是向前逐个配对，同一个地址在一个区块里来回买卖时每笔都要扫完整个区块。这里用两个索引把每笔的成本压到 O(log n)：

| 问题 | 索引 | 成本 |
|------|------|------|
| B 之前最近的一笔“别人的、方向与 B 相反”的 swap 在哪？（潜在受害者 v） | `latest`：每个方向记住最近一笔 swap 的下标和交易方，以及最近一笔“交易方不同”的 swap 的下标 | O(1) |
| X 自己在 v 之前最近的一笔反方向 swap 在哪？（抢跑交易 f） | `byActor`：每个交易方、每个方向一个升序的下标列表 | 二分查找 |

```go
// mev/mev.go — poolBlock.match
// 最近一笔别人的反方向 swap
last := pb.latest[opposite]
victim := last.index
if last.by == who {
    victim = last.otherIndex
}
if victim < 0 {
    return nil
}

// 自己在它之前的反方向 swap，从最近的开始找
fronts := pb.byActor[who][opposite]
for i := sort.SearchInts(fronts, victim) - 1; i >= 0; i-- {
    front := pb.swaps[fronts[i]]
    // ... 收集 front 之后、方向相同、不是自己的交易作为受害者；有受害者即命中
}
```

只有命中时才扫描 f 和 B 之间的 swap 来收集受害者，命中后这一组交易结案、清空索引。整个区块 O(n log n)，不需要给单个区块内的 swap 数设上限。事件正常按 `logIndex` 顺序到达时增量检测；乱序到达时按 `logIndex` 排好后整段重放。

Go 基准里的最不利输入（同一个地址在一个区块里来回买卖 256 笔、没有别人的交易）：单笔处理 49 ns，且不随区块内 swap 数增长；改用索引之前是 16,698 ns（见[压测](benchmark.md#压测)）。

命中后记录告警（攻击者、抢跑交易、平仓交易、受害交易列表）、计入 `mev_sandwich_detected_total`、经 SSE 推送；Reorg 时丢弃分叉点之后的告警。

**交易前保护**

| 检查 | 规则 | 默认值 |
|------|------|--------|
| 价格冲击 | `impact = 1 - 报价产出 / 按现价（扣手续费）应得产出`，过大则拒绝 | > 5% 拒绝 |
| deadline | 已过期，或距当前时间过长（长 deadline 给了攻击者择时的空间） | 最长 30 分钟 |
| `min_amount_out` | 低于 `报价 × (1 - 最大滑点)` 视为过松，拒绝 | 最大滑点 50% |
| 推荐滑点 | 用户未指定时用默认值；路径上有池子近期被夹过时收紧并提示 | 默认 0.5%，10 分钟内被夹过收紧到 0.1% |

这些检查在 `POST /api/v1/swap/build` 和 `POST /api/v1/swap` 里执行，报价接口返回 `price_impact`、推荐的 `slippage` 和 `min_amount_out`。告警列表通过 `GET /api/v1/mev/alerts` 查询。

**代码位置**：`backend/mev/mev.go`, `backend/mev/mev_test.go`

---

### 25. 套利机会扫描（三分搜索最优借入量）

**问题**：同一个币对可以有多个费率档的池子，它们的价格由各自的成交决定，不会自动对齐。出现价差时可以用第 11 节的 `FlashArbitrage` 合约零本金套利。但**借多少**？

借得越多，卖出池的价格被自己压得越低、买入池的价格被自己抬得越高，边际利润递减，借过头反而亏：

```
利润
 │        ╭───╮
 │      ╭─╯   ╰─╮
 │    ╭─╯       ╰──╮
 │  ╭─╯            ╰───╮
 └──┴───────────────────┴──→ 借入数量
    1       最优         上界（两池价格被搬平）
```

利润对借入数量是**单峰**的，用三分搜索找峰值：每次把区间三等分，比较两个分点的利润，丢掉利润较低一侧的三分之一。

```go
// arb/arb.go — OptimalAmount
// 搜索上界：把卖出池的价格一路压到买入池现价需要卖多少。超过这个量两池价格已经倒挂，一定更亏
toParity, err := router.SimulateSwap(sell, zeroForOne, huge, buy.SqrtPriceX96)

three := big.NewInt(3)
for new(big.Int).Sub(hi, lo).Cmp(three) > 0 {
    third := new(big.Int).Sub(hi, lo)
    third.Div(third, three)
    m1 := new(big.Int).Add(lo, third)
    m2 := new(big.Int).Sub(hi, third)
    if less(profit(m1), profit(m2)) {
        lo = m1
    } else {
        hi = m2
    }
}
// 区间缩到 3 以内后逐个比较，取利润最高的
```

**利润函数直接用第 17 节的逐 tick 模拟**，包含两池的手续费、tick 穿越和取整，闪电贷手续费按链上规则向上取整，所以链下算出的利润就是链上执行能拿到的利润：

```go
func Profit(sell, buy indexer.PoolSnapshot, borrow string, lenderFee uint32, amount *big.Int) *big.Int {
    otherAmount, _, err := router.QuoteExactInputSingle(sell, borrow, amount)   // 卖出借入的 token
    // ...
    back, _, err := router.QuoteExactInputSingle(buy, other, otherAmount)        // 再买回来
    // ...
    flashFee := umath.MulDivRoundingUp(amount, big.NewInt(int64(lenderFee)), big.NewInt(1e6))
    profit := new(big.Int).Sub(back, amount)
    return profit.Sub(profit, flashFee)
}
```

**扫描流程**（每笔 Swap 触发，只比较成交的那个池子和同币对的其它池子）：

1. **定方向**：价格高的池子里 token0 更贵。借 token0 就在高价池卖、低价池买回；借 token1 反过来
2. **列出候选放贷池**：含有借入 token、既不是卖出池也不是买入池的池子（必须是第三个池子的原因见第 11 节），按活跃流动性从大到小依次尝试。闪电贷费率不同，最优借入量和利润也不同
3. **快速排除**：借入量趋近于 0 时的边际利润都不为正（`价格比 × (1-卖出池费率) × (1-买入池费率) <= 1 + 闪电贷费率`），不必搜索
4. **三分搜索**最优借入量和毛利润，然后**读一次放贷池的链上余额**（ERC20 `balanceOf`）：闪电贷借的是池子合约实际持有的 token，这个数不在事件里；不够借就换下一个候选
5. **扣 gas**：用第 22 节的 `CostWei` 算出一笔套利的 gas 成本，按 WETH 直连池的现价折算成借入 token；净利润不为正则放弃
6. **编码**：生成 `FlashArbitrage.arbitrage` 的调用数据，`minProfit` 设为 gas 成本——链上实际利润达不到就整笔 revert

机会通过 `GET /api/v1/arbitrage/opportunities` 查询（净利润高的在前）并经 SSE 推送；池子再次成交后重新评估，无利可图时移除，Reorg 时全部作废。本模块只产出机会和调用数据，不代签、不自动发交易。

集成测试里的套利场景：在真实节点上制造价差 → 扫描器给出机会 → 用它的调用数据原样发起交易 → 发起人账户实际增加的余额与链下预测的毛利润逐位相等，执行后同一机会的残余利润不到原来的 1%。

> 使用说明：`WETH_ADDRESS` 用于把 gas 成本折算成借入 token，未配置时响应里 `gas_priced=false`；`FLASH_ARB_ADDRESS` 用于填充响应里的 `to`。

**代码位置**：`backend/arb/arb.go`, `backend/arb/arb_test.go`

---

### 26. SSE 实时推送

**为什么需要实时推送？**

| 方式 | 延迟 | 带宽 | 说明 |
|------|------|------|------|
| HTTP 轮询 | 轮询间隔/2 | 高（大量无效请求） | 100 用户 × 每秒 1 次 = 100 QPS 无效负载 |
| SSE 推送 | 毫秒级 | 低（仅推送变化） | 单向，服务端→客户端 |
| WebSocket | 毫秒级 | 低 | 双向，更复杂 |

本实现使用 SSE（Server-Sent Events），因为价格推送是单向的（服务端→客户端），SSE 更简单且浏览器原生支持。

**推送的事件类型**：

| 事件 | 触发时机 | 内容 |
|------|---------|------|
| `trade` | 每笔 Swap | 成交记录（池子、价格、数量、方向、交易哈希） |
| `kline` | 每笔 Swap | 更新后的 1m K 线 |
| `mev_alert` | 检测到三明治 | 告警详情 |
| `arbitrage` | 发现套利机会 | 机会与调用数据 |

**Hub 发布-订阅模式**：

```
  Indexer（Swap 事件，索引协程内同步回调）
      │
      ├─→ kline.OnTrade ──────→ hub.Broadcast("trade" / "kline")
      ├─→ mev.OnAlert ────────→ hub.Broadcast("mev_alert")
      └─→ arb.OnOpportunity ──→ hub.Broadcast("arbitrage")
                                        │ 非阻塞扇出
              ┌─────────────────────────┼─────────────────────────┐
         ┌────▼─────┐              ┌────▼─────┐              ┌────▼─────┐
         │ Client A │              │ Client B │              │ Client C │
         │ 队列 256  │              │ 队列 256  │              │ 队列 256  │
         └──────────┘              └──────────┘              └──────────┘
```

**慢客户端不能拖住索引器**

`Broadcast` 是在索引协程里被同步调用的，它一旦阻塞，整个索引就停了。所以扇出时对每个客户端的有界队列做**非阻塞写**，队列满了说明客户端消费不过来，直接断开它：

```go
// ws/ws.go
func (h *Hub) Broadcast(kind string, payload interface{}) {
    data, err := json.Marshal(payload)
    // ...
    h.mu.Lock()
    defer h.mu.Unlock()
    for c := range h.clients {
        if c.kinds != nil && !c.kinds[kind] {
            continue                      // 客户端没订阅这类事件
        }
        select {
        case c.send <- msg:
        default:
            h.drop(c)                     // 队列已满：断开慢客户端
            metrics.StreamDroppedClients.Inc()
        }
    }
}
```

被断开的客户端重连后从最新数据继续，而不是让它的积压反过来卡住所有人。同时在线的客户端上限是 1024，超过时新的订阅返回 503。

压测结果：500 个订阅端同时在线时，一条消息从最早到最晚收到的订阅端相差（扇出）中位 1.71 ms；200 个正常订阅端加 5 个只连不读的慢客户端，短时间灌入 6,000 笔成交，1,200,000 条推送全部送达，5 个慢客户端被断开，正常订阅端无一受影响（见[压测](benchmark.md#压测)）。

**SSE 协议细节**：

```go
// SSEHandler — 每条消息带事件类型
fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.kind, msg.data)
```

- `?types=trade,kline` 只订阅指定类型，不传则订阅全部
- 每 15 秒发一行 `: heartbeat` 注释保活
- 长连接用 `http.ResponseController` 取消服务器的写超时；日志中间件包装的 `ResponseWriter` 透传 `Flush`，否则流式响应发不出去
- 服务关闭时 Hub 主动断开所有客户端，否则长连接会拖住 HTTP 服务器的优雅关闭

**客户端使用**：

```javascript
// 浏览器原生 EventSource，自动重连
const es = new EventSource("/api/v1/stream?types=trade,kline");
es.addEventListener("trade", (event) => {
    const trade = JSON.parse(event.data);
    console.log(`${trade.pool}: ${trade.price}`);
});
es.addEventListener("kline", (event) => {
    const { pool, interval, candle } = JSON.parse(event.data);
});
```

**代码位置**：`backend/ws/ws.go`, `backend/ws/ws_test.go`

