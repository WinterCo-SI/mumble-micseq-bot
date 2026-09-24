# Mumble 麦序机器人

在 Mumble 中实现类似 YY 的「麦序」模式：频道里的人按顺序轮流发言，每人限时，时间到后自动转到指定频道。

## 快速开始

```sh
CGO_ENABLED=0 go build -o micseq-bot.exe .
cp config.example.json config.json   # 修改 server 等字段
./micseq-bot.exe -config config.json
```

首次运行会自动生成客户端证书（`micseq-bot.crt` / `micseq-bot.key`），并记住服务器证书指纹（`server.fingerprint`）。之后如果服务器证书变了，机器人会拒绝连接；确认是正常更换后删除该文件即可。

## Linux 部署

交叉编译出的是静态链接的二进制文件，不依赖 glibc，可以直接在任何 Linux 发行版上运行：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/micseq-bot-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/micseq-bot-linux-arm64 .
```

在服务器上（以 `/opt/micseq-bot` 为例）：

```sh
chmod +x micseq-bot-linux-amd64          # 从 Windows 复制过来的文件没有可执行权限
./micseq-bot-linux-amd64 -config config.json
```

用 systemd 托管，这样进程异常退出或机器重启后都会自动拉起：

```ini
# /etc/systemd/system/micseq-bot.service
[Unit]
Description=Mumble 麦序机器人
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=/opt/micseq-bot
ExecStart=/opt/micseq-bot/micseq-bot-linux-amd64 -config config.json
Restart=always
RestartSec=5
User=micseq

[Install]
WantedBy=multi-user.target
```

证书、指纹和状态文件都写在工作目录里，所以运行用户需要对 `/opt/micseq-bot` 有写权限。机器人收到 SIGTERM（`systemctl stop`）时会先保存状态再退出。

## 启用麦序

在频道名后面加上标记：

```
会议室 [麦序/发言时间: 300秒/下麦转12]
```

- `发言时间` 支持 `300`（不带单位按秒计）、`50s`、`50秒`、`5min`、`10分钟`、`1h`、`1分30秒`、`1分30` 等写法，最长 24 小时。
- `下麦转` 后面是频道 ID。发言结束后，发言人会被移到这个频道。
- 请使用半角冒号 `:`。Mumble 默认的频道名规则不允许全角 `：`（如果服务器放宽了规则，机器人也能识别全角冒号）。
- 删除标记后机器人停止管理该频道，并发消息提示。
- 修改发言时间从下一位开始生效；修改 `下麦转` 立即生效。
- 标记格式写错时，机器人会在频道里提示错误，并继续使用之前的设置。

## 权限配置（必须）

机器人不会修改 ACL，需要管理员手动配置：

| 频道 | 对象 | 权限 |
|---|---|---|
| 麦序频道 | `@all` | **拒绝** Speak（发言） |
| 麦序频道 | 机器人（注册用户或它所在的组） | 允许 MuteDeafen（禁言/耳聋）、Move（移动）、TextMessage（文字消息） |
| 下麦频道 | 机器人，或者被移动的用户 | 机器人有 Move，**或** 用户有 Enter（默认有） |

ACL 规则只能针对注册用户，所以机器人需要先注册：

- 配置 `"register": true`，机器人连上后会自己注册（需要服务器允许 SelfRegister）；或者
- 管理员在客户端里右键机器人选择「注册」。

建议建一个 `micbot` 组，把机器人加进去，再给这个组授权。

接管频道时，如果发现频道里有人没有被禁言，机器人会发一条警告，提醒检查 ACL。

## 工作方式

- **排队顺序**：机器人按观察到的进入顺序排队。机器人上线前就已经在频道里的人（进入顺序未知）排在前面，按显示名称排序（中文按拼音）。主持人等本来就有发言权限的人也照常排队。
- **发言**：机器人解除队首成员的禁言（suppress）并开始计时。时间到后把这个人移到下麦频道，然后轮到下一位。
- **提前离开**：正在发言的人切换到其他频道，会立即轮到下一位。
- **发言人离线**（断线，或服务器重启后还没连回来）：
  - 麦序暂停，最多等待 `speaker_offline_wait`（默认 1 分钟）。等待期间停止计时，其他人也不会拿到发言权。
  - 在这段时间内重新连接并回到频道的，计时继续，并额外补偿 `rejoin_bonus`（默认 30 秒）。
  - 超时仍未回来的，轮到下一位发言。
  - 之后如果在 `rejoin_window`（默认 10 分钟）内回到频道，会插队到当前发言人之后，也就是下一个发言。发言时长是上次剩余的时间加上 30 秒补偿。多个人都这样回来时，按回来的先后排列。
  - 超过 10 分钟才回来的，按普通新成员排到队尾。
- **管理员禁言**：管理员对正在发言的人使用「禁言」或「耳聋」时计时暂停，解除后继续。自己闭麦（self-mute）不会暂停计时。
- **管理员手动解除禁言**：管理员手动解除某个排队成员的禁言后，这个人会被移出队列。之后只要他还在频道里，机器人就不再管他（不排队、不移动、不播报）。他离开频道再回来，会重新排队。
- **提示消息**：
  - 每 `queue_announce_interval` 发一次当前发言人、剩余时间和排队名单。
  - 每 `remaining_announce_interval` 播报一次剩余时间（暂停时不播报）。
  - 剩余 `warn_before` 时，在频道里提醒下一位做好准备，同时给他发私信。
- **ACL 被修改**：Mumble 服务器在任何 ACL 修改后都会重新禁言所有人，其中也包括正在发言的人。机器人会自动重新解除他的禁言。
- **下麦失败**：下麦频道不存在、已满或机器人没有 Move 权限时，改为禁言（mute）该发言人并在频道里提示。这个人离开频道时，机器人会解除这个禁言。
- **持久化**：状态保存在 `state_file` 里，包括队列、当前发言人、剩余时间和被管理员放行的人。机器人重启后接着之前的状态继续，停机期间不计时。
- **SuperUser**：SuperUser 不能被其他人禁言或移动，所以不参与排队。`ignore_users` 里的名字（比如其他机器人）也不参与。

Mumble 服务器默认每秒最多接收 1 条文字消息（突发 5 条），超出的会被**静默丢弃**。`message_rate` / `message_burst` 不要超过服务器的 `messagelimit` / `messageburst`。

## 断线与服务器重启

- **自动重连**：
  - 机器人与服务器断开后会一直重连。重连间隔从 1 秒开始逐次翻倍，最长 30 秒。
  - 服务器静默失联（例如网络中断）时，约 20 秒内会被发现。
  - 如果 gumble 客户端卡死，超过 10 秒会放弃这条连接并重新连接。
- **麦序状态不丢失**：状态保存在内存里，重连后会与服务器上的实际情况重新对照。
- **服务器离线**：
  - 机器人不会崩溃，会一直重连。
  - 已用真实服务器测试过以下情况：正常停止、强杀进程、进程冻结（TCP 连接还在，但服务器不响应），以及机器人启动时服务器还没上线。
  - 一次连接只要稳定保持 10 秒以上，下次断线就重新从 1 秒开始重试。
- **服务器重启**：重启后大家会陆续重新连上。机器人重连后：
  - 当前发言人按上面的「发言人离线」规则处理，1 分钟的等待从机器人重连时开始算。
  - 排队的人保留位置 `rejoin_grace`（默认 90 秒），回来后保持原来的顺序，不按重连先后重新排；超时仍没回来的人会被移出队列。
- **单个用户断线**（服务器正常运行）：
  - 排队的人同样保留位置 `rejoin_grace`。轮到他时如果还没回来，就先跳过他，让后面的人发言；他回来后仍排在前面。
  - 排队名单里会标注「断线，等待重连」。
- **停机期间不计时**：机器人断线期间不扣发言时间。
- **防止私信发错人**：服务器重启后会话 ID 会被重新分配，所以断线前没发出去的私信会被丢弃。频道消息照常发送。

建议用 NSSM、Windows 服务或 systemd 托管机器人进程，这样进程本身异常退出后也能自动拉起。状态文件保证重启后能接着之前的麦序继续。

## 配置项

| 字段 | 默认值 | 说明 |
|---|---|---|
| `server` | — | `host` 或 `host:port`，默认端口 64738 |
| `username` / `password` | `麦序机器人` / 空 | 登录名和服务器密码 |
| `tokens` | 空 | Access tokens |
| `cert_file` / `key_file` | `micseq-bot.crt/.key` | PEM 客户端证书，不存在时自动生成 |
| `register` | `false` | 连接后自行注册 |
| `server_fingerprint` | 空 | 固定的服务器证书 SHA-256 指纹；为空时使用 `trust_file`，首次连接时记住指纹 |
| `home_channel` | `0` | 机器人自己待在哪个频道（ID），0 表示不移动 |
| `state_file` | `micseq-state.json` | 状态文件 |
| `queue_announce_interval` | `60s` | 排队名单播报间隔，`"off"` 表示关闭 |
| `remaining_announce_interval` | `60s` | 剩余时间播报间隔，`"off"` 表示关闭 |
| `warn_before` | `30s` | 提前多久提醒下一位 |
| `rejoin_grace` | `90s` | 断线的排队成员保留位置多久。`"off"` 表示断线立即移出 |
| `speaker_offline_wait` | `1分钟` | 发言人离线时麦序暂停等待多久。`"off"` 表示立即轮到下一位 |
| `rejoin_bonus` | `30s` | 离线的发言人回来后额外补偿的时间 |
| `rejoin_window` | `10分钟` | 发言人离线失去发言权后，多久之内回来可以插队到当前发言人之后 |
| `message_rate` / `message_burst` | `1` / `5` | 文字消息限速 |
| `ignore_users` | 空 | 不参与排队的用户名 |
| `pinyin_sort` | `true` | 中文名按拼音排序 |

时间类字段可以写数字（秒），也可以写 `"90s"`、`"2分钟"` 这样的字符串。

## 开发

```sh
go test ./...
```

端到端测试需要一个真实的 Mumble 服务器：

```sh
docker run -d --name micseq-e2e -p 64799:64738 \
  -e MUMBLE_SUPERUSER_PASSWORD=e2epass -e MUMBLE_CONFIG_AUTOBANATTEMPTS=0 \
  mumblevoip/mumble-server
MICSEQ_E2E_ADDR=127.0.0.1:64799 MICSEQ_E2E_SUPERUSER_PASSWORD=e2epass \
  MICSEQ_E2E_CONTAINER=micseq-e2e go test -tags e2e -v .
```

`TestServerRestart` 和 `TestServerOutages` 会对这个容器执行 `docker restart`、`stop`、`kill`、`pause`，以验证服务器重启或离线后机器人不会崩溃，并且麦序能够恢复。

代码结构：

- `internal/micseq/`：纯逻辑，不依赖 gumble。包括标记和时长解析、状态机（`Manager.Handle` / `Tick` 返回要执行的动作）、消息模板和状态持久化。
- `bot.go`：gumble 适配层。把事件转换给状态机，并执行状态机返回的动作。
- `sender.go`：限速、合并的文字消息发送队列。
- `config.go`、`tlsutil.go`、`main.go`：配置、证书和重连循环。
