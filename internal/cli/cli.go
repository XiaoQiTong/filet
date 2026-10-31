package cli

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"filet/internal/client"
)

var flagServer string

func Execute(version string) {
	root := &cobra.Command{
		Use:   "filet",
		Short: "登录账号、查看设备、点对点直发文件",
		Long: `filet 由三部分组成：
  server  部署在公网服务器上的控制面（账号、设备列表、信令）
  agent   每台接收设备上常驻的接收端
  send    命令行发送端（网页端也可以直接发送）

文件数据只在发送端和接收端之间通过 WebRTC 直连，不经过服务器。`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&flagServer, "server", "s", "", "控制面地址，例如 https://files.example.com")
	root.AddCommand(
		newServerCmd(),
		newRegisterCmd(),
		newLoginCmd(),
		newLogoutCmd(),
		newWhoamiCmd(),
		newDevicesCmd(),
		newAgentCmd(),
		newSendCmd(),
		newStunCmd(),
		newVersionCmd(version),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func loadConfig() *client.Config {
	cfg, err := client.LoadConfig()
	if err != nil {
		fatalf("%v", err)
	}
	return cfg
}

func resolveServer(cfg *client.Config) string {
	if flagServer != "" {
		return client.NormalizeServer(flagServer)
	}
	if env := os.Getenv("FILET_SERVER"); env != "" {
		return client.NormalizeServer(env)
	}
	if cfg.Server != "" {
		return client.NormalizeServer(cfg.Server)
	}
	fatalf("请用 -s/--server 指定控制面地址，例如 -s https://files.example.com")
	return ""
}

func requireToken(cfg *client.Config) {
	if cfg.Token == "" {
		fatalf("尚未登录，请先运行 filet login -s <控制面地址> -u <用户名>")
	}
}

func promptLine(label string) string {
	fmt.Fprint(os.Stderr, label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fatalf("读取输入失败: %v", err)
	}
	return strings.TrimSpace(line)
}

func promptPassword(label string) string {
	fmt.Fprint(os.Stderr, label)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		raw, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fatalf("读取密码失败: %v", err)
		}
		return string(raw)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fatalf("读取密码失败: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func ensureDeviceID(cfg *client.Config) error {
	if cfg.DeviceID != "" {
		return nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	cfg.DeviceID = hex.EncodeToString(b)
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	os.Exit(1)
}
