package transfer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	ChunkSize = 16 * 1024
	highWater = 1 << 20
	lowWater  = 256 << 10
)

type Control struct {
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	OK     bool   `json:"ok,omitempty"`
	Path   string `json:"path,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Result struct {
	OK   bool
	Path string
	Size int64
}

// Send streams one file over dc. progress is called after every chunk.
func Send(ctx context.Context, dc *webrtc.DataChannel, filePath, name string, progress func(sent, total int64)) (Result, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Result{}, err
	}
	if info.IsDir() {
		return Result{}, fmt.Errorf("%s 是目录", filePath)
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Result{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}

	ctrl := make(chan Control, 8)
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if !m.IsString {
			return
		}
		var c Control
		if json.Unmarshal(m.Data, &c) != nil {
			return
		}
		select {
		case ctrl <- c:
		default:
		}
	})
	low := make(chan struct{}, 1)
	dc.SetBufferedAmountLowThreshold(lowWater)
	dc.OnBufferedAmountLow(func() {
		select {
		case low <- struct{}{}:
		default:
		}
	})

	meta, err := json.Marshal(Control{Kind: "meta", Name: name, Size: info.Size(), SHA256: sum})
	if err != nil {
		return Result{}, err
	}
	if err := dc.SendText(string(meta)); err != nil {
		return Result{}, err
	}

	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ready, err := waitControl(readyCtx, ctrl, "ready")
	cancel()
	if err != nil {
		return Result{}, fmt.Errorf("等待接收方就绪: %w", err)
	}
	if ready.Kind == "done" {
		return Result{}, errors.New(ready.Error)
	}

	total := info.Size()
	sent := int64(0)
	buf := make([]byte, ChunkSize)
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			if err := waitBuffer(ctx, dc, low); err != nil {
				return Result{}, err
			}
			if err := dc.Send(buf[:n]); err != nil {
				return Result{}, err
			}
			sent += int64(n)
			if progress != nil {
				progress(sent, total)
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return Result{}, rerr
		}
	}

	doneCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	done, err := waitControl(doneCtx, ctrl, "done")
	cancel()
	if err != nil {
		return Result{}, fmt.Errorf("等待接收方确认: %w", err)
	}
	if !done.OK {
		return Result{}, fmt.Errorf("接收方失败: %s", done.Error)
	}
	return Result{OK: true, Path: done.Path, Size: done.Bytes}, nil
}

func waitControl(ctx context.Context, ch <-chan Control, kind string) (Control, error) {
	for {
		select {
		case c := <-ch:
			if c.Kind == kind {
				return c, nil
			}
			if c.Kind == "done" && !c.OK {
				return c, nil
			}
		case <-ctx.Done():
			return Control{}, ctx.Err()
		}
	}
}

func waitBuffer(ctx context.Context, dc *webrtc.DataChannel, low <-chan struct{}) error {
	for dc.BufferedAmount() > highWater {
		select {
		case <-low:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
			if dc.BufferedAmount() > highWater {
				return errors.New("发送缓冲持续拥塞，接收方可能已停止读取")
			}
		}
	}
	return nil
}

type Receiver struct {
	SaveDir string
	Logf    func(string, ...any)
}

// Handle blocks until the transfer finishes, fails, or ctx is canceled.
func (r *Receiver) Handle(ctx context.Context, dc *webrtc.DataChannel) (Result, error) {
	saveDir := r.SaveDir
	if saveDir == "" {
		saveDir = "."
	}
	st := &recvState{
		dc:      dc,
		saveDir: saveDir,
		logf:    r.Logf,
		doneCh:  make(chan struct{}),
	}
	dc.OnMessage(st.onMessage)
	dc.OnClose(func() { st.abort("传输连接已关闭") })

	select {
	case <-st.doneCh:
	case <-ctx.Done():
		st.abort("传输超时或已取消")
	}
	return st.result, st.err
}

type recvState struct {
	mu       sync.Mutex
	dc       *webrtc.DataChannel
	saveDir  string
	logf     func(string, ...any)
	file     *os.File
	writer   *bufio.Writer
	hash     hash.Hash
	meta     Control
	written  int64
	tmpPath  string
	finished bool
	result   Result
	err      error
	doneCh   chan struct{}
}

func (s *recvState) onMessage(m webrtc.DataChannelMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	if m.IsString {
		var c Control
		if json.Unmarshal(m.Data, &c) != nil {
			return
		}
		if c.Kind == "meta" {
			if s.file != nil {
				s.failLocked("重复的文件元数据")
				return
			}
			s.startLocked(c)
		}
		return
	}
	if s.file == nil {
		s.failLocked("收到数据但缺少文件元数据")
		return
	}
	if _, err := s.writer.Write(m.Data); err != nil {
		s.failLocked("写入临时文件失败: " + err.Error())
		return
	}
	if _, err := s.hash.Write(m.Data); err != nil {
		s.failLocked("计算校验值失败: " + err.Error())
		return
	}
	s.written += int64(len(m.Data))
	if s.written > s.meta.Size {
		s.failLocked("收到的数据超过声明大小")
		return
	}
	if s.written == s.meta.Size {
		s.completeLocked()
	}
}

func (s *recvState) startLocked(c Control) {
	if c.Size < 0 {
		s.failLocked("文件大小无效")
		return
	}
	if err := os.MkdirAll(s.saveDir, 0o755); err != nil {
		s.failLocked("创建保存目录失败: " + err.Error())
		return
	}
	f, err := os.CreateTemp(s.saveDir, ".filet-*.part")
	if err != nil {
		s.failLocked("创建临时文件失败: " + err.Error())
		return
	}
	s.file = f
	s.tmpPath = f.Name()
	s.writer = bufio.NewWriterSize(f, 256*1024)
	s.hash = sha256.New()
	s.meta = c
	if s.logf != nil {
		s.logf("开始接收 %s (%d 字节)", c.Name, c.Size)
	}
	if err := s.dc.SendText(`{"kind":"ready"}`); err != nil {
		s.failLocked("发送就绪应答失败: " + err.Error())
		return
	}
	if c.Size == 0 {
		s.completeLocked()
	}
}

func (s *recvState) completeLocked() {
	if s.file == nil {
		s.failLocked("没有可完成的文件")
		return
	}
	if err := s.writer.Flush(); err != nil {
		s.failLocked("刷新临时文件失败: " + err.Error())
		return
	}
	if err := s.file.Close(); err != nil {
		s.failLocked("关闭临时文件失败: " + err.Error())
		return
	}
	s.file = nil

	gotSum := hex.EncodeToString(s.hash.Sum(nil))
	if s.meta.SHA256 != "" && !strings.EqualFold(gotSum, s.meta.SHA256) {
		os.Remove(s.tmpPath)
		s.failLocked("sha256 校验失败")
		return
	}
	finalPath := uniquePath(s.saveDir, sanitizeName(s.meta.Name))
	if err := os.Rename(s.tmpPath, finalPath); err != nil {
		os.Remove(s.tmpPath)
		s.failLocked("保存文件失败: " + err.Error())
		return
	}
	done, _ := json.Marshal(Control{Kind: "done", OK: true, Path: finalPath, Bytes: s.written})
	_ = s.dc.SendText(string(done))
	if s.logf != nil {
		note := ""
		if s.meta.SHA256 == "" {
			note = "（发送方未提供校验值）"
		}
		s.logf("接收完成: %s -> %s%s", s.meta.Name, finalPath, note)
	}
	s.finishLocked(nil, Result{OK: true, Path: finalPath, Size: s.written})
}

func (s *recvState) failLocked(msg string) {
	errMsg, _ := json.Marshal(Control{Kind: "done", OK: false, Error: msg})
	_ = s.dc.SendText(string(errMsg))
	s.finishLocked(errors.New(msg), Result{})
}

func (s *recvState) finishLocked(err error, result Result) {
	if s.finished {
		return
	}
	s.finished = true
	s.err = err
	s.result = result
	close(s.doneCh)
}

func (s *recvState) abort(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
	if s.tmpPath != "" {
		os.Remove(s.tmpPath)
	}
	s.finishLocked(errors.New(msg), Result{})
}

func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	ext := path.Ext(name)
	switch strings.ToUpper(strings.TrimSuffix(name, ext)) {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		name = "_" + name
	}
	if runes := []rune(name); len(runes) > 120 {
		keep := 120 - len([]rune(ext))
		if keep < 1 {
			keep = 120
			ext = ""
		}
		name = string(runes[:keep]) + ext
	}
	return name
}

func uniquePath(dir, name string) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	p := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
}
