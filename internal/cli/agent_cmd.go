package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"filet/internal/agent"
	"filet/internal/client"
)

func newAgentCmd() *cobra.Command {
	var (
		name    string
		saveDir string
		iceFlag string
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "常驻接收端：自动接收同账号设备发来的文件",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			cfg.Server = resolveServer(cfg)
			requireToken(cfg)
			if err := ensureDeviceID(cfg); err != nil {
				return err
			}
			if name == "" {
				if cfg.DeviceName != "" {
					name = cfg.DeviceName
				} else if host, err := os.Hostname(); err == nil {
					name = host
				} else {
					name = "filet-device"
				}
			}
			cfg.DeviceName = name
			if err := client.SaveConfig(cfg); err != nil {
				return fmt.Errorf("保存配置失败: %w", err)
			}

			if saveDir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					saveDir = "filet-received"
				} else {
					saveDir = filepath.Join(home, "Downloads", "filet")
				}
			}
			absDir, err := filepath.Abs(saveDir)
			if err != nil {
				absDir = saveDir
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			iceURLs := splitList(iceFlag)
			if len(iceURLs) == 0 {
				apiCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				iceURLs, err = client.NewAPI(cfg.Server, cfg.Token).ICE(apiCtx)
				cancel()
				if err != nil {
					fmt.Fprintf(os.Stderr, "警告: 获取 STUN 配置失败（%v），仅尝试局域网/直连\n", err)
				}
			}

			fmt.Printf(`filet agent
  服务器   : %s
  账号     : %s
  设备名   : %s
  设备 ID  : %s
  保存目录 : %s
  STUN     : %v

按 Ctrl+C 退出。
`, cfg.Server, cfg.Username, name, cfg.DeviceID, absDir, iceURLs)

			logger := log.New(os.Stderr, "", log.LstdFlags)
			a := agent.New(agent.Options{
				Server:   cfg.Server,
				Token:    cfg.Token,
				DeviceID: cfg.DeviceID,
				Name:     name,
				Platform: runtime.GOOS + "/" + runtime.GOARCH,
				SaveDir:  absDir,
				ICE:      iceURLs,
				Logger:   logger,
				OnToken: func(token string) {
					fresh, err := client.LoadConfig()
					if err != nil {
						logger.Printf("会话续期失败（读取配置）: %v", err)
						return
					}
					fresh.Token = token
					if err := client.SaveConfig(fresh); err != nil {
						logger.Printf("会话续期失败（写入配置）: %v", err)
						return
					}
					logger.Printf("会话令牌已续期，重新计 30 天有效期")
				},
			})
			return a.Run(ctx)
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", "设备名称（默认主机名）")
	cmd.Flags().StringVarP(&saveDir, "dir", "d", "", "接收文件保存目录（默认 ~/Downloads/filet）")
	cmd.Flags().StringVar(&iceFlag, "ice", "", "覆盖 STUN 地址，逗号分隔")
	return cmd
}
