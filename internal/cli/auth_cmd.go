package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"filet/internal/client"
)

func newRegisterCmd() *cobra.Command {
	var username, password string
	cmd := &cobra.Command{
		Use:   "register",
		Short: "注册账号",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			srv := resolveServer(cfg)
			if username == "" {
				username = promptLine("用户名: ")
			}
			if password == "" {
				password = promptPassword("密码: ")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := client.Register(ctx, srv, username, password); err != nil {
				return err
			}
			fmt.Printf("注册成功：%s\n下一步：filet login -u %s\n", username, username)
			return nil
		},
	}
	cmd.Flags().StringVarP(&username, "user", "u", "", "用户名")
	cmd.Flags().StringVarP(&password, "password", "p", "", "密码（不填则交互输入，推荐不填）")
	return cmd
}

func newLoginCmd() *cobra.Command {
	var username, password string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "登录并保存会话",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			srv := resolveServer(cfg)
			if username == "" {
				username = promptLine("用户名: ")
			}
			if password == "" {
				password = promptPassword("密码: ")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			fresh, err := client.Login(ctx, srv, username, password)
			if err != nil {
				return err
			}
			fresh.DeviceID = cfg.DeviceID
			fresh.DeviceName = cfg.DeviceName
			if err := client.SaveConfig(fresh); err != nil {
				return fmt.Errorf("保存配置失败: %w", err)
			}
			fmt.Printf("登录成功：%s @ %s\n配置已保存到 %s\n", fresh.Username, fresh.Server, client.ConfigPath())
			return nil
		},
	}
	cmd.Flags().StringVarP(&username, "user", "u", "", "用户名")
	cmd.Flags().StringVarP(&password, "password", "p", "", "密码（不填则交互输入，推荐不填）")
	return cmd
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "清除本地登录状态",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			cfg.Token = ""
			if err := client.SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Println("已退出登录")
			return nil
		},
	}
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "显示当前登录账号",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			cfg.Server = resolveServer(cfg)
			requireToken(cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			username, err := client.NewAPI(cfg.Server, cfg.Token).Me(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("%s @ %s\n", username, cfg.Server)
			return nil
		},
	}
}

func newDevicesCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "devices",
		Aliases: []string{"ls"},
		Short:   "列出账号下的所有设备",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfig()
			cfg.Server = resolveServer(cfg)
			requireToken(cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			devices, err := client.NewAPI(cfg.Server, cfg.Token).Devices(ctx)
			if err != nil {
				return err
			}
			if len(devices) == 0 {
				fmt.Println("还没有设备。请在目标设备上运行：filet agent")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "状态\t名称\t平台\t设备 ID\t最后在线")
			for _, d := range devices {
				status := "离线"
				if d.Online {
					status = "在线"
				}
				id := d.ID
				if len(id) > 8 {
					id = id[:8]
				}
				last := "-"
				if !d.LastSeen.IsZero() {
					last = d.LastSeen.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", status, d.Name, d.Platform, id, last)
			}
			_ = w.Flush()
			return nil
		},
	}
}
