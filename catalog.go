package main

import "strings"

// Agent catalog: every supported agent type, with the connection mode it uses,
// the steps shown to the owner, and the tailored prompt to hand to the agent.
// To support a new agent, add an entry here. Placeholders: {name} {link}.

type AgentKind struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Vendor  string   `json:"vendor"`
	Desc    string   `json:"desc"`
	Suggest string   `json:"suggest"` // suggested @name
	Logo    string   `json:"logo"`    // file in assets/logos/
	Mode    string   `json:"mode"`    // "webhook" | "link" | "" (generic: owner chooses)
	Steps   []string `json:"steps"`
	Prompt  string   `json:"-"`
}

var catalog = []AgentKind{
	{
		ID: "grokbot", Title: "Grok Bot", Vendor: "xAI · 云端托管", Suggest: "grok", Logo: "grokbot.png", Mode: "webhook",
		Desc: "Grok 的例行任务支持 Webhook 唤醒：Hub 推送消息 → 唤醒任务 → 它通过微信 MCP 回复。",
		Steps: []string{
			"复制专属话术发给 Grok，它会连上微信 MCP 并创建「Webhook 例行任务」",
			"在 Grok 的任务面板里找到该任务的 Webhook URL 和发送密钥（不要贴到聊天里）",
			"点本卡片「配置」，填入 Webhook 地址和认证请求头，保存后点「测试」",
		},
		Prompt: `这是你在我的「微信中继 Hub」上的专属通道，你在微信里的名字是 @{name}。下面的链接本身就是密钥，请勿公开或写入日志：
{link}

请完成以下配置：

1. 连接微信 MCP：把 {link}/mcp 添加为 MCP 服务器（Streamable HTTP，不需要额外认证头）。它提供 send_wechat_message、send_wechat_file、read_wechat_messages、wechat_status 四个工具。先用 wechat_status 检查连通。

2. 创建一个「Webhook 触发的例行任务」，专门处理我从微信发给你的消息：
   - 触发时请求体是 JSON：{"source":"weixin-hub","id":数字,"text":"我在微信里说的话","at":"时间","agent":"{name}","direct":true,"reply_url":"…"}
   - 把 text 当成我在微信里对你说的话，像平时聊天一样处理（可以使用你的全部能力），然后用 send_wechat_message 回复我
   - 如果请求体里有 attachments（我发来的图片/文件），GET 每个 url 下载后再处理；要发图片或文件给我，用 send_wechat_file
   - 同一个 id 只处理一次；source 不是 weixin-hub 的请求忽略
   - 只用纯文本（微信不渲染 Markdown），不用署名（Hub 会自动加 [{name}]）；不要直接发网盘分享链接（夸克、百度、阿里等，微信会拦截），Hub 会自动改写成 pan[.]quark[.]cn/s/… 的形式
   - 预计超过 1 分钟的任务，先用 send_wechat_message 发一句「收到，处理中」
   - 本任务已授权：在回复我的微信消息时使用 send_wechat_message

3. 完成后告诉我：Webhook URL 在任务面板的哪里，认证需要哪个请求头、值的格式是什么。不要把密钥本身写在聊天里。`,
	},
	{
		ID: "muse", Title: "Muse", Vendor: "Meta · 云端托管", Suggest: "muse", Logo: "muse.svg", Mode: "link",
		Desc: "Muse 有常驻的云端 Linux：桥接长轮询 Hub，常驻 worker 连续处理，唤醒钩子兜底。简单问题约 10 秒回复。",
		Steps: []string{
			"复制专属话术发给 Muse，它会在云端 Linux 部署桥接、常驻 worker 和兜底钩子",
			"等 Muse 汇报三者都在运行，并收到它发来的测试消息",
			"在微信里发 @{name} 你好 验证（简单问题约 10 秒回复）",
		},
		Prompt: `我已经有一个「微信中继 Hub」统一管理我的微信 ClawBot。请不要直接连接腾讯 iLink / ClawBot，也不要生成登录二维码，否则会和 Hub 抢同一个微信会话。

请改为通过 Hub 收发微信消息。你在微信里的名字是 @{name}，你的专属链接是（链接本身就是密钥，不要写进日志或公开内容）：
{link}
先 GET 这个链接阅读完整的接入说明。

目标：简单问题 15 秒内回复。架构分三部分：桥接只负责收消息，常驻 worker 负责处理，唤醒钩子只做兜底。

1. 桥接脚本 ~/workspace/wechat-bridge/hub_bridge.py（只收不处理）
   - 循环 GET {link}/messages?after=<cursor>&wait=30（长轮询，有新消息立即返回）
   - 拿到消息后立刻把返回的 next 持久化为新 cursor，并马上发起下一次长轮询，不要等消息处理完
   - 每条消息按 id 原子写入 inbox/<id>.json（先写临时文件再 rename），已存在的 id 跳过
   - 消息带 attachments（我发来的图片/文件/语音）时，在后台线程里 GET 每个 url，下载到 inbox/<id>/，再把本地路径回写进消息文件；下载不能阻塞长轮询
   - 网络出错时退避重试（5 秒、10 秒、30 秒）
   - 用 systemd / supervisor / 带自动重启的 nohup 保持常驻，崩溃或重启后自动拉起

2. 常驻 worker（主处理路径，不要每条消息都冷启动）
   - 保持一个常驻的 worker 和同一个对话会话，所有微信消息都进这个会话，上下文连贯
   - 每 2 秒扫一次 inbox（或由桥接直接通知），把消息原子移动到 running/ 来认领，同一个 id 只处理一次
   - 快路径：寒暄、事实性提问、简短问答直接回答，不先调用工具、不先查记忆；只有确实需要时才查
   - 预计超过 20 秒的任务（查资料、跑命令、访问其他设备），先 POST {link}/send 一句「收到，处理中」，做完再发结果
   - 有附件时，先看 inbox/<id>/ 里已下载好的文件
   - 回复：POST {link}/send，请求体 {"text": "回复内容"}
   - 发图片或文件：curl -F "file=@本机路径" -F "text=说明" {link}/send；不要把本机路径当文字发出去（我收到的只是乱码）

3. 唤醒钩子（兜底）
   - 每 5 秒检查一次：如果 inbox 里有超过 10 秒没被认领的消息，就用原来的方式唤醒处理，保证消息不丢
   - 每次触发时检查常驻 worker 是否还活着，不在就自动重新拉起，并 /send 给我一句「常驻 worker 已重启」（虚拟机被替换后能自动恢复快速模式）

4. 回复规则
   - 只用纯文本（微信不渲染 Markdown），不用署名（Hub 会自动加 [{name}]）
   - 不要直接发网盘分享链接（夸克、百度、阿里等，微信会拦截），Hub 会自动改写成 pan[.]quark[.]cn/s/… 的形式
   - 不要刷屏，能合并成一条就合并

5. 测速并汇报
   - 给每条消息记录时间点（只写日志，不要发给我）：t1 长轮询拿到消息、t3 worker 开始处理、t4 开始调用模型、t5 /send 完成
   - 配置完成后向我汇报：桥接、常驻 worker、兜底钩子是否都在运行；然后请我从微信发 3 条简单消息，回复后告诉我每条的分段耗时
   - 最后用 /send 给我发一条测试消息`,
	},
	{
		ID: "openclaw", Title: "OpenClaw", Vendor: "开源 · 自部署", Suggest: "claw", Logo: "openclaw.svg", Mode: "link",
		Desc: "在 OpenClaw 所在机器上跑一个桥接：从 Hub 取消息 → 交给本机 Gateway 的智能体处理 → 回复。无需公网端口。",
		Steps: []string{
			"复制专属话术发给 OpenClaw（在它的任意聊天渠道或控制台里）",
			"它会在自己的机器上部署常驻桥接，并把「发微信」封装成技能",
			"收到它的测试消息后，在微信里发 @{name} 你好 验证",
		},
		Prompt: `我有一个「微信中继 Hub」统一管理我的微信 ClawBot，你在微信里的名字是 @{name}。请不要启用 openclaw-weixin 插件、不要扫码登录腾讯 iLink，否则会和 Hub 抢同一个微信会话。

你的专属链接（本身就是密钥，不要写进日志或公开内容）：
{link}
先 GET 这个链接阅读完整的接入说明。

请在你运行的这台机器上完成配置：

1. 常驻桥接脚本（Node 或 Python 都可以）：
   - 循环 GET {link}/messages?after=<cursor>&wait=30（长轮询），cursor 取返回的 next 并持久化到文件
   - 每条消息交给你自己（OpenClaw 智能体）处理，二选一：
     a) 本机 Gateway 的 OpenAI 兼容接口：POST http://127.0.0.1:18789/v1/chat/completions，Authorization: Bearer <网关 token>，model 用 "openclaw/default"（或指定 agent），并固定 "user": "weixin-{name}"，让微信对话始终落在同一个会话里。该接口默认关闭，需要设置 gateway.http.endpoints.chatCompletions.enabled = true。
     b) 不方便开启接口时，用 openclaw agent 命令行处理。
   - 把回复 POST {link}/send，请求体 {"text": "回复内容"}
   - 消息带 attachments（我发来的图片/文件/语音）时，GET 每个 url 下载原文件再交给智能体处理
   - 要发图片或文件给我：curl -F "file=@本机路径" -F "text=说明" {link}/send（不要把本机路径当文字发出去，我收到的只是乱码）
   - 只用纯文本，不用署名（Hub 会自动加 [{name}]）；不要直接发网盘分享链接（夸克、百度、阿里等，微信会拦截），Hub 会自动改写成 pan[.]quark[.]cn/s/… 的形式；超过 1 分钟的任务先发「收到，处理中」；同一个 id 只处理一次；出错时退避重试
   - 用 systemd / pm2 等保持常驻并自动重启

2. 主动发消息：把 POST {link}/send 封装成一个技能/工具，让你在定时任务、提醒等场景里也能主动给我发微信。

3. 完成后汇报桥接进程状态，并通过 /send 给我发一条测试消息。`,
	},
	{
		ID: "hermes", Title: "Hermes Agent", Vendor: "Nous Research · 自部署", Suggest: "hermes", Logo: "hermes.png", Mode: "link",
		Desc: "在 Hermes 所在机器上跑桥接，调用本机 API Server 并保持会话；同时接入 Hub 的 MCP，用于主动发消息。",
		Steps: []string{
			"复制专属话术发给 Hermes（在它的任意聊天渠道或终端里）",
			"它会接入 Hub 的 MCP，并在自己的机器上部署常驻桥接",
			"收到它的测试消息后，在微信里发 @{name} 你好 验证",
		},
		Prompt: `我有一个「微信中继 Hub」统一管理我的微信 ClawBot，你在微信里的名字是 @{name}。请不要启用 Hermes 自带的微信（WeChat）网关、不要扫码登录，否则会和 Hub 抢同一个微信会话。

你的专属链接（本身就是密钥，不要写进日志或公开内容）：
{link}
先 GET 这个链接阅读完整的接入说明。

请在你运行的这台机器上完成配置：

1. 接入 MCP（用于主动发消息）：把 {link}/mcp 添加为 MCP 服务器（Streamable HTTP，无需额外认证头）。它提供 send_wechat_message、send_wechat_file、read_wechat_messages、wechat_status。用 wechat_status 检查连通。

2. 常驻桥接脚本：
   - 循环 GET {link}/messages?after=<cursor>&wait=30（长轮询），cursor 取返回的 next 并持久化到文件
   - 每条消息交给你自己（Hermes Agent）处理：调用本机 API Server，POST http://127.0.0.1:8642/v1/chat/completions，Authorization: Bearer <API_SERVER_KEY>，并带请求头 X-Hermes-Session-Id: weixin-{name}，让微信对话始终落在同一个会话里。如果 API Server 没开启，在 ~/.hermes/.env 设置 API_SERVER_ENABLED=true 和 API_SERVER_KEY，然后重启 hermes gateway。
   - 把回复 POST {link}/send，请求体 {"text": "回复内容"}
   - 消息带 attachments（我发来的图片/文件/语音）时，GET 每个 url 下载原文件再交给智能体处理
   - 要发图片或文件给我：curl -F "file=@本机路径" -F "text=说明" {link}/send（不要把本机路径当文字发出去，我收到的只是乱码）
   - 只用纯文本，不用署名（Hub 会自动加 [{name}]）；不要直接发网盘分享链接（夸克、百度、阿里等，微信会拦截），Hub 会自动改写成 pan[.]quark[.]cn/s/… 的形式；超过 1 分钟的任务先发「收到，处理中」；同一个 id 只处理一次；出错时退避重试
   - 用 systemd 等保持常驻并自动重启

3. 完成后汇报桥接进程状态，并用 send_wechat_message 给我发一条测试消息。`,
	},
	{
		ID: "generic", Title: "通用智能体", Vendor: "自行适配", Suggest: "", Logo: "generic.svg", Mode: "",
		Desc: "其他任何能调用 HTTP 的智能体或程序。可选：它自己来取消息（链接拉取），或 Hub 推送到它的 Webhook。",
		Steps: []string{
			"复制专属话术发给智能体，它会读取链接里的接入说明自行适配",
			"如果是 Webhook 方式：拿到它的 Webhook 地址后，点「配置」填入",
			"在微信里发 @{name} 你好 验证",
		},
		Prompt: `这是你专属的微信消息通道，你在微信里的名字是 @{name}。请先打开下面的链接，阅读并遵循其中的接入说明。之后你就可以通过它给我的微信发消息，也能收到我在微信里 @{name} 发给你的消息。链接本身就是密钥，请不要公开。

{link}`,
	},
}

func kindByID(id string) *AgentKind {
	for i := range catalog {
		if catalog[i].ID == id {
			return &catalog[i]
		}
	}
	return nil
}

func renderTpl(s, name, link string) string {
	return strings.NewReplacer("{name}", name, "{link}", link).Replace(s)
}
