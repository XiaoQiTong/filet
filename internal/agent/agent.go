package agent

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"filet/internal/client"
	"filet/internal/p2p"
	"filet/internal/protocol"
	"filet/internal/transfer"
)

type Options struct {
	Server   string
	Token    string
	DeviceID string
	Name     string
	Platform string
	SaveDir  string
	ICE      []string
	Logger   *log.Logger

	// OnToken 在会话令牌续期后回调，用于把新令牌写回配置。
	OnToken func(token string)
}

type Agent struct {
	opts       Options
	log        *log.Logger
	tokenSaved time.Time
}

func New(opts Options) *Agent {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Agent{opts: opts, log: logger}
}

func (a *Agent) logf(format string, args ...any) {
	a.log.Printf(format, args...)
}

// tokenSaveInterval 限制配置写入频率：断线重连每次都会拿到新令牌，
// 但内存里更新即可，落盘按小时节流。
const tokenSaveInterval = time.Hour

func (a *Agent) saveToken(token string) {
	if a.opts.OnToken == nil {
		return
	}
	if !a.tokenSaved.IsZero() && time.Since(a.tokenSaved) < tokenSaveInterval {
		return
	}
	a.tokenSaved = time.Now()
	a.opts.OnToken(token)
}

func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		connected, err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			backoff = time.Second
		}
		a.logf("信令连接断开: %v，%s 后重连", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (a *Agent) session(ctx context.Context) (bool, error) {
	ws, err := client.DialWS(ctx, a.opts.Server, a.opts.Token, a.opts.DeviceID, a.opts.Name, a.opts.Platform)
	if err != nil {
		return false, err
	}
	defer ws.Close()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-stop:
		}
	}()
	go func() {
		addr, err := p2p.MappedAddr(a.opts.ICE, 4*time.Second)
		if err != nil {
			return
		}
		if err := ws.SendInfo(ctx, addr); err != nil {
			return
		}
		a.logf("外网地址: %s", addr)
	}()

	peers := map[string]*p2p.Conn{}
	var mu sync.Mutex
	defer func() {
		mu.Lock()
		for _, c := range peers {
			c.Close()
		}
		mu.Unlock()
	}()

	connected := false
	for {
		env, err := ws.Read()
		if err != nil {
			return connected, err
		}
		switch env.Type {
		case protocol.TypeHello:
			connected = true
			a.logf("已上线：账号 %s / 设备 %s", env.Username, a.opts.Name)
		case protocol.TypeToken:
			if env.Token == "" || env.Token == a.opts.Token {
				continue
			}
			a.opts.Token = env.Token
			a.saveToken(env.Token)
		case protocol.TypeError:
			a.logf("服务端提示: %s", env.Error)
		case protocol.TypeSignal:
			a.handleSignal(ctx, ws, peers, &mu, env)
		}
	}
}

func (a *Agent) handleSignal(ctx context.Context, ws *client.WS, peers map[string]*p2p.Conn, mu *sync.Mutex, env protocol.Envelope) {
	var sig p2p.Signal
	if err := json.Unmarshal(env.Data, &sig); err != nil {
		return
	}
	switch sig.Kind {
	case "offer":
		if sig.Description == nil {
			return
		}
		conn, err := p2p.NewConn(a.opts.ICE)
		if err != nil {
			a.logf("创建连接失败: %v", err)
			return
		}
		dcCh := make(chan *webrtc.DataChannel, 1)
		conn.PC.OnDataChannel(func(dc *webrtc.DataChannel) {
			select {
			case dcCh <- dc:
			default:
			}
		})
		forward := func(s p2p.Signal) error {
			return ws.SendSignal(context.Background(), env.From, s)
		}
		p2p.ForwardCandidates(conn.PC, forward)

		mu.Lock()
		if old := peers[env.From]; old != nil {
			old.Close()
		}
		peers[env.From] = conn
		mu.Unlock()

		answer, err := p2p.NewAnswer(conn.PC, *sig.Description)
		if err != nil {
			a.logf("处理连接请求失败: %v", err)
			mu.Lock()
			if peers[env.From] == conn {
				delete(peers, env.From)
			}
			mu.Unlock()
			conn.Close()
			return
		}
		if err := ws.SendSignal(ctx, env.From, answer); err != nil {
			a.logf("发送应答失败: %v", err)
		}
		go a.receive(ctx, conn, dcCh, env.From, peers, mu)
	case "candidate":
		if sig.Candidate == nil {
			return
		}
		mu.Lock()
		conn := peers[env.From]
		mu.Unlock()
		if conn != nil {
			if err := conn.AddCandidate(*sig.Candidate); err != nil {
				a.logf("添加 ICE 候选失败: %v", err)
			}
		}
	}
}

func (a *Agent) receive(ctx context.Context, conn *p2p.Conn, dcCh <-chan *webrtc.DataChannel, from string, peers map[string]*p2p.Conn, mu *sync.Mutex) {
	defer func() {
		mu.Lock()
		if peers[from] == conn {
			delete(peers, from)
		}
		mu.Unlock()
		conn.Close()
	}()

	var dc *webrtc.DataChannel
	select {
	case dc = <-dcCh:
	case <-time.After(30 * time.Second):
		a.logf("等待数据通道超时（来自 %s）", from)
		return
	case <-ctx.Done():
		return
	}

	if _, err := (&transfer.Receiver{SaveDir: a.opts.SaveDir, Logf: a.logf}).Handle(ctx, dc); err != nil {
		a.logf("接收失败（来自 %s）: %v", from, err)
	}
}
