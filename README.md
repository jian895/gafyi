<p align="center"><img src="assets/brand/ga-mark.svg" width="96" alt="ga"></p>

<h1 align="center">ga<sub>.fyi</sub></h1>

<p align="center"><b>@ 谁，谁就回你。</b><br>把一个微信号，接到你所有的 AI 智能体。</p>

在微信里发 `@grok 查一下库存`、`@muse 画张图`、`@hermes 总结今天的邮件`，消息就会路由到对应的智能体，它们的回复（以及定时任务、提醒等主动消息）会自动回到你的微信。

> English: **ga** (Gateway for Agents) is a self-hosted gateway for WeChat. It holds your WeChat ClawBot session(s) and routes messages to many AI agents (Grok Bot, Muse, OpenClaw, Hermes, or anything that speaks HTTP). Type `@agent` in WeChat to talk to an agent; every agent can message you back through its personal link.

```
                       ┌──────────────────── ga ────────────────────┐
 微信 A ⇄ ClawBot ⇄ iLink │  账号 A：@grok（Webhook） @muse（链接拉取）   │ ⇄ 智能体
 微信 B ⇄ ClawBot ⇄ iLink │  账号 B：@claw  @hermes                     │
                       └─────────────────────────────────────────────┘
```

### 名字的由来

- **ga** 是 **G**ateway for **A**gents，智能体网关；中文读作「嘎」，消息落地的那一声。
- **.fyi** 是 for your information，给你捎个信。
- 标志是一个 @：@ 是 ga 唯一的用法，而 @ 的中心本来就是一个 a。外圈不闭合，缺口里的金点是刚送出的消息。
- 粤语句末的「㗎」也是「跟你说一声」的语气。

## 为什么需要它

微信的 ClawBot（iLink Bot 协议）一次只能接一个程序：谁在轮询，消息就归谁。ga 独占这个会话，再把消息分发给多个智能体：

- **多个微信**：可以绑定多个微信，每个微信各自管理自己的智能体。
- **多个智能体**：用 `@名字` 切换对话；切换是「粘性」的，之后不带 @ 的消息都继续发给它。
- **双向通信**：任何智能体都能随时给你发消息，前缀为 `[名字]`。
- **图片和文件**：收发图片、视频、文件；你发的语音会附带微信的转写文字。
- **按智能体定制接入**：内置 Grok Bot、Muse、OpenClaw、Hermes 的专属接入话术，另有通用方式。
- **两种接入方式**：Webhook（ga 推送给智能体）或链接拉取（智能体长轮询）。智能体不需要公网 IP。
- **轻量**：单个 Go 二进制，没有任何依赖，常驻内存约 10 MB。

## 快速开始

```bash
# 1. 编译（需要 Go 1.22+），也可以用 make build-all 交叉编译
make build
sudo install -m 755 gafyi /usr/local/bin/

# 2. 配置
sudo mkdir -p /etc/gafyi
sudo cp config.example.json /etc/gafyi/config.json
gafyi genkey        # 生成 admin_token，填进 config.json
# 把 public_url 改成你的对外地址（智能体的专属链接会用到）

# 3. 以 systemd 服务运行
sudo useradd --system --no-create-home --shell /usr/sbin/nologin gafyi
sudo mkdir -p /var/lib/gafyi && sudo chown gafyi: /var/lib/gafyi
sudo chown root:gafyi /etc/gafyi/config.json && sudo chmod 640 /etc/gafyi/config.json
sudo cp deploy/gafyi.service /etc/systemd/system/
sudo systemctl enable --now gafyi
```

然后用 HTTPS 反向代理把服务暴露出去，参考 [deploy/Caddyfile.example](deploy/Caddyfile.example)，再打开 `<public_url>/admin`。

Docker 方式：`docker build -t gafyi .`，再把配置文件挂载到 `/config.json`，数据目录挂载到 `/data`。

## 使用

1. **绑定微信**：在首页点「绑定微信」→ 起名 → 用手机微信扫码。首次使用需要先在微信「我 → 设置 → 插件」里开启 ClawBot。然后在微信里给 ClawBot 发一句话，激活推送通道。
2. **新建连接**：进入这个微信的页面 →「新建连接」→ 选择智能体类型 → 起名（比如 `grok`）。
3. **完成接入**：照卡片上的步骤，把「专属话术」发给对应的智能体。它首次连上时，你的微信会收到「✅ @grok 已首次连接」。
4. **对话**：在微信里发 `@grok 你好`。

### 微信里的指令

| 输入 | 作用 |
|---|---|
| `@名字 内容` | 切换到该智能体并发送；之后不带 @ 的消息都发给它 |
| `@名字` | 只切换，不发内容 |
| 直接打字 | 发给当前对话的智能体 |
| `@all 内容` | 发给这个微信下的所有智能体（不切换） |
| `/use 名字` · `/list` · `/ping` | 切换 · 列出智能体 · 刷新推送通道 |

## 支持的智能体

| 类型 | 接入方式 | 原理 |
|---|---|---|
| Grok Bot（xAI） | Webhook | ga 推送消息，唤醒 Grok 的例行任务；Grok 通过 ga 的 MCP 回复 |
| Muse（Meta） | 链接拉取 | Muse 在它的云端 Linux 运行桥接脚本，长轮询 ga，再用唤醒钩子处理 |
| OpenClaw | 链接拉取 | 桥接程序运行在 OpenClaw 所在机器上，调用本机 Gateway（`/v1/chat/completions`，并固定 `user` 以保持会话） |
| Hermes Agent | 链接拉取 + MCP | 桥接调用本机 API Server（带 `X-Hermes-Session-Id`），主动发消息走 MCP |
| 通用 | 任选一种 | 智能体读取专属链接里的接入说明，自行适配 |

新增一种智能体：在 [catalog.go](catalog.go) 里加一条，写好它的接入方式、步骤和专属话术即可。

## 智能体接口

每个连接都有一条专属链接 `<public_url>/a/<key>`。这条链接就是密钥，下面的接口都不需要额外认证：

| 方法和路径 | 说明 |
|---|---|
| `GET  <link>` | 接入说明（Markdown），智能体打开就能看懂怎么接 |
| `POST <link>/send` `{"text":"…"}` | 给主人发微信（也支持 `GET ?text=`） |
| `POST <link>/send` 带文件 | 发图片/视频/文件：multipart `file=@本机文件`，或 JSON `file_url` / `file_base64`+`filename`，单个最大 25 MB |
| `GET  <link>/media/<id>` | 下载主人发来的图片/文件/视频/语音（消息里的 `attachments[].url`） |
| `GET  <link>/messages?after=<next>&wait=30` | 长轮询，读取主人发来的消息 |
| `GET  <link>/status` | 推送通道是否可用 |
| `<link>/mcp` | MCP 服务（Streamable HTTP），提供 `send_wechat_message`、`send_wechat_file`、`read_wechat_messages`、`wechat_status` |

Webhook 方式下，ga 会 POST 如下 JSON：

```json
{"source":"weixin-hub","id":123,"text":"…","at":"…","agent":"grok","direct":true,"attachments":[{"id":"…","type":"image","url":"<link>/media/…"}],"reply_url":"<link>/send","instruction":"…"}
```

> `source` 字段的值保留为旧名 `weixin-hub`，因为已经接入的智能体会用它来过滤请求。


智能体可以异步回复：调用 MCP，或者 POST 到 `reply_url`。如果想同步回复，就在响应体里返回 `{"reply":"…"}`。认证请求头可以在后台按平台要求设置。

## 配置

| 字段 | 默认值 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:8787` | 监听地址 |
| `data_dir` | `/var/lib/gafyi` | 数据目录，数据保存在其中的 `hub.json`，权限 0600 |
| `admin_token` | — | 管理后台口令，至少 16 位 |
| `public_url` | — | 对外访问地址，用来生成智能体的专属链接 |
| `send_interval_ms` | `1200` | 每个微信两条出站消息之间的最小间隔 |
| `warn_after_sends` | `8` | 连续推送多少条后，提醒你回复一句以保持通道 |

## 已知限制

- 媒体协议（AES-128-ECB 加密 + 微信 CDN）参考自 openclaw-weixin，没有官方文档。发出的语音会以文件形式发送。主人发来的媒体 Hub 不落盘，只保存 CDN 引用，智能体下载时实时解密；引用保留 7 天，重启后失效。
- `file_url` 只能是公网地址，ga 不会访问内网、本机地址。
- 智能体回复里的网盘分享链接会被自动改写成 `pan[.]quark[.]cn/…` 的形式，避免触发微信拦截。
- 主动推送依赖你最近一次发给 ClawBot 的消息里带的 `context_token`。它的有效期和条数限制没有官方文档。长时间不互动后，回复一句或发 `/ping` 即可恢复。
- 接口返回 `accepted` 只表示微信服务器接受了，不保证手机一定收到。
- 收件箱保存在内存中（每个连接保留最近 500 条），服务重启后清空。
- 专属链接里含有密钥，反向代理的访问日志可能会记录完整链接。

## 免责声明

本项目与腾讯公司无关，也未获得其认可。所用的 iLink Bot 协议参考自 MIT 许可的 [Tencent/openclaw-weixin](https://github.com/Tencent/openclaw-weixin)，相关接口可能随时变化。请遵守微信的使用条款，风险自负。

Grok、Muse、OpenClaw、Hermes 等名称和 logo 归各自所有者所有，本项目仅用于标识对应的智能体，不代表获得其认可（来源见 [assets/logos](assets/logos/README.md)）。

## 许可证

[MIT](LICENSE)
