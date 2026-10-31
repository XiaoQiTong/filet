package server

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"filet/internal/protocol"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 90 * time.Second
	pingPeriod = 30 * time.Second
)

type client struct {
	hub       *hub
	username  string
	peerID    string
	deviceID  string
	name      string
	platform  string
	ephemeral bool
	conn      *websocket.Conn
	send      chan []byte
	done      chan struct{}
	once      sync.Once
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *client) enqueue(msg []byte) {
	select {
	case <-c.done:
	case c.send <- msg:
	default:
		c.close()
	}
}

func (c *client) sendEnvelope(env protocol.Envelope) {
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	c.enqueue(raw)
}

func (c *client) sendError(msg string) {
	c.sendEnvelope(protocol.Envelope{Type: protocol.TypeError, Error: msg})
}

type hub struct {
	mu      sync.RWMutex
	clients map[string]*client
}

func newHub() *hub {
	return &hub{clients: map[string]*client{}}
}

func (h *hub) add(c *client) {
	h.mu.Lock()
	old := h.clients[c.peerID]
	h.clients[c.peerID] = c
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	if h.clients[c.peerID] == c {
		delete(h.clients, c.peerID)
	}
	h.mu.Unlock()
}

func (h *hub) get(peerID string) (*client, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.clients[peerID]
	return c, ok
}

func (h *hub) peers(username string) []*client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*client, 0, len(h.clients))
	for _, c := range h.clients {
		if c.username == username {
			out = append(out, c)
		}
	}
	return out
}

func (h *hub) onlineDeviceIDs(username string) map[string]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := map[string]bool{}
	for _, c := range h.clients {
		if c.username == username && c.deviceID != "" {
			out[c.deviceID] = true
		}
	}
	return out
}

// ephemeralDevices 返回只存在于当前会话的接收端（网页接收模式），不落库。
func (h *hub) ephemeralDevices(username string) []protocol.Device {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]protocol.Device, 0, len(h.clients))
	for _, c := range h.clients {
		if c.username != username || !c.ephemeral || c.deviceID == "" {
			continue
		}
		out = append(out, protocol.Device{
			ID:       c.deviceID,
			Name:     c.name,
			Platform: c.platform,
			Online:   true,
		})
	}
	return out
}
