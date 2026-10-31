package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"filet/internal/protocol"
)

type WS struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func DialWS(ctx context.Context, server, token, deviceID, name, platform string) (*WS, error) {
	base := NormalizeServer(server)
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("服务端地址无效: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("服务端地址需为 http(s)://")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/ws"
	q := u.Query()
	if deviceID != "" {
		q.Set("device_id", deviceID)
	}
	if name != "" {
		q.Set("name", name)
	}
	if platform != "" {
		q.Set("platform", platform)
	}
	u.RawQuery = q.Encode()

	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second
	conn, resp, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("连接信令服务失败 (HTTP %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("连接信令服务失败: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})
	return &WS{conn: conn}, nil
}

func (w *WS) Read() (protocol.Envelope, error) {
	_, raw, err := w.conn.ReadMessage()
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return protocol.Envelope{}, err
	}
	return env, nil
}

func (w *WS) SendSignal(ctx context.Context, to string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	env, err := json.Marshal(protocol.Envelope{Type: protocol.TypeSignal, To: to, Data: raw})
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = w.conn.SetWriteDeadline(deadline)
	return w.conn.WriteMessage(websocket.TextMessage, env)
}

func (w *WS) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.Close()
}
