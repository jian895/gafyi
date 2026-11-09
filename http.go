package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed admin.html
var adminHTML []byte

//go:embed assets/logos/*.png assets/logos/*.svg assets/brand/*.svg
var assetsFS embed.FS

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func bearer(r *http.Request) string {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return r.URL.Query().Get("key")
}

func readJSON(r *http.Request, v any) {
	json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(v)
}

func (h *Hub) routes() http.Handler {
	mux := http.NewServeMux()

	// ================= agent API =================
	sendH := h.withAgent(func(w http.ResponseWriter, r *http.Request, a *Agent) {
		var text string
		var media *OutMedia
		switch r.Method {
		case http.MethodGet:
			text = r.URL.Query().Get("text")
		case http.MethodPost:
			ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
			switch ct {
			case "multipart/form-data":
				if err := r.ParseMultipartForm(8 << 20); err != nil {
					errJSON(w, 400, "bad multipart body (max 25 MB file): "+err.Error())
					return
				}
				text = firstNonEmpty(r.FormValue("text"), r.FormValue("caption"))
				if f, fh, err := r.FormFile("file"); err == nil {
					data, _ := io.ReadAll(io.LimitReader(f, maxMediaBytes+1))
					f.Close()
					m, err := newOutMedia(data, fh.Filename, r.FormValue("as"))
					if err != nil {
						errJSON(w, 400, err.Error())
						return
					}
					media = m
				} else if u := firstNonEmpty(r.FormValue("file_url"), r.FormValue("image_url")); u != "" {
					m, err := mediaFromRequest(r.Context(), sendFileArgs{FileURL: u, Filename: r.FormValue("filename"), As: r.FormValue("as")})
					if err != nil {
						errJSON(w, 400, err.Error())
						return
					}
					media = m
				}
			default:
				body, err := io.ReadAll(r.Body)
				if err != nil {
					errJSON(w, 413, "request too large (max 25 MB file)")
					return
				}
				var req sendFileArgs
				if json.Unmarshal(body, &req) == nil {
					text = firstNonEmpty(req.Text, req.Caption)
					if req.hasFile() {
						m, err := mediaFromRequest(r.Context(), req)
						if err != nil {
							errJSON(w, 400, err.Error())
							return
						}
						media = m
					}
				} else if ct == "application/x-www-form-urlencoded" {
					vals, _ := url.ParseQuery(string(body))
					text = vals.Get("text")
				} else {
					text = string(body) // plain-text body
				}
			}
		default:
			errJSON(w, 405, "use POST (or GET ?text=)")
			return
		}
		if media != nil {
			h.respondErr(w, h.enqueueMedia(a.acc, a.Name, a.label(), text, media))
			return
		}
		if strings.TrimSpace(text) == "" {
			errJSON(w, 400, `text is empty: POST {"text": "..."} (or send a file: file_url / file_base64 / multipart "file")`)
			return
		}
		h.respondSend(w, a.acc, a.Name, a.label(), text)
	})
	msgsH := h.withAgent(func(w http.ResponseWriter, r *http.Request, a *Agent) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		writeJSON(w, 200, h.inboxResult(r, a, after, wait))
	})
	statusH := h.withAgent(func(w http.ResponseWriter, r *http.Request, a *Agent) {
		s := h.accountStatus(a.acc)
		writeJSON(w, 200, map[string]any{"agent": a.Name, "wechat": s.Name, "logged_in": s.LoggedIn, "online": s.Online,
			"can_send": s.LoggedIn && s.HasContext, "last_inbound": s.LastInbound, "sends_since_last_inbound": s.SendsSince})
	})
	docH := h.withAgent(func(w http.ResponseWriter, r *http.Request, a *Agent) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(agentDoc(h.baseURL(r), a)))
	})
	mcpH := h.withAgent(h.handleMCP)

	// link form: everything under the agent's personal link /a/<key>
	mux.HandleFunc("/a/{key}", docH)
	mux.HandleFunc("/a/{key}/", docH)
	mux.HandleFunc("/a/{key}/send", sendH)
	mux.HandleFunc("/a/{key}/messages", msgsH)
	mux.HandleFunc("/a/{key}/status", statusH)
	mux.HandleFunc("/a/{key}/mcp", mcpH)
	mux.HandleFunc("GET /a/{key}/media/{id}", h.withAgent(h.serveMedia))
	// header form: Authorization: Bearer <key>
	mux.HandleFunc("/v1/send", sendH)
	mux.HandleFunc("/v1/messages", msgsH)
	mux.HandleFunc("/v1/status", statusH)
	mux.HandleFunc("/v1/doc", docH)
	mux.HandleFunc("/mcp", mcpH)
	mux.HandleFunc("GET /v1/media/{id}", h.withAgent(h.serveMedia))

	// ================= admin =================
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(adminHTML)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "admin", http.StatusFound)
	})
	adm := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, h.withAdmin(fn)) }

	adm("GET /admin/api/catalog", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"kinds": catalog})
	})
	adm("GET /admin/api/accounts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"accounts": h.listAccounts()})
	})
	adm("POST /admin/api/accounts", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		readJSON(r, &req)
		acc := h.createAccount(req.Name)
		h.startLogin(acc)
		writeJSON(w, 200, h.accountStatus(acc))
	})
	acct := func(pattern string, fn func(http.ResponseWriter, *http.Request, *Account)) {
		adm(pattern, func(w http.ResponseWriter, r *http.Request) {
			acc := h.accountByID(r.PathValue("id"))
			if acc == nil {
				errJSON(w, 404, "这个微信不存在或已解绑")
				return
			}
			fn(w, r, acc)
		})
	}
	acct("GET /admin/api/accounts/{id}", func(w http.ResponseWriter, r *http.Request, acc *Account) {
		writeJSON(w, 200, h.accountStatus(acc))
	})
	acct("PATCH /admin/api/accounts/{id}", func(w http.ResponseWriter, r *http.Request, acc *Account) {
		var req struct {
			Name string `json:"name"`
		}
		readJSON(r, &req)
		if err := h.renameAccount(acc, req.Name); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, h.accountStatus(acc))
	})
	acct("DELETE /admin/api/accounts/{id}", func(w http.ResponseWriter, r *http.Request, acc *Account) {
		h.deleteAccount(acc)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	acct("POST /admin/api/accounts/{id}/login", func(w http.ResponseWriter, r *http.Request, acc *Account) {
		h.startLogin(acc)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	acct("POST /admin/api/accounts/{id}/verify", func(w http.ResponseWriter, r *http.Request, acc *Account) {
		var req struct {
			Code string `json:"code"`
		}
		readJSON(r, &req)
		writeJSON(w, 200, map[string]bool{"accepted": h.submitVerifyCode(acc, strings.TrimSpace(req.Code))})
	})
	acct("POST /admin/api/accounts/{id}/test", func(w http.ResponseWriter, r *http.Request, acc *Account) {
		h.respondSend(w, acc, "hub", "[hub]", "测试消息：ga 推送通道正常 ✅")
	})

	// connections of an account
	acct("GET /admin/api/accounts/{id}/agents", func(w http.ResponseWriter, r *http.Request, acc *Account) {
		h.mu.Lock()
		list := append([]*Agent(nil), acc.Agents...)
		cur := ""
		if a := acc.currentAgent(); a != nil {
			cur = a.Name
		}
		h.mu.Unlock()
		writeJSON(w, 200, map[string]any{"current": cur, "agents": h.agentViews(r, list)})
	})
	acct("POST /admin/api/accounts/{id}/agents", func(w http.ResponseWriter, r *http.Request, acc *Account) {
		var in AgentInput
		readJSON(r, &in)
		a, err := h.createAgent(acc, in)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, h.agentViews(r, []*Agent{a})[0])
	})
	agentOp := func(pattern string, fn func(http.ResponseWriter, *http.Request, *Account, string)) {
		acct(pattern, func(w http.ResponseWriter, r *http.Request, acc *Account) { fn(w, r, acc, r.PathValue("name")) })
	}
	agentOp("PUT /admin/api/accounts/{id}/agents/{name}", func(w http.ResponseWriter, r *http.Request, acc *Account, name string) {
		var in AgentInput
		readJSON(r, &in)
		a, err := h.updateAgent(acc, name, in)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, h.agentViews(r, []*Agent{a})[0])
	})
	agentOp("DELETE /admin/api/accounts/{id}/agents/{name}", func(w http.ResponseWriter, r *http.Request, acc *Account, name string) {
		if err := h.deleteAgent(acc, name); err != nil {
			errJSON(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	agentOp("POST /admin/api/accounts/{id}/agents/{name}/rotate", func(w http.ResponseWriter, r *http.Request, acc *Account, name string) {
		a, err := h.rotateAgentKey(acc, name)
		if err != nil {
			errJSON(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, h.agentViews(r, []*Agent{a})[0])
	})
	agentOp("POST /admin/api/accounts/{id}/agents/{name}/current", func(w http.ResponseWriter, r *http.Request, acc *Account, name string) {
		h.mu.Lock()
		a := acc.agent(name)
		h.mu.Unlock()
		if a == nil {
			errJSON(w, 404, "not found")
			return
		}
		h.switchTo(acc, a.Name)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	agentOp("POST /admin/api/accounts/{id}/agents/{name}/test", func(w http.ResponseWriter, r *http.Request, acc *Account, name string) {
		h.mu.Lock()
		a := acc.agent(name)
		owner := acc.owner()
		h.mu.Unlock()
		if a == nil {
			errJSON(w, 404, "not found")
			return
		}
		if owner == "" {
			errJSON(w, 409, errNoContext.Error())
			return
		}
		h.deliver(acc, a.Name, owner, "你好，请用一句话介绍你自己。（这是来自管理后台的连接测试）", true, nil)
		writeJSON(w, 200, map[string]string{"status": "sent", "mode": a.Mode})
	})

	// static assets (agent logos). Locked down so an SVG opened directly can't run anything.
	sub, _ := fs.Sub(assetsFS, "assets")
	files := http.StripPrefix("/assets/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		files.ServeHTTP(w, r)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return mux
}

func (h *Hub) inboxResult(r *http.Request, a *Agent, after int64, wait int) map[string]any {
	if wait > 60 {
		wait = 60
	}
	msgs := h.fetchInbox(r.Context(), a, after, time.Duration(wait)*time.Second)
	next := after
	for _, m := range msgs {
		if m.ID > next {
			next = m.ID
		}
	}
	if msgs == nil {
		msgs = []InMsg{}
	}
	return map[string]any{"messages": msgs, "next": next}
}

func (h *Hub) respondSend(w http.ResponseWriter, acc *Account, name, label, text string) {
	h.respondErr(w, h.enqueue(acc, name, label, "", text))
}

func (h *Hub) respondErr(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		writeJSON(w, 200, map[string]string{"status": "accepted"})
	case errors.Is(err, errNoContext):
		errJSON(w, 409, err.Error())
	default:
		var ae *APIError
		if errors.As(err, &ae) {
			writeJSON(w, 502, map[string]any{"error": ae.Msg, "code": ae.Code,
				"hint": "推送通道可能已过期：请主人在微信里回复任意内容或 /ping"})
			return
		}
		errJSON(w, 502, err.Error())
	}
}

func (h *Hub) withAgent(fn func(http.ResponseWriter, *http.Request, *Agent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key == "" {
			key = bearer(r)
		}
		a := h.agentByKey(key)
		if a == nil {
			errJSON(w, 401, "invalid agent link / key")
			return
		}
		if !isDocPath(r.URL.Path) && r.URL.Path != "/v1/doc" {
			h.markSeen(a, true)
		}
		fn(w, r, a)
	}
}

func (h *Hub) withAdmin(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(h.cfg.AdminToken)) != 1 {
			errJSON(w, 401, "invalid admin token")
			return
		}
		fn(w, r)
	}
}

// isDocPath reports whether p is the bare agent link (/a/<key> or /a/<key>/).
func isDocPath(p string) bool {
	p = strings.TrimSuffix(p, "/")
	return strings.Count(p, "/") == 2 && strings.HasPrefix(p, "/a/")
}

// ================= MCP (streamable HTTP, stateless JSON) =================

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

var mcpTools = []map[string]any{
	{
		"name":        "send_wechat_message",
		"description": "Send a text message to the owner's WeChat (via ga.fyi). The message is prefixed with this agent's name. Use for replies, results, notifications and questions to the human.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "Plain text; long text is split automatically"},
		}, "required": []string{"text"}},
	},
	{
		"name": "send_wechat_file",
		"description": "Send an image, video or file to the owner's WeChat. Give either file_url (a public http(s) URL the Hub downloads) or file_base64 (the file content). " +
			"Images (jpg/png/gif/webp) show as pictures, mp4 as video, everything else as a file attachment. Max 25 MB. Do not pass local file paths: the Hub cannot read your disk.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"file_url":    map[string]any{"type": "string", "description": "Public http(s) URL of the file"},
			"file_base64": map[string]any{"type": "string", "description": "File content, base64-encoded (alternative to file_url)"},
			"filename":    map[string]any{"type": "string", "description": "File name with extension, e.g. report.pdf (recommended with file_base64)"},
			"caption":     map[string]any{"type": "string", "description": "Optional text sent together with the file"},
			"as":          map[string]any{"type": "string", "enum": []string{"image", "video", "file"}, "description": "Force how it is shown (default: by file type)"},
		}},
	},
	{
		"name":        "read_wechat_messages",
		"description": "Read messages the owner sent to this agent from WeChat. Pass the returned `next` as `after` to get only newer messages. Optionally wait up to 60s for a new one.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"after":        map[string]any{"type": "integer", "description": "Only messages with id greater than this"},
			"wait_seconds": map[string]any{"type": "integer", "description": "Long-poll up to this many seconds (0-60)"},
		}},
	},
	{
		"name":        "wechat_status",
		"description": "Check whether the WeChat channel is online and able to push messages.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

func (h *Hub) handleMCP(w http.ResponseWriter, r *http.Request, a *Agent) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(405)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	var req rpcReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
		return
	}
	if len(req.ID) == 0 { // notification
		w.WriteHeader(202)
		return
	}
	reply := func(result any) { writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) }
	rpcErr := func(code int, msg string) {
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": msg}})
	}
	toolText := func(text string, isErr bool) {
		reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr})
	}

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		reply(map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "gafyi", "version": version},
			"instructions":    "You are agent '@" + a.Name + "'. Use send_wechat_message to message the human on WeChat.",
		})
	case "ping":
		reply(map[string]any{})
	case "tools/list":
		reply(map[string]any{"tools": mcpTools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		switch p.Name {
		case "send_wechat_message":
			var args struct {
				Text string `json:"text"`
			}
			json.Unmarshal(p.Arguments, &args)
			if strings.TrimSpace(args.Text) == "" {
				toolText("text is required", true)
				return
			}
			if err := h.enqueue(a.acc, a.Name, a.label(), "", args.Text); err != nil {
				toolText("send failed: "+err.Error(), true)
				return
			}
			toolText("accepted by WeChat server", false)
		case "send_wechat_file":
			var args sendFileArgs
			json.Unmarshal(p.Arguments, &args)
			if !args.hasFile() {
				toolText("file_url or file_base64 is required", true)
				return
			}
			m, err := mediaFromRequest(r.Context(), args)
			if err != nil {
				toolText(err.Error(), true)
				return
			}
			if err := h.enqueueMedia(a.acc, a.Name, a.label(), firstNonEmpty(args.Caption, args.Text), m); err != nil {
				toolText("send failed: "+err.Error(), true)
				return
			}
			toolText("accepted by WeChat server ("+m.Kind+", "+m.Name+")", false)
		case "read_wechat_messages":
			var args struct {
				After int64 `json:"after"`
				Wait  int   `json:"wait_seconds"`
			}
			json.Unmarshal(p.Arguments, &args)
			b, _ := json.Marshal(h.inboxResult(r, a, args.After, args.Wait))
			toolText(string(b), false)
		case "wechat_status":
			s := h.accountStatus(a.acc)
			b, _ := json.Marshal(map[string]any{"online": s.Online, "can_send": s.LoggedIn && s.HasContext,
				"last_inbound": s.LastInbound, "sends_since_last_inbound": s.SendsSince})
			toolText(string(b), false)
		default:
			rpcErr(-32602, "unknown tool: "+p.Name)
		}
	default:
		rpcErr(-32601, "method not found: "+req.Method)
	}
}

// ================= views & agent guide =================

func (h *Hub) baseURL(r *http.Request) string {
	if h.cfg.PublicURL != "" {
		return h.cfg.PublicURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type agentView struct {
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	Link      string    `json:"link"`
	Kind      string    `json:"kind"`
	KindTitle string    `json:"kind_title"`
	Mode      string    `json:"mode"`
	Webhook   string    `json:"webhook,omitempty"`
	Headers   []string  `json:"webhook_headers,omitempty"` // names only; values are secret
	Pending   bool      `json:"pending"`                   // webhook not configured yet
	Handoff   string    `json:"handoff"`                   // tailored prompt for the agent
	Steps     []string  `json:"steps"`
	Created   time.Time `json:"created"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  *seenInfo `json:"last_seen,omitempty"`
}

func (h *Hub) agentViews(r *http.Request, list []*Agent) []agentView {
	base := h.baseURL(r)
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]agentView, 0, len(list))
	for _, a := range list {
		v := agentView{Name: a.Name, Key: a.Key, Link: base + "/a/" + a.Key, Kind: a.Kind, Mode: a.Mode,
			Webhook: a.Webhook, Pending: a.Mode == "webhook" && a.Webhook == "", Created: a.Created, FirstSeen: a.FirstSeen}
		if k := kindByID(a.Kind); k != nil {
			v.KindTitle = k.Title
			v.Handoff = renderTpl(k.Prompt, a.Name, v.Link)
			for _, s := range k.Steps {
				v.Steps = append(v.Steps, renderTpl(s, a.Name, v.Link))
			}
		}
		for k := range a.WebhookHeaders {
			v.Headers = append(v.Headers, k)
		}
		if s, ok := a.acc.seen[strings.ToLower(a.Name)]; ok {
			v.LastSeen = &s
		}
		out = append(out, v)
	}
	return out
}

// agentDoc is the integration guide served at the agent's personal link.
func agentDoc(base string, a *Agent) string {
	return renderTpl(agentDocTpl, a.Name, base+"/a/"+a.Key)
}

const agentDocTpl = `# 微信消息通道 · @{name}

你是「@{name}」。这个链接是你专属的微信消息通道，通过它你可以：

1. **给主人的微信发消息**
2. **读取主人在微信里 @{name} 发给你的消息**

你的专属链接（同时也是密钥，请保密，不要出现在公开代码、日志或回复中）：

    {link}

下面所有接口都在这个链接之下，**不需要额外的请求头或认证**。

---

## 1. 发消息给主人

    POST {link}/send
    Content-Type: application/json

    {"text": "要发送的内容"}

示例：

    curl -s -X POST {link}/send -H "Content-Type: application/json" -d '{"text":"任务完成 ✅"}'

如果你只能打开网址、不能发 POST 请求，也可以用 GET（内容需要 URL 编码）：

    GET {link}/send?text=%E4%BD%A0%E5%A5%BD

返回值：

| HTTP | 含义 | 你该怎么做 |
|---|---|---|
| 200 {"status":"accepted"} | 已发出 | — |
| 409 | 推送通道尚未激活 | 告诉主人：先在微信里给 ClawBot 发一句话 |
| 502 | 发送失败（通常是通道过期） | 稍后重试一次；仍失败就请主人在微信里回复 /ping |

规则：

- 文字只支持纯文本（微信不渲染 Markdown）。消息会自动加上 [{name}] 前缀，你不需要自己署名。
- 要发图片、视频或文件，见下面的「发图片和文件」。
- 不要刷屏：把多条内容合并成一条再发，长文本会自动分段。
- 适合发送：任务结果、需要主人做决定的问题、重要提醒。

## 1b. 发图片和文件

同一个 /send 接口，三种方式任选（单个文件最大 25 MB）：

    # a) 文件在你本机：直接上传（推荐）
    curl -s -X POST {link}/send -F "file=@/path/to/chart.png" -F "text=今天的销售图"

    # b) 文件有公网地址：Hub 去下载
    POST {link}/send  {"file_url": "https://example.com/report.pdf", "text": "报告见附件"}

    # c) 文件内容 base64
    POST {link}/send  {"file_base64": "iVBORw0...", "filename": "chart.png", "text": "可选说明"}

- jpg/png/gif/webp 显示为图片，mp4 显示为视频，其他（pdf、xlsx、zip…）显示为文件。可以用 "as": "image" | "video" | "file" 强制指定。
- 不要把本机路径当文字发出去（例如 MEDIA:/tmp/a.png），主人看到的只是一串乱码。Hub 读不到你的硬盘，请用方式 a 上传。
- 不要直接发网盘分享链接（夸克、百度、阿里等），微信会拦截。

## 2. 读取主人发给你的消息

主人在微信里发「@{name} 内容」之后，不带 @ 的消息也会继续发给你，直到他 @ 了别的智能体。

    GET {link}/messages?after=0&wait=30

返回：

    {"messages":[{"id":1731050000001,"text":"帮我查一下…","at":"2026-11-08T10:00:00+08:00","direct":true}],"next":1731050000001}

- 第一次 after 传 0，之后每次传上一次返回的 next，就只会拿到新消息。
- wait 是长轮询秒数（0–60），有新消息会立即返回。
- direct=true 表示主人明确 @ 了你。
- 收件箱只保留最近 500 条，服务重启后清空。
- 主人发来图片、文件、视频或语音时，消息里会带 attachments：

      {"id":…,"text":"[图片]","attachments":[{"id":"50b1…","type":"image","url":"{link}/media/50b1…"}]}

  GET 每个 url 就能下载原文件（type 为 image / file / video / voice；file 带 name 和 size；语音是 silk 格式，text 里通常已有微信的语音转文字）。附件链接保留 7 天，Hub 重启后失效。

典型循环：读消息 → 处理 → 用 /send 回复 → 带着 next 继续读。

## 3. 查看通道状态

    GET {link}/status

返回中 can_send=true 表示当前可以推送。

## 4. MCP（如果你的客户端支持）

    MCP Server（Streamable HTTP）：{link}/mcp

提供四个工具：send_wechat_message(text)、send_wechat_file(file_url 或 file_base64, filename, caption)、read_wechat_messages(after, wait_seconds)、wechat_status()。
`

// ================= file sending helpers =================

const maxRequestBytes = maxMediaBytes*4/3 + 1<<20 // base64 overhead + JSON

type sendFileArgs struct {
	Text       string `json:"text"`
	Caption    string `json:"caption"`
	FileURL    string `json:"file_url"`
	ImageURL   string `json:"image_url"`
	FileBase64 string `json:"file_base64"`
	Filename   string `json:"filename"`
	As         string `json:"as"`
}

func (s sendFileArgs) hasFile() bool {
	return s.FileURL != "" || s.ImageURL != "" || s.FileBase64 != ""
}

func mediaFromRequest(ctx context.Context, s sendFileArgs) (*OutMedia, error) {
	as := s.As
	if s.ImageURL != "" && s.FileURL == "" {
		s.FileURL = s.ImageURL
		if as == "" {
			as = "image"
		}
	}
	if s.FileBase64 != "" {
		b64 := s.FileBase64
		if i := strings.Index(b64, ";base64,"); i >= 0 && strings.HasPrefix(b64, "data:") {
			b64 = b64[i+8:] // data URL
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return nil, errors.New("file_base64 is not valid base64")
		}
		return newOutMedia(data, s.Filename, as)
	}
	if strings.HasPrefix(s.FileURL, "/") || strings.HasPrefix(s.FileURL, "file:") {
		return nil, errors.New("local paths can't be read by the Hub: upload the file content (file_base64 or multipart) or give a public URL")
	}
	data, name, err := fetchURL(ctx, s.FileURL)
	if err != nil {
		return nil, err
	}
	if s.Filename != "" {
		name = s.Filename
	}
	return newOutMedia(data, name, as)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
