# filet

自建的点对点文件直发服务：用账号登录，在网页或命令行里看到自己账号下所有设备，把文件直接送到目标设备。

控制面（部署在阿里云 ECS 上）只负责账号、设备在线状态和 WebRTC 信令；文件内容通过 DTLS/SCTP 加密数据通道在发送端与接收端之间直连传输，**不经过服务器**。

```
 浏览器 / CLI 发送端 ─── WebSocket（SDP / ICE 信令，JSON）───> 控制面 ECS
          │                                                      ▲
          └──── WebRTC DataChannel（16KB 分块，P2P 直连）────> agent / 网页接收端
```

## 功能

- 用户名密码注册/登录，密码用 argon2id 哈希，会话用 HS256 JWT（30 天），网页端用 HttpOnly Cookie，CLI 用 Bearer Token
- 同一账号下的设备自动注册上线，网页/CLI 均可查看在线状态
- 设备列表显示各设备的外网地址（STUN 探测的公网 IP:端口）和 IP 归属地（可关闭）
- 页面右上角显示本端信令在线状态（在线 / 已断开 / 重连中）；30 分钟无操作自动休眠并断开，任意操作即唤醒，传输过程中不会休眠
- 网页端拖拽上传，命令行 `filet send`，支持一次发给多台设备
- 网页端默认既是发送端也是接收端：页面打开即以临时设备（例如 `Chrome 页面`）上线，无需切换按钮
- 接收需确认：别人发文件时，接收端先出现一条文件请求，点「接收」后发送端才开始传输；请求 5 分钟未处理自动过期
- 网页接收列表实时显示，最多保留 12 条，点击条目即可下载；支持选择目录流式写盘（Chromium）或缓存后下载
- 文件分块走 WebRTC DataChannel，带背压控制；CLI 发送时附带 SHA-256 与 MD5，接收端自动比对；无论哪种发送方式，接收端都会打印落盘文件的 MD5
- 接收端自动按原名保存，重名自动加 ` (1)`；文件名做路径穿越与 Windows 保留名清洗
- 内置轻量 STUN 服务，ECS 上无需额外组件
- 单文件二进制，前端资源嵌入其中，无运行时依赖

## 快速开始（本机试跑）

```sh
./scripts/build.sh          # 或者：go build -o bin/filet ./cmd/filet
./bin/filet server -l 127.0.0.1:8085 -d ./devdata --stun :3478
# 另开终端
./bin/filet -s http://127.0.0.1:8085 register -u alice
./bin/filet -s http://127.0.0.1:8085 login -u alice
./bin/filet -s http://127.0.0.1:8085 agent -n dev-a -d ./devrecv

# 发送（第三个终端）
./bin/filet -s http://127.0.0.1:8085 send ./some-file.zip -t dev-a
./bin/filet -s http://127.0.0.1:8085 send ./some-file.zip --all
```

浏览器打开 `http://127.0.0.1:8085`，用同一账号登录即可看到设备并发送。

### 网页端

- **发送与接收一体**：登录后本页自动以临时设备（例如 `Chrome 页面`）出现在账号的设备列表里，既能作为发送端，也能被别人发送；关掉页面设备即下线。
- **发送**：拖入或选择文件，勾选在线设备后发送，进度和结果都显示在页面里。
- **接收**：别人发文件给你时会先出现一条文件请求（文件名、大小、来源），点「接收」后才开始直连传输，点「拒绝」则立即取消；5 分钟未处理自动过期。
- **接收列表**：收到的文件实时出现在下方，最多保留 12 条，点击条目即可下载。
- **设备列表**：显示设备名、平台、在线状态，以及外网地址（客户端用 STUN 探测出的公网 IP:端口）和 IP 归属地；归属地由服务端查询，`--geo-api ""` 可关闭。
- **保存位置**：Chrome/Edge 可点「选择保存目录」，数据边收边写入磁盘（列表仍可点击下载副本）；没有选择目录或浏览器不支持时，文件先缓存在内存，点击下载时再保存。

命令行 `filet agent` 作为常驻接收端没有交互界面，会直接接受同账号发来的文件。

### 命令行

| 命令 | 说明 |
| --- | --- |
| `filet server` | 启动控制面（账号、设备、信令，可选内置 STUN） |
| `filet register -u <用户名>` | 注册账号（密码交互输入） |
| `filet login -u <用户名>` | 登录并保存会话（`~/.filet/config.json`） |
| `filet logout` | 清除本地会话 |
| `filet whoami` | 显示当前账号 |
| `filet devices`（别名 `ls`） | 列出账号下所有设备 |
| `filet agent -n <设备名> -d <保存目录>` | 常驻接收端，自动接受同账号发来的文件（无确认界面） |
| `filet send <文件> -t <设备>` | 点对点发送；`-t` 可重复，`--all` 发给全部在线设备 |
| `filet stun` | 单独跑一个 STUN 服务 |
| `filet version` | 版本信息 |

全局参数：`-s/--server` 指定控制面地址（也可用 `FILET_SERVER` 环境变量）；`FILET_CONFIG` 可覆盖配置文件路径。

`server` 常用参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-l/--listen` | `:8085` | HTTP 监听地址，建议放在 Caddy/Nginx 后面只听 127.0.0.1 |
| `-d/--data` | `filet-data` | 数据目录（账号、设备、JWT 密钥） |
| `--no-register` | 关 | 关闭网页/CLI 自助注册 |
| `--ice` | 公共 STUN 列表 | 下发给客户端的 STUN 地址，逗号分隔 |
| `--stun` | `:3478` | 内置 STUN 的 UDP 监听地址，留空禁用 |
| `--public-ip` | 空 | ECS 公网 IP，提供后自动加入内置 STUN 地址 |
| `--geo-api` | `http://ip-api.com/json/{ip}?lang=zh-CN&fields=status,country,regionName,city` | 按 IP 查归属地的接口模板，`{ip}` 为占位符；留空则关闭归属地显示 |
| `--tls-cert` / `--tls-key` | 空 | 不挂反向代理时可直接由 filet 提供 HTTPS |

`agent` / `send` 都支持 `--ice` 覆盖 STUN；默认从控制面 `/api/ice` 获取。

## 部署到阿里云 ECS

### 1. 安全组

| 端口 | 协议 | 用途 |
| --- | --- | --- |
| 443 | TCP | HTTPS（Caddy 反向代理） |
| 80 | TCP | Let's Encrypt 证书签发/跳转 |
| 3478 | UDP | 内置 STUN |

8085（filet 本体）只监听 127.0.0.1，不要对公网开放。WebRTC 的数据端口是双方 NAT 协商出来的临时 UDP 端口，不需要在安全组里开固定范围（安全组默认放行出方向即可）。

大陆节点绑定域名需要先完成 ICP 备案；只给 IP 用 HTTP 也能跑，但浏览器会把它当不安全上下文，建议还是上 HTTPS（WebRTC 本身不强制，但 Cookie 的 Secure、剪贴板等能力会受影响）。

### 2. 直接部署（systemd + Caddy）

```sh
# 上传二进制
sudo install -m 0755 dist/filet-linux-amd64 /usr/local/bin/filet

# 专用用户与数据目录
sudo useradd --system --home /var/lib/filet --shell /usr/sbin/nologin filet
sudo install -d -o filet -g filet -m 0700 /var/lib/filet

# 服务单元：把 --public-ip 改成你的 ECS 公网 IP
sudo cp deploy/filet.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now filet
sudo systemctl status filet

# Caddy：把域名换成你的，Caddy 自动申请证书并转发 WebSocket
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

`deploy/filet.service` 里已经带了 `ProtectSystem=strict`、`NoNewPrivileges` 等加固项；`--public-ip` 会同时用于页面提示和内置 STUN。

### 3. 接收设备

每台要接收文件的机器都需要跑 `filet agent`：

```sh
filet -s https://files.example.com login -u alice
filet agent -n my-laptop -d ~/Downloads/filet
```

Linux 机器可以装成系统服务，参考 `deploy/filet-agent.service`（先以同一用户执行一次 `login` 生成配置）。Windows 可以用计划任务或 `nssm` 把 `filet.exe agent` 做成开机自启。

## 自动打包（GitHub Actions）

仓库内置两条 workflow：

- `.github/workflows/ci.yml`：每次 push / PR 执行 `go vet`、编译并交叉编译，产物作为 workflow artifact 保留 7 天
- `.github/workflows/release.yml`：推送 `v*` 标签（或手动触发）时交叉编译并创建 GitHub Release，附带可下载的压缩包和 `SHA256SUMS`；Release 正文取自 `CHANGELOG.md` 里对应的 `## vX.Y.Z` 小节，缺这一节会直接失败

发版前先在 `CHANGELOG.md` 补好新版本小节，再推送标签：

```sh
git tag v0.1.0
git push origin v0.1.0
```

Release 资产命名（`<version>` 带 `v` 前缀）：

| 平台 | 文件 |
| --- | --- |
| Linux x86_64 | `filet-<version>-linux-amd64.tar.gz` |
| Linux ARM64 | `filet-<version>-linux-arm64.tar.gz` |
| Windows x64 | `filet-<version>-windows-amd64.zip` |
| macOS Intel | `filet-<version>-darwin-amd64.tar.gz` |
| macOS Apple Silicon | `filet-<version>-darwin-arm64.tar.gz` |

下载后解压得到单文件二进制，直接按上文部署即可，不需要装 Go。CI 使用 `go.mod` 里声明的 Go 版本。

## 数据与安全

- 数据全部在 `--data` 目录的 `store.json`：账号、argon2id 密码哈希、设备列表、JWT 签名密钥。请限制目录权限并做备份；删除该文件等于清空所有账号与设备记录。
- 登录失败有速率限制（10 次/10 分钟/IP）；WebSocket 连接要求登录态；信令只在同一账号的 peer 之间转发。
- 网页端 CSP 为 `default-src 'self'`，脚本样式全部外链。
- 校验值：`filet send` 会先算好 SHA-256 与 MD5 随文件头发给接收端，接收端写盘后再算一遍比对，任一不一致就删除临时文件并报「sha256/md5 校验失败」；`filet agent` 的日志和 `filet send` 的成功输出都会显示文件的 MD5。浏览器没有原生 MD5 实现，网页端发送不携带校验值、接收也不校验，此时接收端只打印算出的 MD5 并提示「请自行比对」，完整性依赖 DTLS/SCTP 的加密与校验。
- 服务器看得到元数据（账号、设备名、文件名、大小、传输时段），看不到文件内容。
- 设备的外网地址由客户端主动上报（STUN 探测结果）；归属地由服务端把公网 IP 发往 `--geo-api` 指定的第三方接口（默认 ip-api.com）查询，结果缓存 24 小时，失败缓存 10 分钟。不希望 IP 离开服务器时用 `--geo-api ""` 关闭；私网/回环地址不会发起查询。
- 会话令牌（HS256 JWT）有效期 30 天。`filet agent` 常驻期间由服务端在连接建立时及每 12 小时下发新令牌并自动写回 `~/.filet/config.json`，长期在线不会过期；连续离线超过 30 天需要重新 `filet login`，日志会直接提示「会话无效或已过期」。
- 「退出登录」只清本地凭据（浏览器 cookie 或 CLI 配置里的令牌），服务端不吊销已签发的 JWT：令牌在剩余有效期内仍然可用。共用电脑请记得退出并清浏览器数据；怀疑令牌泄漏时，停服后删除 `store.json` 里的 `jwt_secret` 字段再启动，会生成新密钥并使所有旧令牌作废（全员需重新登录）。
- 网页登录 cookie 同样持续 30 天，关闭浏览器后重开仍是登录态；30 分钟无操作只是休眠断线，任意操作即恢复，不需要重新输入密码。

## 已知限制

- **对称 NAT / 运营商 CGNAT**：没有内置 TURN 中继时，双方都处在对称 NAT 后无法直连，会提示直连超时。优先使用 IPv6 或至少一方有公网地址；确有需要可在 `--ice` 里加自建 TURN。
- 接收端需要运行 `filet agent`，或保持 filet 网页打开；网页接收要人点「接收」，CLI 发送端最多等 5 分钟，超时即失败。
- 一次发送给 N 台设备，就是 N 份独立的点对点上传，发送端上行带宽会被分担。
- 当前只支持单个文件、无断点续传；大文件传输期间需要保持页面/终端打开。
- 设备列表里的外网端口是 STUN 探测时的 NAT 映射端口，真正传输时每次连接会重新分配，端口可能不同；归属地来自 IP 库，精确到城市，移动网络或代理下可能偏差较大。
- 归属地查询依赖服务器出网访问 `--geo-api`；访问不到时只显示外网地址，不影响传输。
- 浏览器直接发送不会预检接收端剩余磁盘空间，失败会在传输结束时报错。
- 网页接收是临时设备：关闭页面就从设备列表消失，不会写进 `store.json`；每个标签页是独立设备。接收列表只保留最近 12 条，超出后旧条目自动移除。
- 网页接收时，Chrome/Edge 可以选择目录把数据直接写入磁盘；其他浏览器或不选目录时文件会先缓存在内存，超大文件可能失败。接收页面需要一直开着，刷新后重新登录即可继续接收。

## 目录结构

```
cmd/filet          程序入口
internal/server    控制面 HTTP 服务、WebSocket hub、嵌入式网页
internal/client    配置文件与 HTTP/WS 客户端
internal/agent     接收端重连循环与接收逻辑
internal/p2p       WebRTC 连接与 ICE 候选转发
internal/transfer  数据通道分块传输协议与落盘
internal/stunserver 内置 STUN
internal/cli       cobra 命令
deploy             systemd 单元、Caddyfile
scripts            env.sh（独立 Go 工具链）、build.sh（交叉编译）
.github/workflows  CI 与 Release 自动打包
```
