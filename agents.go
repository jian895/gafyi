package main

// Connections (agents) of an account: CRUD, webhook push, liveness.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var agentNameRe = regexp.MustCompile(`^[\p{Han}A-Za-z0-9_\-]{1,32}$`)

func validateAgentName(name string) error {
	if !agentNameRe.MatchString(name) {
		return errors.New("名字只能包含中文、字母、数字、下划线和短横线，最长 32 个字符")
	}
	switch strings.ToLower(name) {
	case "all", "hub", "admin":
		return errors.New("这个名字是保留字")
	}
	return nil
}

// AgentInput is what the admin UI submits when creating/configuring a connection.
type AgentInput struct {
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	Mode           string `json:"mode"`
	Webhook        string `json:"webhook"`
	WebhookHeaders string `json:"webhook_headers"` // "Header: value" lines; empty = keep; "-" = clear
}

func applyInput(a *Agent, in AgentInput) error {
	switch in.Mode {
	case "webhook":
		u := strings.TrimSpace(in.Webhook)
		// empty is allowed: created first, configured once the platform issues a URL
		if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return errors.New("Webhook 地址需要以 http:// 或 https:// 开头")
		}
		a.Webhook = u
		switch hs := strings.TrimSpace(in.WebhookHeaders); hs {
		case "":
		case "-":
			a.WebhookHeaders = nil
		default:
			m := map[string]string{}
			for _, line := range strings.Split(hs, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				k, v, ok := strings.Cut(line, ":")
				k, v = strings.TrimSpace(k), strings.TrimSpace(v)
				if !ok || k == "" || strings.ContainsAny(k, " \t") {
					return errors.New("请求头格式应为「名称: 值」，每行一个，例如 Authorization: Bearer xxx")
				}
				m[k] = v
			}
			a.WebhookHeaders = m
		}
	case "link":
		a.Webhook, a.WebhookHeaders = "", nil
	default:
		return errors.New("未知的接入方式")
	}
	a.Mode = in.Mode
	return nil
}

func (h *Hub) createAgent(acc *Account, in AgentInput) (*Agent, error) {
	name := strings.TrimLeft(strings.TrimSpace(in.Name), "@＠")
	if err := validateAgentName(name); err != nil {
		return nil, err
	}
	kind := kindByID(in.Kind)
	if kind == nil {
		kind = kindByID("generic")
	}
	if kind.Mode != "" {
		in.Mode = kind.Mode
	}
	if in.Mode == "" {
		in.Mode = "link"
	}
	a := &Agent{Name: name, Kind: kind.ID, Key: genKey(), Created: time.Now()}
	if err := applyInput(a, in); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if acc.agent(name) != nil {
		return nil, errors.New("这个微信下已经有 @" + name + " 了")
	}
	acc.Agents = append(acc.Agents, a)
	h.attachAgentLocked(acc, a)
	if len(acc.Agents) == 1 {
		acc.Current = a.Name
	}
	h.saveLocked()
	log.Printf("[%s] agent created: %s (%s)", acc.Name, name, kind.ID)
	return a, nil
}

func (h *Hub) updateAgent(acc *Account, name string, in AgentInput) (*Agent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := acc.agent(name)
	if a == nil {
		return nil, errors.New("not found")
	}
	if k := kindByID(a.Kind); k != nil && k.Mode != "" {
		in.Mode = k.Mode // catalog agents have a fixed connection mode
	}
	if in.Mode == "" {
		in.Mode = a.Mode
	}
	cp := *a
	if err := applyInput(&cp, in); err != nil {
		return nil, err
	}
	*a = cp
	h.saveLocked()
	log.Printf("[%s] agent updated: %s (%s)", acc.Name, a.Name, a.Mode)
	return a, nil
}

func (h *Hub) deleteAgent(acc *Account, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, a := range acc.Agents {
		if strings.EqualFold(a.Name, name) {
			acc.Agents = append(acc.Agents[:i], acc.Agents[i+1:]...)
			delete(h.byKey, a.Key)
			if box := acc.boxes[strings.ToLower(a.Name)]; box != nil {
				close(box.wake)
				delete(acc.boxes, strings.ToLower(a.Name))
			}
			h.saveLocked()
			log.Printf("[%s] agent deleted: %s", acc.Name, a.Name)
			return nil
		}
	}
	return errors.New("not found")
}

func (h *Hub) rotateAgentKey(acc *Account, name string) (*Agent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := acc.agent(name)
	if a == nil {
		return nil, errors.New("not found")
	}
	delete(h.byKey, a.Key)
	a.Key = genKey()
	a.FirstSeen = time.Time{}
	h.byKey[a.Key] = a
	h.saveLocked()
	log.Printf("[%s] agent key rotated: %s", acc.Name, a.Name)
	return a, nil
}

// ---------- liveness ----------

type seenInfo struct {
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
}

func (h *Hub) markSeen(a *Agent, ok bool) {
	acc := a.acc
	h.mu.Lock()
	acc.seen[strings.ToLower(a.Name)] = seenInfo{At: time.Now(), OK: ok}
	first := ok && a.FirstSeen.IsZero()
	if first {
		a.FirstSeen = time.Now()
		h.saveLocked()
	}
	owner := acc.owner()
	name := a.Name
	h.mu.Unlock()
	if first {
		log.Printf("[%s] agent first connected: %s", acc.Name, name)
		if owner != "" {
			h.hubReply(acc, owner, "✅ @"+name+" 已首次连接到 Hub，接入打通了。之后 @"+name+" 就能和它对话。")
		}
	}
}

// ---------- webhook push ----------

var webhookClient = &http.Client{Timeout: 60 * time.Second}

func (h *Hub) pushWebhook(acc *Account, a Agent, m InMsg) {
	if a.Webhook == "" {
		h.hubReply(acc, m.from, "@"+a.Name+" 还没有配置 Webhook 地址，这条消息已暂存。请在管理后台点它的「配置」填写。")
		return
	}
	link := h.cfg.PublicURL + "/a/" + a.Key
	payload, _ := json.Marshal(map[string]any{
		"source":      "weixin-hub", // wire identifier kept from the old name: deployed agents filter on it
		"id":          m.ID,
		"text":        m.Text,
		"at":          m.At,
		"agent":       a.Name,
		"direct":      m.Direct,
		"attachments": m.Attachments,
		"reply_url":   link + "/send",
		"instruction": "这是主人刚在微信里发给 @" + a.Name + " 的消息（字段 text）。请处理它，然后用微信 MCP 工具 send_wechat_message 回复主人，" +
			"或 POST {\"text\":\"回复内容\"} 到 reply_url。同一个 id 只处理一次。" +
			"如果有 attachments（主人发来的图片/文件/视频/语音），GET 每个 url 下载原文件。要发图片或文件给主人，用 send_wechat_file。",
	})
	ptr := h.agentByKey(a.Key) // live agent, for markSeen
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", a.Webhook, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if len(a.WebhookHeaders) == 0 {
			req.Header.Set("Authorization", "Bearer "+a.Key)
		}
		for k, v := range a.WebhookHeaders {
			req.Header.Set(k, v)
		}
		resp, err := webhookClient.Do(req)
		if err != nil {
			log.Printf("[%s] webhook %s: %v", acc.Name, a.Name, err)
			time.Sleep(time.Duration(i+1) * 3 * time.Second)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
			log.Printf("[%s] webhook %s: http %d %s", acc.Name, a.Name, resp.StatusCode, trunc(string(raw), 200))
			if ptr != nil {
				h.markSeen(ptr, false)
			}
			h.enqueue(acc, a.Name, a.label(), m.from, fmt.Sprintf("⚠️ Webhook 拒绝了这条消息（HTTP %d）：%s。请检查后台里的 Webhook 地址和认证请求头。", resp.StatusCode, trunc(strings.TrimSpace(string(raw)), 120)))
			return
		}
		if resp.StatusCode >= 300 {
			log.Printf("[%s] webhook %s: http %d", acc.Name, a.Name, resp.StatusCode)
			time.Sleep(time.Duration(i+1) * 3 * time.Second)
			continue
		}
		if ptr != nil {
			h.markSeen(ptr, true)
		}
		// optional synchronous reply: only an explicit JSON {"reply": "..."} counts
		var r struct {
			Reply string `json:"reply"`
		}
		if json.Unmarshal(raw, &r) == nil && strings.TrimSpace(r.Reply) != "" {
			h.enqueue(acc, a.Name, a.label(), m.from, strings.TrimSpace(r.Reply))
		}
		return
	}
	if ptr != nil {
		h.markSeen(ptr, false)
	}
	h.enqueue(acc, a.Name, a.label(), m.from, "⚠️ 消息推送到 Webhook 失败（已重试 3 次），请检查智能体平台是否正常。")
}
