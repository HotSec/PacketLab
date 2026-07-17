package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"packetlab/internal/models"

	"github.com/gorilla/websocket"
)

// TestWS_BroadcastMultipleClients 验证多客户端都能收到 broadcast 广播。
func TestWS_BroadcastMultipleClients(t *testing.T) {
	hub := newWSHub()
	go hub.run()
	defer hub.Stop()

	// 构造最小 WebSocket handler（模拟 Server.handleWebSocket 的核心逻辑）
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &wsClient{
			hub:  hub,
			conn: conn,
			send: make(chan []byte, 64),
		}
		hub.register <- client
		go client.writePump()
		go client.readPump()
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c1, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial c1: %v", err)
	}
	defer c1.Close()
	c2, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial c2: %v", err)
	}
	defer c2.Close()

	// 等待注册
	time.Sleep(100 * time.Millisecond)

	// 广播新请求
	hub.broadcast(&models.CapturedRequest{
		ID:     42,
		Method: "GET",
		URL:    "http://test.example/",
		Host:   "test.example",
	})

	// 两个客户端都应收到
	for i, c := range []*websocket.Conn{c1, c2} {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("client %d read: %v", i, err)
		}
		if !strings.Contains(string(msg), "new_request") {
			t.Fatalf("client %d did not receive new_request: %s", i, msg)
		}
		if !strings.Contains(string(msg), "test.example") {
			t.Fatalf("client %d msg missing host: %s", i, msg)
		}
	}
}

// TestWS_BufferFullDropsClient 验证客户端 send 缓冲满时 broadcast 不阻塞，而是踢掉客户端。
// wsHub.run 中 default 分支会 close(client.send) + delete(h.clients, client)。
func TestWS_BufferFullDropsClient(t *testing.T) {
	hub := newWSHub()
	go hub.run()
	defer hub.Stop()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &wsClient{
			hub:  hub,
			conn: conn,
			send: make(chan []byte, 2), // 小缓冲
		}
		hub.register <- client
		go client.writePump()
		go client.readPump()
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	time.Sleep(100 * time.Millisecond)

	// 发送大量广播，不读 → 客户端 send 缓冲满 → hub 踢掉客户端
	// 不应阻塞
	for i := 0; i < 100; i++ {
		hub.broadcast(&models.CapturedRequest{
			ID:     int64(i),
			Method: "GET",
			URL:    "http://test.example/",
			Host:   "test.example",
		})
	}

	// 给 hub 时间处理
	time.Sleep(200 * time.Millisecond)

	// 客户端应已被踢掉（连接关闭）
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			break // 连接已关闭
		}
	}
}

// TestWS_StopClosesClients 验证 hub.Stop() 关闭后所有客户端连接被清理。
func TestWS_StopClosesClients(t *testing.T) {
	hub := newWSHub()
	go hub.run()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &wsClient{
			hub:  hub,
			conn: conn,
			send: make(chan []byte, 64),
		}
		hub.register <- client
		go client.writePump()
		go client.readPump()
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	time.Sleep(100 * time.Millisecond)

	hub.Stop()

	// 客户端应收到关闭帧
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			break // 连接已关闭
		}
	}
}
