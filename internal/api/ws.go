package api

import (
	"encoding/json"
	"log"
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
	register    chan *wsClient
	unregister  chan *wsClient
}

func newWSHub() *wsHub {
	return &wsHub{
		clients:     make(map[*wsClient]bool),
		broadcastCh: make(chan *models.CapturedRequest, 256),
		register:    make(chan *wsClient),
		unregister:  make(chan *wsClient),
	}
}

func (h *wsHub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("[ws] client connected (%d total)", len(h.clients))

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			log.Printf("[ws] client disconnected (%d total)", len(h.clients))

		case req := <-h.broadcastCh:
			// 序列化为简略列表项
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
		}
	}
}

func (c *wsClient) readPump() {
	defer func() {
		c.hub.unregister <- c
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
				log.Printf("[ws] read error: %v", err)
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
		// 通道满则丢弃
	}
}
