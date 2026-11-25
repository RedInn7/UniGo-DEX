package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"unigo-dex/metrics"
)

// ws.go — 实时行情推送（成交 / K 线 / MEV 告警 / 套利机会）
//
// === 为什么要推送？ ===
//
// HTTP 轮询的问题：
//   - 客户端每秒轮询一次 → 大量无效请求（数据可能没变）
//   - 100 个用户 × 每秒 1 次 = 100 QPS 的无效负载
//   - 延迟 = 轮询间隔 / 2（平均），做不到实时
//
// 服务端推送：
//   - 只在有新数据时推送 → 没有无效流量
//   - 延迟 = 事件传播时间（毫秒级）
//   - 每个客户端一条长连接
//
// === 为什么用 SSE 而不是 WebSocket？ ===
//
// 行情是单向的（服务端 → 客户端），用不上 WebSocket 的双向通道。
// SSE（Server-Sent Events）就是一个不结束的 HTTP 响应：标准库即可实现，
// 浏览器用 EventSource 订阅并自带断线重连，也能直接穿过只认 HTTP 的代理和网关。
//
// === 架构 ===
//
//   Indexer(Swap事件) → K 线 / MEV / 套利模块 → Hub.Broadcast() → 扇出到所有订阅者
//
//   ┌──────────┐    ┌──────────┐
//   │ Client A │    │ Client B │
//   │ (SSE连接) │    │ (SSE连接) │
//   └────▲─────┘    └────▲─────┘
//        │               │
//   ┌────┴───────────────┴───────┐
//   │         Hub                │
//   │  clients（每个带有界队列）   │
//   └────────────▲───────────────┘
//                │ Broadcast(类型, 数据)
//   ┌────────────┴───────────────┐
//   │   订阅 Swap 事件流的各模块    │
//   └────────────────────────────┘
//
// === 慢客户端不能拖住索引器 ===
//
// Broadcast 在索引协程里被同步调用，所以它绝不阻塞：
//   - 每个客户端有一个有界发送队列，扇出时用非阻塞写
//   - 队列满了说明客户端消费不过来，直接断开它（客户端重连后从最新数据继续），
//     而不是让它的积压反过来卡住所有人

// 每个客户端的发送队列长度，以及同时在线的客户端上限
const (
	clientQueueSize = 256
	maxClients      = 1024
)

// message 一条待推送的消息
type message struct {
	kind string // 消息类型：trade / kline / mev_alert / arbitrage
	data []byte // JSON
}

// client 一个订阅者
type client struct {
	send  chan message
	kinds map[string]bool // 只订阅这些类型；nil 表示全部
}

// Hub 管理所有订阅者并扇出消息
type Hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
}

// NewHub 创建 Hub
func NewHub() *Hub {
	return &Hub{clients: make(map[*client]struct{})}
}

// Run 阻塞到 ctx 结束，然后断开所有客户端并拒绝新的订阅
func (h *Hub) Run(ctx context.Context) {
	<-ctx.Done()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		h.drop(c)
	}
}

// Broadcast 向所有订阅了 kind 的客户端推送一条消息，永不阻塞
func (h *Hub) Broadcast(kind string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("推送消息序列化失败", "kind", kind, "err", err)
		return
	}
	msg := message{kind: kind, data: data}

	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.kinds != nil && !c.kinds[kind] {
			continue
		}
		select {
		case c.send <- msg:
		default:
			// 队列已满：断开慢客户端
			h.drop(c)
			metrics.StreamDroppedClients.Inc()
			slog.Warn("推送队列已满，断开慢客户端")
		}
	}
}

// drop 断开一个客户端（调用方持有锁）
func (h *Hub) drop(c *client) {
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
		metrics.StreamClients.Set(int64(len(h.clients)))
	}
}

// subscribe 注册一个订阅者；Hub 已关闭或客户端数达到上限时返回 nil
func (h *Hub) subscribe(kinds map[string]bool) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.clients) >= maxClients {
		return nil
	}
	c := &client{send: make(chan message, clientQueueSize), kinds: kinds}
	h.clients[c] = struct{}{}
	metrics.StreamClients.Set(int64(len(h.clients)))
	return c
}

func (h *Hub) unsubscribe(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drop(c)
}

// ClientCount 当前客户端数量
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// SSEHandler 返回 SSE 订阅端点
//
// 可选参数 types 用逗号分隔要订阅的消息类型，如 ?types=trade,kline；不传则订阅全部。
// 每条消息的格式：
//
//	event: trade
//	data: {...}
func (h *Hub) SSEHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		var kinds map[string]bool
		if v := r.URL.Query().Get("types"); v != "" {
			kinds = make(map[string]bool)
			for _, kind := range strings.Split(v, ",") {
				kinds[strings.TrimSpace(kind)] = true
			}
		}
		c := h.subscribe(kinds)
		if c == nil {
			http.Error(w, "too many stream clients", http.StatusServiceUnavailable)
			return
		}
		defer h.unsubscribe(c)

		// 长连接不受服务器写超时限制
		http.NewResponseController(w).SetWriteDeadline(time.Time{})

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// 心跳保活（每 15 秒）
		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()

		for {
			select {
			case <-r.Context().Done():
				return

			case msg, ok := <-c.send:
				if !ok {
					return // 被 Hub 断开
				}
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.kind, msg.data); err != nil {
					return
				}
				flusher.Flush()

			case <-heartbeat.C:
				if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
}
