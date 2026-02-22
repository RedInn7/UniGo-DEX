package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ws.go — WebSocket 实时价格推送
//
// === 为什么需要 WebSocket？ ===
//
// HTTP 轮询的问题：
//   - 客户端每秒轮询 /api/v1/quote → 大量无效请求（价格可能没变）
//   - 100 个用户 × 每秒 1 次 = 100 QPS 的无效负载
//   - 延迟 = 轮询间隔 / 2（平均），无法实时
//
// WebSocket 推送的优势：
//   - 服务端只在价格变动时推送 → 零无效流量
//   - 延迟 = 事件传播时间（毫秒级）
//   - 单个 TCP 连接，无 HTTP 握手开销
//
// === 架构 ===
//
//   Indexer(Swap事件) → Hub.Broadcast() → 扇出到所有订阅者
//
//   ┌──────────┐    ┌──────────┐
//   │ Client A │    │ Client B │
//   │ (ws连接)  │    │ (ws连接)  │
//   └────┬─────┘    └────┬─────┘
//        │               │
//        │  subscribe    │  subscribe
//        ↓               ↓
//   ┌────────────────────────────┐
//   │         Hub               │
//   │  clients map[*Client]bool │
//   │  broadcast chan []byte    │
//   └────────────┬──────────────┘
//                │ 价格变动事件
//   ┌────────────▼──────────────┐
//   │     Event Indexer         │
//   └───────────────────────────┘
//
// === 注意事项 ===
//
// 本实现使用标准库 HTTP 长轮询模拟 WebSocket 行为（SSE 风格），
// 生产环境建议使用 gorilla/websocket 或 nhooyr.io/websocket。
// 核心的 Hub 发布-订阅模式是相同的。

// PriceUpdate 价格更新消息
type PriceUpdate struct {
	Pool         string  `json:"pool"`
	Token0       string  `json:"token0"`
	Token1       string  `json:"token1"`
	Price        float64 `json:"price"`
	Tick         int32   `json:"tick"`
	Liquidity    string  `json:"liquidity"`
	Timestamp    int64   `json:"timestamp"`
	BlockNumber  uint64  `json:"block_number"`
}

// Client 代表一个 WebSocket 客户端
type Client struct {
	// send 消息发送通道（有界，防止慢客户端阻塞 Hub）
	send chan []byte

	// subscriptions 订阅的交易对
	subscriptions map[string]bool

	mu sync.RWMutex
}

// Hub 管理所有 WebSocket 连接的中心
//
// Hub 是经典的发布-订阅模式实现：
//   - 维护活跃客户端集合
//   - 接收广播消息，扇出到所有订阅了相关交易对的客户端
//   - 处理客户端的连接和断开
type Hub struct {
	// clients 所有活跃客户端
	clients map[*Client]bool

	// broadcast 广播通道
	broadcast chan []byte

	// register 客户端注册通道
	register chan *Client

	// unregister 客户端注销通道
	unregister chan *Client

	mu sync.RWMutex
}

// NewHub 创建 Hub
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// Run 启动 Hub 事件循环
//
// 单一协程处理所有注册/注销/广播，避免并发竞争
// 这是 Go 并发编程中经典的 "select loop" 模式
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// 关闭所有客户端
			h.mu.Lock()
			for client := range h.clients {
				close(client.send)
				delete(h.clients, client)
			}
			h.mu.Unlock()
			return

		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			slog.Info("WebSocket 客户端连接",
				"total_clients", len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				close(client.send)
				delete(h.clients, client)
			}
			h.mu.Unlock()
			slog.Info("WebSocket 客户端断开",
				"total_clients", len(h.clients))

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
					// 发送成功
				default:
					// 客户端 send buffer 满了（慢客户端）
					// 关闭连接，避免 Hub 被阻塞
					close(client.send)
					delete(h.clients, client)
					slog.Warn("关闭慢速 WebSocket 客户端")
				}
			}
			h.mu.RUnlock()
		}
	}
}

// BroadcastPriceUpdate 广播价格更新
//
// 由 Indexer 在处理 Swap 事件后调用
func (h *Hub) BroadcastPriceUpdate(update *PriceUpdate) {
	data, err := json.Marshal(update)
	if err != nil {
		return
	}

	select {
	case h.broadcast <- data:
	default:
		// broadcast channel 满了，丢弃（不阻塞 Indexer）
		slog.Warn("WebSocket 广播通道已满，丢弃消息")
	}
}

// ClientCount 当前客户端数量
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// SSEHandler 基于 Server-Sent Events (SSE) 的实时推送
//
// SSE 是 WebSocket 的轻量替代：
//   - 单向：服务端 → 客户端（足够用于价格推送）
//   - 基于 HTTP：无需协议升级，穿透性更好
//   - 自动重连：浏览器原生支持断线重连
//   - 标准库即可实现，无外部依赖
//
// 客户端使用：
//   const es = new EventSource("/ws/prices");
//   es.onmessage = (e) => { console.log(JSON.parse(e.data)); };
func (h *Hub) SSEHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 检查是否支持 Flusher（SSE 必需）
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		// 设置 SSE 响应头
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// 创建客户端
		client := &Client{
			send:          make(chan []byte, 64),
			subscriptions: make(map[string]bool),
		}

		// 注册到 Hub
		h.register <- client

		// 请求结束时注销
		defer func() { h.unregister <- client }()

		// 心跳：每 15 秒发送空注释，保持连接活跃
		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()

		for {
			select {
			case <-r.Context().Done():
				return

			case msg, ok := <-client.send:
				if !ok {
					return
				}
				// SSE 格式：data: {json}\n\n
				w.Write([]byte("data: "))
				w.Write(msg)
				w.Write([]byte("\n\n"))
				flusher.Flush()

			case <-heartbeat.C:
				// SSE 心跳（注释行）
				w.Write([]byte(": heartbeat\n\n"))
				flusher.Flush()
			}
		}
	})
}
