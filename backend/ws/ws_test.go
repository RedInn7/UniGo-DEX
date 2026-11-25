package ws

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readEvent 从 SSE 流里读一条事件，返回 (event, data)
func readEvent(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	var event, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && event != "":
			return event, data
		}
	}
}

func waitClients(t *testing.T, h *Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.ClientCount() != n {
		if time.Now().After(deadline) {
			t.Fatalf("client count = %d, want %d", h.ClientCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSSEDeliversAndFilters(t *testing.T) {
	h := NewHub()
	srv := httptest.NewServer(h.SSEHandler())
	defer srv.Close()

	open := func(query string) (*bufio.Reader, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+query, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("content-type = %q", ct)
		}
		return bufio.NewReader(resp.Body), func() { cancel(); resp.Body.Close() }
	}

	all, closeAll := open("")
	tradesOnly, closeTrades := open("?types=trade")
	waitClients(t, h, 2)

	h.Broadcast("kline", map[string]int{"close": 7})
	h.Broadcast("trade", map[string]string{"tx": "0xabc"})

	if ev, data := readEvent(t, all); ev != "kline" || data != `{"close":7}` {
		t.Fatalf("all[0] = %s %s", ev, data)
	}
	if ev, data := readEvent(t, all); ev != "trade" || data != `{"tx":"0xabc"}` {
		t.Fatalf("all[1] = %s %s", ev, data)
	}
	// 只订阅 trade 的客户端收不到 kline
	if ev, _ := readEvent(t, tradesOnly); ev != "trade" {
		t.Fatalf("filtered client got %s", ev)
	}

	// 客户端断开后被注销
	closeAll()
	closeTrades()
	waitClients(t, h, 0)
}

func TestSlowClientIsDroppedWithoutBlocking(t *testing.T) {
	h := NewHub()
	slow := h.subscribe(nil) // 从不读取
	fast := h.subscribe(nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < clientQueueSize+10; i++ {
			h.Broadcast("trade", i)
			<-fast.send // 快客户端及时消费
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Broadcast blocked on a slow client")
	}

	if h.ClientCount() != 1 {
		t.Fatalf("client count = %d, want 1 (slow client dropped)", h.ClientCount())
	}
	// 慢客户端的队列被关闭：读完积压后得到关闭信号
	drained := 0
	for range slow.send {
		drained++
	}
	if drained != clientQueueSize {
		t.Fatalf("slow client had %d queued messages, want %d", drained, clientQueueSize)
	}
}

func TestRunClosesClientsOnShutdown(t *testing.T) {
	h := NewHub()
	c := h.subscribe(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	cancel()
	<-done

	if _, ok := <-c.send; ok {
		t.Fatal("client channel should be closed")
	}
	if h.subscribe(nil) != nil {
		t.Fatal("closed hub should refuse new subscribers")
	}
	h.Broadcast("trade", 1) // 关闭后广播不 panic
	h.unsubscribe(c)        // 重复注销不 panic
}
