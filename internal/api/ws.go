package api

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"packetlab/internal/models"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4096
)

// wsClient WebSocket 客户端
type wsClient struct {
	hub  *wsHub
	conn *websocket.Conn
	send chan []byte
}

// wsHub WebSocket 中心
type wsHub struct {
	clients     map[*wsClient]bool
	broadcastCh chan *models.CapturedRequest
	interceptCh chan *models.PendingRequest
	updateCh    chan *models.CapturedRequest // SSE 等流式响应的增量更新
	register    chan *wsClient
	unregister  chan *wsClient
	stopCh      chan struct{}
	stopOnce    sync.Once
}

func newWSHub() *wsHub {
	return &wsHub{
		clients:     make(map[*wsClient]bool),
		broadcastCh: make(chan *models.CapturedRequest, 256),
		interceptCh: make(chan *models.PendingRequest, 64),
		updateCh:    make(chan *models.CapturedRequest, 256),
		register:    make(chan *wsClient),
		unregister:  make(chan *wsClient),
		stopCh:      make(chan struct{}),
	}
}

func (h *wsHub) run() {
	for {
		select {
		case <-h.stopCh:
			for c := range h.clients {
				close(c.send)
			}
			return
		case client := <-h.register:
			h.clients[client] = true
			slog.Info("ws client connected", "total", len(h.clients))

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			slog.Info("ws client disconnected", "total", len(h.clients))

		case req := <-h.broadcastCh:
			msg, err := json.Marshal(map[string]interface{}{
				"type": "new_request",
				"data": models.RequestListItem{
					ID:         req.ID,
					Method:     req.Method,
					URL:        req.URL,
					Host:       req.Host,
					StatusCode: req.StatusCode,
					DurationMs: req.DurationMs,
					SizeBytes:  req.SizeBytes,
					CapturedAt: req.CapturedAt,
					IsHTTPS:    req.IsHTTPS,
				},
			})
			if err != nil {
				continue
			}

			for client := range h.clients {
				select {
				case client.send <- msg:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}

		case req := <-h.interceptCh:
			msg, err := json.Marshal(map[string]interface{}{
				"type": "intercept_request",
				"data": req,
			})
			if err != nil {
				continue
			}
			for client := range h.clients {
				select {
				case client.send <- msg:
				default:
					slog.Warn("ws intercept notification dropped, client buffer full")
				}
			}

		case req := <-h.updateCh:
			// SSE 流式响应增量推送：仅推送元数据，body 由前端按需请求 API
			msg, err := json.Marshal(map[string]interface{}{
				"type": "update_request",
				"data": map[string]interface{}{
					"id":          req.ID,
					"status_code": req.StatusCode,
					"duration_ms": req.DurationMs,
					"size_bytes":  req.SizeBytes,
				},
			})
			if err != nil {
				continue
			}
			for client := range h.clients {
				select {
				case client.send <- msg:
				default:
					slog.Warn("ws update_request dropped, client buffer full", "id", req.ID)
				}
			}
		}
	}
}

func (c *wsClient) readPump() {
	defer func() {
		select {
		case c.hub.unregister <- c:
		case <-c.hub.stopCh:
		}
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Warn("ws read error", "error", err)
			}
			break
		}
	}
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// broadcast 向所有客户端广播新请求
func (h *wsHub) broadcast(req *models.CapturedRequest) {
	select {
	case h.broadcastCh <- req:
	default:
		slog.Warn("ws broadcast channel full, dropping message", "url", req.URL)
	}
}

// broadcastIntercept 广播待审批请求
func (h *wsHub) broadcastIntercept(req *models.PendingRequest) {
	select {
	case h.interceptCh <- req:
	default:
		slog.Warn("ws intercept channel full, dropping message")
	}
}

// BroadcastUpdate 广播请求更新（SSE 等流式响应的增量推送）
func (h *wsHub) BroadcastUpdate(req *models.CapturedRequest) {
	select {
	case h.updateCh <- req:
	default:
		slog.Warn("ws update channel full, dropping message", "url", req.URL)
	}
}

func (h *wsHub) Stop() {
	h.stopOnce.Do(func() { close(h.stopCh) })
}
