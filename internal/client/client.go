package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filet/internal/protocol"
)

type Config struct {
	Server     string `json:"server"`
	Token      string `json:"token"`
	Username   string `json:"username"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
}

func ConfigPath() string {
	if p := os.Getenv("FILET_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "filet-config.json"
	}
	return filepath.Join(home, ".filet", "config.json")
}

func LoadConfig() (*Config, error) {
	raw, err := os.ReadFile(ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", ConfigPath(), err)
	}
	return &c, nil
}

func SaveConfig(c *Config) error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o600)
}

func NormalizeServer(server string) string {
	server = strings.TrimSpace(server)
	server = strings.TrimRight(server, "/")
	if server == "" {
		return server
	}
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	return server
}

type API struct {
	Server string
	Token  string
	hc     *http.Client
}

func NewAPI(server, token string) *API {
	return &API{
		Server: NormalizeServer(server),
		Token:  token,
		hc:     &http.Client{Timeout: 20 * time.Second},
	}
}

func (a *API) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Server+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接服务端失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if msg == "" {
			msg = resp.Status
		}
		return errors.New(msg)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func Register(ctx context.Context, server, username, password string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	api := NewAPI(server, "")
	return api.do(ctx, http.MethodPost, "/api/register", map[string]string{
		"username": username,
		"password": password,
	}, nil)
}

func Login(ctx context.Context, server, username, password string) (*Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	api := NewAPI(server, "")
	var out struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	if err := api.do(ctx, http.MethodPost, "/api/login", map[string]string{
		"username": username,
		"password": password,
	}, &out); err != nil {
		return nil, err
	}
	return &Config{Server: api.Server, Token: out.Token, Username: out.Username}, nil
}

func (a *API) Me(ctx context.Context) (string, error) {
	var out struct {
		Username string `json:"username"`
	}
	if err := a.do(ctx, http.MethodGet, "/api/me", nil, &out); err != nil {
		return "", err
	}
	return out.Username, nil
}

func (a *API) Devices(ctx context.Context) ([]Device, error) {
	var out []Device
	if err := a.do(ctx, http.MethodGet, "/api/devices", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *API) ICE(ctx context.Context) ([]string, error) {
	var out struct {
		URLs []string `json:"urls"`
	}
	if err := a.do(ctx, http.MethodGet, "/api/ice", nil, &out); err != nil {
		return nil, err
	}
	return out.URLs, nil
}

type Device = protocol.Device
