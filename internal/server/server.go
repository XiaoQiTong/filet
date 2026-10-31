package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"filet/internal/auth"
	"filet/internal/protocol"
	"filet/internal/store"
	"filet/internal/stunserver"
)

//go:embed web
var webFS embed.FS

const (
	sessionCookie = "filet_session"
	maxBodyBytes  = 1 << 20
)

type Config struct {
	Addr       string
	DataDir    string
	NoRegister bool
	ICEURLs    []string
	TLSCert    string
	TLSKey     string
	STUNAddr   string
}

type Server struct {
	cfg   Config
	store *store.Store
	hub   *hub
	limit *attemptLimiter
}

func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = ":8085"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "filet-data"
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "store.json"))
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, hub: newHub(), limit: newAttemptLimiter()}, nil
}

func (s *Server) Run(ctx context.Context) error {
	if s.cfg.STUNAddr != "" {
		go func() {
			if err := stunserver.Run(ctx, s.cfg.STUNAddr, log.Printf); err != nil {
				log.Printf("STUN 服务退出: %v", err)
			}
		}()
	}
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	useTLS := s.cfg.TLSCert != "" && s.cfg.TLSKey != ""
	if useTLS {
		log.Printf("控制面监听 %s (TLS)", s.cfg.Addr)
	} else {
		log.Printf("控制面监听 %s", s.cfg.Addr)
	}
	var err error
	if useTLS {
		err = srv.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/register", s.handleRegister)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.authed(s.handleMe))
	mux.HandleFunc("GET /api/devices", s.authed(s.handleDevices))
	mux.HandleFunc("GET /api/ice", s.handleICE)
	mux.HandleFunc("GET /api/ws", s.handleWS)

	return securityHeaders(mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "web assets missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`)

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.cfg.NoRegister {
		writeErr(w, http.StatusForbidden, "本服务已关闭自助注册")
		return
	}
	ip := clientIP(r)
	if s.limit.blocked("reg:" + ip) {
		writeErr(w, http.StatusTooManyRequests, "尝试过于频繁，请稍后再试")
		return
	}
	var req credentials
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !usernameRe.MatchString(req.Username) {
		writeErr(w, http.StatusBadRequest, "用户名需为 3-32 位字母、数字、下划线、点或短横线")
		return
	}
	if len(req.Password) < 8 || len(req.Password) > 128 {
		writeErr(w, http.StatusBadRequest, "密码长度需在 8-128 位之间")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成密码哈希失败")
		return
	}
	if err := s.store.CreateUser(req.Username, hash); err != nil {
		if errors.Is(err, store.ErrExists) {
			s.limit.fail("reg:" + ip)
			writeErr(w, http.StatusConflict, "用户名已存在")
			return
		}
		log.Printf("创建用户失败: %v", err)
		writeErr(w, http.StatusInternalServerError, "创建用户失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": req.Username})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.limit.blocked("login:" + ip) {
		w.Header().Set("Retry-After", "600")
		writeErr(w, http.StatusTooManyRequests, "尝试过于频繁，请 10 分钟后再试")
		return
	}
	var req credentials
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	user, ok := s.store.User(req.Username)
	if !ok || !auth.VerifyPassword(user.PasswordHash, req.Password) {
		s.limit.fail("login:" + ip)
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	s.limit.clear("login:" + ip)
	token, err := auth.NewToken(s.store.Secret(), user.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成会话失败")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(auth.TokenTTL.Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"username": user.Username, "token": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, ok := s.username(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, username)))
	}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"username": currentUser(r)})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deviceList(currentUser(r)))
}

func (s *Server) handleICE(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]string{"urls": s.cfg.ICEURLs})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	username, ok := s.username(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
		return
	}
	q := r.URL.Query()
	deviceID := strings.TrimSpace(q.Get("device_id"))
	name := strings.TrimSpace(q.Get("name"))
	platform := strings.TrimSpace(q.Get("platform"))
	ephemeral := q.Get("ephemeral") == "1"
	if ephemeral && deviceID == "" {
		writeErr(w, http.StatusBadRequest, "网页接收模式需要 device_id")
		return
	}
	if deviceID != "" && !deviceIDRe.MatchString(deviceID) {
		writeErr(w, http.StatusBadRequest, "device_id 无效")
		return
	}
	if name == "" {
		name = "未命名设备"
	}
	if r := []rune(name); len(r) > 64 {
		name = string(r[:64])
	}
	if platform == "" {
		platform = "unknown"
	}

	conn, err := (&websocket.Upgrader{ReadBufferSize: 8192, WriteBufferSize: 8192}).Upgrade(w, r, nil)
	if err != nil {
		return
	}

	if deviceID != "" && !ephemeral {
		if err := s.store.UpsertDevice(store.Device{ID: deviceID, Username: username, Name: name, Platform: platform}); err != nil {
			log.Printf("登记设备失败: %v", err)
		}
	}
	peerID := deviceID
	if peerID == "" {
		peerID = "p-" + randomHex(8)
	}
	c := &client{
		hub:       s.hub,
		username:  username,
		peerID:    peerID,
		deviceID:  deviceID,
		name:      name,
		platform:  platform,
		ephemeral: ephemeral,
		conn:      conn,
		send:      make(chan []byte, 32),
		done:      make(chan struct{}),
	}
	s.hub.add(c)
	c.sendEnvelope(protocol.Envelope{
		Type:     protocol.TypeHello,
		PeerID:   peerID,
		Username: username,
		Devices:  s.deviceList(username),
	})
	s.broadcastDevices(username)

	go c.writeLoop()
	c.readLoop()
	s.hub.remove(c)
	c.close()
	if deviceID != "" && !ephemeral {
		if err := s.store.MarkSeen(deviceID); err != nil {
			log.Printf("更新设备时间失败: %v", err)
		}
	}
	s.broadcastDevices(username)
}

func (c *client) readLoop() {
	conn := c.conn
	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.Type != protocol.TypeSignal || env.To == "" {
			continue
		}
		target, ok := c.hub.get(env.To)
		if !ok {
			c.sendError("目标设备不在线")
			continue
		}
		if target.username != c.username {
			c.sendError("无权向该设备发送数据")
			continue
		}
		out, err := json.Marshal(protocol.Envelope{
			Type: protocol.TypeSignal,
			From: c.peerID,
			Data: env.Data,
		})
		if err != nil {
			continue
		}
		target.enqueue(out)
	}
}

func (c *client) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (s *Server) broadcastDevices(username string) {
	env := protocol.Envelope{Type: protocol.TypeDevices, Devices: s.deviceList(username)}
	for _, c := range s.hub.peers(username) {
		c.sendEnvelope(env)
	}
}

func (s *Server) deviceList(username string) []protocol.Device {
	online := s.hub.onlineDeviceIDs(username)
	devs := s.store.Devices(username)
	out := make([]protocol.Device, 0, len(devs)+4)
	seen := make(map[string]bool, len(devs))
	for _, d := range devs {
		seen[d.ID] = true
		out = append(out, protocol.Device{
			ID:       d.ID,
			Name:     d.Name,
			Platform: d.Platform,
			Online:   online[d.ID],
			LastSeen: d.LastSeen,
		})
	}
	now := time.Now().UTC()
	for _, d := range s.hub.ephemeralDevices(username) {
		if seen[d.ID] {
			continue
		}
		d.LastSeen = now
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Server) username(r *http.Request) (string, bool) {
	var token string
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if token == "" {
		if c, err := r.Cookie(sessionCookie); err == nil {
			token = c.Value
		}
	}
	if token == "" {
		return "", false
	}
	username, err := auth.ParseToken(s.store.Secret(), token)
	if err != nil || username == "" {
		return "", false
	}
	if _, ok := s.store.User(username); !ok {
		return "", false
	}
	return username, true
}

type userKey struct{}

func currentUser(r *http.Request) string {
	u, _ := r.Context().Value(userKey{}).(string)
	return u
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求格式错误")
	}
	return nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

var deviceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type attemptLimiter struct {
	mu sync.Mutex
	m  map[string]*attempt
}

type attempt struct {
	count int
	reset time.Time
}

const (
	attemptWindow = 10 * time.Minute
	maxAttempts   = 10
)

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{m: map[string]*attempt{}}
}

func (l *attemptLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.m[key]
	if !ok {
		return false
	}
	if time.Now().After(a.reset) {
		delete(l.m, key)
		return false
	}
	return a.count >= maxAttempts
}

func (l *attemptLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.m) > 10000 {
		for k, a := range l.m {
			if now.After(a.reset) {
				delete(l.m, k)
			}
		}
	}
	a, ok := l.m[key]
	if !ok || now.After(a.reset) {
		l.m[key] = &attempt{count: 1, reset: now.Add(attemptWindow)}
		return
	}
	a.count++
}

func (l *attemptLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}
