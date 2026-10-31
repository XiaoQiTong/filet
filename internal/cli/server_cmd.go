package cli

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"filet/internal/server"
	"filet/internal/stunserver"
)

const defaultICE = "stun:stun.miwifi.com:3478,stun:stun.chat.bilibili.com:3478,stun:stun.l.google.com:19302"

func newServerCmd() *cobra.Command {
	var (
		listen     string
		dataDir    string
		noRegister bool
		iceFlag    string
		stunAddr   string
		publicIP   string
		certFile   string
		keyFile    string
	)
	cmd := &cobra.Command{
		Use:   "server",
		Short: "启动控制面服务（账号、设备列表、信令）",
		RunE: func(cmd *cobra.Command, args []string) error {
			ice := splitList(iceFlag)
			if publicIP != "" {
				port := "3478"
				if stunAddr != "" {
					if _, p, err := net.SplitHostPort(stunAddr); err == nil && p != "" {
						port = p
					} else if strings.HasPrefix(stunAddr, ":") {
						port = strings.TrimPrefix(stunAddr, ":")
					}
				}
				ice = append([]string{fmt.Sprintf("stun:%s:%s", publicIP, port)}, ice...)
			}
			srv, err := server.New(server.Config{
				Addr:       listen,
				DataDir:    dataDir,
				NoRegister: noRegister,
				ICEURLs:    ice,
				TLSCert:    certFile,
				TLSKey:     keyFile,
				STUNAddr:   stunAddr,
			})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return srv.Run(ctx)
		},
	}
	cmd.Flags().StringVarP(&listen, "listen", "l", ":8085", "HTTP 监听地址（默认 8085，建议放在反向代理后面，监听 127.0.0.1）")
	cmd.Flags().StringVarP(&dataDir, "data", "d", "filet-data", "数据目录（账号、设备、密钥）")
	cmd.Flags().BoolVar(&noRegister, "no-register", false, "关闭自助注册")
	cmd.Flags().StringVar(&iceFlag, "ice", defaultICE, "下发给客户端的 STUN 地址，逗号分隔")
	cmd.Flags().StringVar(&stunAddr, "stun", ":3478", "同时启动内置 STUN 的 UDP 地址，留空禁用")
	cmd.Flags().StringVar(&publicIP, "public-ip", "", "ECS 公网 IP，提供后会自动加入内置 STUN 地址")
	cmd.Flags().StringVar(&certFile, "tls-cert", "", "直接启用 HTTPS 的证书文件（可选）")
	cmd.Flags().StringVar(&keyFile, "tls-key", "", "HTTPS 私钥文件（可选）")
	return cmd
}

func newStunCmd() *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "stun",
		Short: "单独运行一个轻量 STUN 服务",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return stunserver.Run(ctx, listen, log.Printf)
		},
	}
	cmd.Flags().StringVarP(&listen, "listen", "l", ":3478", "UDP 监听地址")
	return cmd
}

func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "显示版本信息",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("filet %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		},
	}
}
