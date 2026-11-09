# G@ 设计文档

> 一个微信，连接你所有的智能体。

## 1. 现状梳理

| 维度 | 现状（v0.1） | 问题 |
|---|---|---|
| 微信账号 | 全局只有一份 `Creds` / `Cursor` / `Convs`，一个轮询协程 | 无法同时绑定多个微信 |
| 智能体 | 全局一份 `agents.json`，名字全局唯一 | 无法按微信划分，A 微信的 @grok 和 B 微信的 @grok 冲突 |
| 路由 | 「当前对话」是全局的 | 多个微信会互相串台 |
| 发送 | 全局一个发送队列，收件人默认为唯一的 owner | 智能体不知道应该发往哪个微信 |
| 存储 | `state.json` + `agents.json` 分散保存 | 两份文件需要一起迁移，一致性差 |
| 代码 | 遗留「OpenAI 接口直连大模型」、对话记忆、输入状态等代码 | 已确认无用，增加维护成本 |
| 前端 | 单页，状态、扫码、连接、指令全部堆在一起 | 没有「微信」这一层，无法扩展 |
| 开源 | 没有 LICENSE、部署文件和通用说明；`public_url` 等配置写死为本机 | 别人无法直接部署 |

## 2. 核心概念

```
Hub（一台服务器上的一个实例）
 └── 微信账号 Account（扫码绑定的一个 ClawBot，可以有多个）
      ├── 凭据、消息游标、推送通道（context_token）
      ├── 当前对话：不带 @ 的消息发给谁
      └── 连接 Agent（一个智能体，可以有多个）
           ├── 类型 kind：grokbot / muse / openclaw / hermes / generic …
           ├── 接入方式 mode：webhook（Hub 推送）/ link（智能体拉取）
           └── 专属链接 /a/<key>：既是身份，也是密钥
```

- 每个**微信账号相互隔离**：各自有轮询、发送队列、当前对话和连接列表。
- **连接属于某一个微信账号**。名字只需要在该账号内唯一，不同微信可以各有一个 @grok。
- 专属链接的 key 全局唯一。Hub 通过 key 就能确定「是哪个微信下的哪个智能体」，所以智能体发出的消息自动回到对应的微信。
- 同一个微信重复扫码时，不会新建账号，而是更新已有账号的凭据（按 `ilink_user_id` 去重）。

## 3. 消息流

```
入站：微信 A ──getupdates──▶ 账号 A 的轮询 ──路由(@名字 / 当前对话)──▶ 账号 A 的连接
                                                          ├─ webhook：POST 推送（异步）
                                                          └─ link：进入收件箱，等智能体长轮询来取

出站：智能体 ──/a/<key>/send 或 MCP──▶ 按 key 找到所属账号 ──该账号的发送队列(限速、分片)──▶ 微信 A
```

路由规则（每个账号独立）：
- `@名字 内容`：发给该连接，并把它切换为当前对话（粘性）；只发 `@名字` 则只切换。
- 不带 @：发给当前对话。
- `@all`：发给该账号下的所有连接，不切换当前对话。
- `/use 名字`、`/list`、`/ping`：Hub 指令。
- 出站消息不经过路由，任何连接都可以随时发送，前缀为 `[名字]`。

## 4. 存储

单文件 `data_dir/hub.json`，权限 0600，每次修改原子写入（先写临时文件再 rename）：

```json
{
  "version": 2,
  "accounts": [
    {
      "id": "a1b2c3d4", "name": "我的微信", "created": "…",
      "creds": { "token": "…", "base_url": "…", "bot_id": "…", "user_id": "…" },
      "cursor": "…",
      "convs": { "<user_id>": { "context_token": "…", "last_inbound": "…", "sends_since": 0 } },
      "current": "grok",
      "agents": [ { "name": "grok", "key": "…", "kind": "grokbot", "mode": "webhook", "webhook": "…", "webhook_headers": {…} } ]
    }
  ]
}
```

**迁移**：首次启动时，如果没有 `hub.json`，但有 v0.1 的 `state.json` / `agents.json`，就把它们合并成一个名为「我的微信」的账号，原文件改名为 `*.v1.bak` 保留。迁移后已有的专属链接（key）保持不变，智能体侧不需要任何改动。

## 5. 管理后台信息架构

```
登录（admin_token）
└── 首页 · 我的微信
     ├── 账号卡片：名称、在线状态、当前对话、连接数量（智能体标签）、最近消息时间
     ├── ＋ 绑定微信 → 弹窗：命名 → 扫码 → 配对码（如需要）→ 完成后进入账号页
     └── 空状态：三步引导（绑定微信 → 新建连接 → 在微信里 @它）
└── 账号页 · <微信名称>
     ├── 顶部：返回、名称（可改）、状态
     ├── 状态卡：推送通道、当前对话、最近收到、已连续推送
     ├── 连接：＋ 新建连接（选智能体类型 → 命名）→ 卡片（专属步骤 / 话术 / 配置 / 测试 / 切换 / 删除）
     ├── 微信里的指令（可折叠）
     └── 设置：重新扫码、发送测试消息、解绑此微信（危险操作）
```

前端仍是零构建的单个 HTML 文件，用 hash 路由（`#/`、`#/a/<id>`），整个程序仍是一个二进制文件。

## 6. 接口

管理接口（`Authorization: Bearer <admin_token>`）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/accounts` | 账号列表及摘要 |
| POST | `/admin/api/accounts` | 新建账号并开始扫码 `{name}` |
| GET / PATCH / DELETE | `/admin/api/accounts/{id}` | 账号详情 / 改名 / 解绑 |
| POST | `/admin/api/accounts/{id}/login` · `/verify` · `/test` | 重新扫码 / 提交配对码 / 测试消息 |
| GET / POST | `/admin/api/accounts/{id}/agents` | 连接列表 / 新建 |
| PUT / DELETE | `/admin/api/accounts/{id}/agents/{name}` | 配置 / 删除 |
| POST | `/admin/api/accounts/{id}/agents/{name}/rotate` · `/current` · `/test` | 重置链接 / 切换为当前对话 / 测试 |
| GET | `/admin/api/catalog` | 可接入的智能体类型 |

智能体接口保持不变：`/a/<key>`（说明）、`/send`、`/messages`、`/status`、`/mcp`，以及 Bearer 认证形式的 `/v1/*`。

## 7. 本次删除的内容

- `mode=api`（Hub 直接调用大模型）、对话记忆 `/reset`、正在输入状态：已确认不符合「接入智能体」的定位。
- 旧版的全局 `default_agent`：由每个账号的 `current` 取代。

## 8. 开源工程化

- `LICENSE`（MIT）、`README.md`（中文为主，附英文简介）、`config.example.json`
- `deploy/`：systemd 服务文件、Caddy 反向代理示例、Dockerfile
- `Makefile`：`build` / `build-all`（交叉编译）/ `run`
- 声明：本项目与腾讯无关，所用 iLink Bot 协议参考自 MIT 许可的 Tencent/openclaw-weixin
