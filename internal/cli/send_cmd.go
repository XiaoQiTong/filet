package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/spf13/cobra"

	"filet/internal/client"
	"filet/internal/p2p"
	"filet/internal/protocol"
	"filet/internal/transfer"
)

func newSendCmd() *cobra.Command {
	var (
		targets []string
		all     bool
		iceFlag string
	)
	cmd := &cobra.Command{
		Use:   "send <文件>",
		Short: "把文件点对点直发给指定设备",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			info, err := os.Stat(filePath)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return fmt.Errorf("%s 是目录，当前版本只支持发送单个文件", args[0])
			}

			cfg := loadConfig()
			cfg.Server = resolveServer(cfg)
			requireToken(cfg)

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			api := client.NewAPI(cfg.Server, cfg.Token)
			devices, err := api.Devices(ctx)
			if err != nil {
				return err
			}
			chosen, err := pickTargets(devices, cfg.DeviceID, targets, all)
			if err != nil {
				return err
			}
			iceURLs := splitList(iceFlag)
			if len(iceURLs) == 0 {
				iceURLs, err = api.ICE(ctx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "警告: 获取 STUN 配置失败（%v），仅尝试局域网/直连\n", err)
				}
			}

			ws, err := client.DialWS(ctx, cfg.Server, cfg.Token, "", "", "")
			if err != nil {
				return err
			}
			defer ws.Close()
			go func() {
				<-ctx.Done()
				ws.Close()
			}()

			envCh := make(chan protocol.Envelope, 32)
			errCh := make(chan error, 1)
			done := make(chan struct{})
			defer close(done)
			go func() {
				for {
					env, err := ws.Read()
					if err != nil {
						select {
						case errCh <- err:
						case <-done:
						}
						return
					}
					select {
					case envCh <- env:
					case <-done:
						return
					}
				}
			}()

			fmt.Printf("发送 %s（%s）到 %d 台设备\n", filepath.Base(filePath), humanBytes(info.Size()), len(chosen))
			failed := 0
			for i, dev := range chosen {
				fmt.Printf("[%d/%d] %s (%s)\n", i+1, len(chosen), dev.Name, dev.Platform)
				drainEnvelopes(envCh)
				res, err := sendToDevice(ctx, ws, envCh, errCh, dev, filePath, iceURLs)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ✗ %v\n", err)
					failed++
					continue
				}
				fmt.Printf("  ✓ 已送达 %s，保存在 %s\n", dev.Name, res.Path)
			}
			if failed > 0 {
				return fmt.Errorf("%d 台设备发送失败", failed)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&targets, "to", "t", nil, "目标设备名称或 ID，可重复或用逗号分隔")
	cmd.Flags().BoolVar(&all, "all", false, "发送给所有在线设备（本机除外）")
	cmd.Flags().StringVar(&iceFlag, "ice", "", "覆盖 STUN 地址，逗号分隔")
	return cmd
}

func sendToDevice(ctx context.Context, ws *client.WS, envCh <-chan protocol.Envelope, errCh <-chan error, dev client.Device, filePath string, ice []string) (transfer.Result, error) {
	conn, err := p2p.NewConn(ice)
	if err != nil {
		return transfer.Result{}, err
	}
	defer conn.Close()

	dc, err := conn.PC.CreateDataChannel("file", nil)
	if err != nil {
		return transfer.Result{}, err
	}
	openCh := make(chan struct{})
	dc.OnOpen(func() {
		select {
		case <-openCh:
		default:
			close(openCh)
		}
	})
	dcClosed := make(chan struct{})
	dc.OnClose(func() {
		select {
		case <-dcClosed:
		default:
			close(dcClosed)
		}
	})

	p2p.ForwardCandidates(conn.PC, func(s p2p.Signal) error {
		return ws.SendSignal(ctx, dev.ID, s)
	})
	offer, err := p2p.NewOffer(conn.PC)
	if err != nil {
		return transfer.Result{}, err
	}
	if err := ws.SendSignal(ctx, dev.ID, offer); err != nil {
		return transfer.Result{}, err
	}

	var answer *webrtc.SessionDescription
	answerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	for answer == nil {
		select {
		case <-answerCtx.Done():
			cancel()
			return transfer.Result{}, fmt.Errorf("等待应答超时：设备可能不在线，或双方 NAT 无法穿透")
		case env := <-envCh:
			desc, err := applySignal(env, conn, dev.ID)
			if err != nil {
				cancel()
				return transfer.Result{}, err
			}
			answer = desc
		case err := <-errCh:
			cancel()
			return transfer.Result{}, fmt.Errorf("信令连接断开: %w", err)
		}
	}
	cancel()
	if err := conn.SetRemote(*answer); err != nil {
		return transfer.Result{}, err
	}

	openCtx, cancelOpen := context.WithTimeout(ctx, 30*time.Second)
	err = waitDataChannel(openCtx, conn, dev.ID, openCh, dcClosed, envCh, errCh)
	cancelOpen()
	if err != nil {
		return transfer.Result{}, err
	}

	tctx, tcancel := context.WithCancel(ctx)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-tctx.Done():
				return
			case env := <-envCh:
				_, _ = applySignal(env, conn, dev.ID)
			case <-errCh:
				return
			}
		}
	}()

	p := &progressPrinter{total: -1, start: time.Now(), last: time.Now()}
	if info, err := os.Stat(filePath); err == nil {
		p.total = info.Size()
	}
	progress := func(sent, total int64) {
		p.total = total
		p.update(sent)
	}
	res, err := transfer.Send(tctx, dc, filePath, filepath.Base(filePath), progress)
	tcancel()
	<-drained
	if err != nil {
		return transfer.Result{}, err
	}
	return res, nil
}

func waitDataChannel(ctx context.Context, conn *p2p.Conn, devID string, openCh, dcClosed <-chan struct{}, envCh <-chan protocol.Envelope, errCh <-chan error) error {
	for {
		select {
		case <-openCh:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("直连建立超时：双方可能都在对称 NAT / 运营商 CGNAT 后，或 UDP 被防火墙拦截")
		case <-dcClosed:
			return fmt.Errorf("数据通道在建立阶段关闭")
		case env := <-envCh:
			if _, err := applySignal(env, conn, devID); err != nil {
				return err
			}
		case err := <-errCh:
			return fmt.Errorf("信令连接断开: %w", err)
		}
	}
}

func applySignal(env protocol.Envelope, conn *p2p.Conn, devID string) (*webrtc.SessionDescription, error) {
	if env.Type == protocol.TypeError {
		return nil, fmt.Errorf("服务端: %s", env.Error)
	}
	if env.Type != protocol.TypeSignal || env.From != devID {
		return nil, nil
	}
	var sig p2p.Signal
	if err := json.Unmarshal(env.Data, &sig); err != nil {
		return nil, nil
	}
	switch sig.Kind {
	case "answer":
		return sig.Description, nil
	case "candidate":
		if sig.Candidate != nil {
			_ = conn.AddCandidate(*sig.Candidate)
		}
	}
	return nil, nil
}

func drainEnvelopes(ch <-chan protocol.Envelope) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func pickTargets(devices []client.Device, selfID string, want []string, all bool) ([]client.Device, error) {
	if all {
		var out []client.Device
		for _, d := range devices {
			if d.Online && d.ID != selfID {
				out = append(out, d)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("没有其他在线设备（先在目标设备上运行 filet agent）")
		}
		return out, nil
	}

	var queries []string
	for _, item := range want {
		for _, part := range strings.Split(item, ",") {
			if p := strings.TrimSpace(part); p != "" {
				queries = append(queries, p)
			}
		}
	}
	if len(queries) == 0 {
		return nil, fmt.Errorf("请用 -t/--to 指定设备名称或 ID，或用 --all 发给全部在线设备")
	}

	var out []client.Device
	seen := map[string]bool{}
	for _, q := range queries {
		var match *client.Device
		for i := range devices {
			d := devices[i]
			if d.ID == q || (len(q) >= 6 && strings.HasPrefix(d.ID, q)) || strings.EqualFold(d.Name, q) {
				if match != nil && match.ID != d.ID {
					return nil, fmt.Errorf("“%s”匹配到多台设备，请改用设备 ID", q)
				}
				match = &devices[i]
			}
		}
		if match == nil {
			return nil, fmt.Errorf("找不到设备“%s”（先运行 filet devices 查看）", q)
		}
		if !match.Online {
			return nil, fmt.Errorf("设备“%s”不在线", q)
		}
		if selfID != "" && match.ID == selfID {
			return nil, fmt.Errorf("不能发送给本机自己的 agent")
		}
		if !seen[match.ID] {
			seen[match.ID] = true
			out = append(out, *match)
		}
	}
	return out, nil
}

type progressPrinter struct {
	total int64
	start time.Time
	last  time.Time
}

func (p *progressPrinter) update(sent int64) {
	now := time.Now()
	if sent < p.total && now.Sub(p.last) < 300*time.Millisecond {
		return
	}
	p.last = now
	elapsed := now.Sub(p.start).Seconds()
	speed := int64(0)
	if elapsed > 0 {
		speed = int64(float64(sent) / elapsed)
	}
	pct := 100.0
	if p.total > 0 {
		pct = float64(sent) * 100 / float64(p.total)
	}
	fmt.Fprintf(os.Stderr, "\r  进度 %5.1f%%  %s/%s  %s/s", pct, humanBytes(sent), humanBytes(p.total), humanBytes(speed))
	if sent >= p.total {
		fmt.Fprintln(os.Stderr)
	}
}
