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

	// 令牌有效期 30 天，续期节奏取 1/2 天，留足断网重试余量。
	tokenRenewPeriod = 12 * time.Hour
)

type client struct {
	hub        *hub
	srv        *Server
	username   string
	peerID     string
	deviceID   string
	name       string
	platform   string
	ephemeral  bool
	observedIP string
	publicAddr string
	location   string
	geoIP      string
	conn       *websocket.Conn
	send       chan []byte
	done       chan struct{}
	once       sync.Once
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

type deviceMeta struct {
	addr     string
	location string
}

// deviceMeta 汇总在线设备的外网地址与归属地，key 为设备 ID。
func (h *hub) deviceMeta(username string) map[string]deviceMeta {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := map[string]deviceMeta{}
	for _, c := range h.clients {
		if c.username != username || c.deviceID == "" {
			continue
		}
		addr := c.publicAddr
		if addr == "" {
			addr = c.observedIP
		}
		if addr == "" && c.location == "" {
			continue
		}
		out[c.deviceID] = deviceMeta{addr: addr, location: c.location}
	}
	return out
}

func (h *hub) setPublicAddr(c *client, addr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.publicAddr == addr {
		return false
	}
	c.publicAddr = addr
	return true
}

func (h *hub) clientAddr(c *client) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if c.publicAddr != "" {
		return c.publicAddr
	}
	return c.observedIP
}

// claimGeo 保证同一连接对同一 IP 只发起一次归属地查询。
func (h *hub) claimGeo(c *client, ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.geoIP == ip {
		return false
	}
	c.geoIP = ip
	return true
}

func (h *hub) setLocation(c *client, ip, location string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	current := c.publicAddr
	if current == "" {
		current = c.observedIP
	}
	if addrIP(current) != ip || c.location == location {
		return false
	}
	c.location = location
	return true
}
