package main

// Per-account runtime: polling WeChat, routing inbound messages, the outbound
// send queue, and account management.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type InMsg struct {
	ID     int64     `json:"id"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
	Agent  string    `json:"agent"`
	Direct bool      `json:"direct"` // the owner addressed this agent with @name
	// Attachments are images/files/videos/voice the owner sent; fetch each url.
	Attachments []Attachment `json:"attachments,omitempty"`
	from        string
}

type inbox struct {
	msgs []InMsg
	wake chan struct{}
}

type sendReq struct {
	label string
	name  string
	to    string
	text  string
	media *OutMedia // optional file; text is then its caption
	done  chan error
}

var errNoContext = errors.New("推送通道还没激活：请先在微信里给 ClawBot 发一句话")

// ---------- lifecycle ----------

func (h *Hub) start() {
	h.mu.Lock()
	accs := append([]*Account(nil), h.accounts...)
	h.mu.Unlock()
	for _, acc := range accs {
		go h.sender(acc)
		if acc.Creds != nil {
			h.startPoller(acc)
		}
	}
}

func (h *Hub) createAccount(name string) *Account {
	h.mu.Lock()
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("微信 %d", len(h.accounts)+1)
	}
	acc := &Account{ID: genID(), Name: trunc(name, 32), Created: time.Now()}
	h.attachLocked(acc)
	h.saveLocked()
	h.mu.Unlock()
	go h.sender(acc)
	log.Printf("account created: %s (%s)", acc.Name, acc.ID)
	return acc
}

func (h *Hub) renameAccount(acc *Account, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("名称不能为空")
	}
	h.mu.Lock()
	acc.Name = trunc(name, 32)
	h.saveLocked()
	h.mu.Unlock()
	return nil
}

func (h *Hub) deleteAccount(acc *Account) {
	h.mu.Lock()
	if acc.pollCancel != nil {
		acc.pollCancel()
	}
	if acc.login != nil && acc.login.cancel != nil {
		acc.login.cancel()
	}
	acc.stop()
	for _, a := range acc.Agents {
		delete(h.byKey, a.Key)
	}
	for _, b := range acc.boxes {
		close(b.wake)
	}
	acc.boxes = map[string]*inbox{} // waiting long-polls find no box and return
	for i, x := range h.accounts {
		if x == acc {
			h.accounts = append(h.accounts[:i], h.accounts[i+1:]...)
			break
		}
	}
	h.saveLocked()
	h.mu.Unlock()
	log.Printf("account deleted: %s (%s)", acc.Name, acc.ID)
}

// ---------- polling ----------

func (h *Hub) startPoller(acc *Account) {
	h.mu.Lock()
	if acc.pollCancel != nil {
		acc.pollCancel()
	}
	ctx, cancel := context.WithCancel(acc.ctx)
	acc.pollCancel = cancel
	creds := *acc.Creds
	h.mu.Unlock()
	go h.poll(ctx, acc, creds)
}

func (h *Hub) setPoll(acc *Account, s string) {
	h.mu.Lock()
	acc.pollStatus = s
	h.mu.Unlock()
}

func (h *Hub) poll(ctx context.Context, acc *Account, c Creds) {
	if err := h.api.notifyStart(ctx, c.BaseURL, c.Token); err != nil && ctx.Err() == nil {
		log.Printf("[%s] notifystart: %v", acc.Name, err)
	}
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		h.mu.Lock()
		cursor := acc.Cursor
		h.mu.Unlock()
		h.setPoll(acc, "在线")
		up, err := h.api.getUpdates(ctx, c.BaseURL, c.Token, cursor)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.Code == -14 {
				log.Printf("[%s] session paused by server (-14), sleeping 1h", acc.Name)
				h.mu.Lock()
				acc.pollStatus = "会话被暂停"
				acc.pausedTill = time.Now().Add(time.Hour)
				h.mu.Unlock()
				sleepCtx(ctx, time.Hour)
				continue
			}
			if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Client.Timeout") {
				continue // normal long-poll timeout
			}
			log.Printf("[%s] getupdates: %v (retry in %s)", acc.Name, err, backoff)
			h.setPoll(acc, "连接异常："+trunc(err.Error(), 80))
			sleepCtx(ctx, backoff)
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = 2 * time.Second
		for _, m := range up.Msgs {
			h.handleInbound(acc, m)
		}
		if up.GetUpdatesBuf != "" {
			h.mu.Lock()
			acc.Cursor = up.GetUpdatesBuf
			h.saveLocked()
			h.mu.Unlock()
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ---------- routing ----------

func (h *Hub) handleInbound(acc *Account, m wireMsg) {
	if m.MessageType != 1 || m.FromUserID == "" {
		return
	}
	text := strings.TrimSpace(textOf(m))
	from := m.FromUserID
	h.mu.Lock()
	cv := acc.Convs[from]
	if cv == nil {
		cv = &ConvState{}
		acc.Convs[from] = cv
	}
	if m.ContextToken != "" {
		cv.ContextToken = m.ContextToken
	}
	cv.LastInbound = time.Now()
	cv.SendsSince = 0
	var current string
	if a := acc.currentAgent(); a != nil {
		current = a.Name
	}
	h.saveLocked()
	h.mu.Unlock()

	if text == "" {
		return
	}
	refs := h.extractMedia(acc, m)
	log.Printf("[%s] inbound: %q%s", acc.Name, trunc(text, 80), map[bool]string{true: fmt.Sprintf(" (+%d media)", len(refs))}[len(refs) > 0])

	if strings.HasPrefix(text, "/") {
		fields := strings.Fields(text)
		switch strings.ToLower(fields[0]) {
		case "/help", "/list", "/agents":
			h.hubReply(acc, from, h.helpText(acc))
			return
		case "/use":
			var a *Agent
			if len(fields) >= 2 {
				h.mu.Lock()
				a = acc.agent(fields[1])
				h.mu.Unlock()
			}
			if a == nil {
				h.hubReply(acc, from, "没有这个智能体。\n\n"+h.helpText(acc))
				return
			}
			h.switchTo(acc, a.Name)
			h.hubReply(acc, from, "已切换到 @"+a.Name+"，之后不带 @ 的消息都会发给它。")
			return
		case "/ping":
			h.hubReply(acc, from, "pong，推送通道已刷新")
			return
		}
	}

	if strings.HasPrefix(text, "@") || strings.HasPrefix(text, "＠") {
		name, rest := h.matchMention(acc, text)
		switch {
		case name == "all":
			h.mu.Lock()
			names := agentNames(acc)
			h.mu.Unlock()
			for _, n := range names {
				h.deliver(acc, n, from, rest, true, refs)
			}
		case name != "":
			changed := h.switchTo(acc, name)
			if rest == "" && len(refs) == 0 {
				if changed {
					h.hubReply(acc, from, "已切换到 @"+name+"，之后不带 @ 的消息都会发给它。")
				} else {
					h.hubReply(acc, from, "当前已经在和 @"+name+" 对话。")
				}
				return
			}
			h.deliver(acc, name, from, rest, true, refs)
		default:
			mentioned, _, _ := strings.Cut(strings.TrimLeft(text, "@＠"), " ")
			h.hubReply(acc, from, "没有叫 @"+trunc(mentioned, 20)+" 的智能体。\n\n"+h.helpText(acc))
		}
		return
	}
	if current == "" {
		h.hubReply(acc, from, "这个微信还没有连接任何智能体，请先在管理后台「新建连接」。")
		return
	}
	h.deliver(acc, current, from, text, false, refs)
}

func agentNames(acc *Account) []string {
	names := make([]string, 0, len(acc.Agents))
	for _, a := range acc.Agents {
		names = append(names, a.Name)
	}
	return names
}

// matchMention resolves "@name rest" (also "@name：rest", "@namerest") to the
// longest matching agent name. Returns ("all", rest) for @all, ("", "") if none.
func (h *Hub) matchMention(acc *Account, text string) (string, string) {
	s := strings.TrimLeft(text, "@＠")
	h.mu.Lock()
	names := append(agentNames(acc), "all")
	h.mu.Unlock()
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, n := range names {
		if len(s) >= len(n) && strings.EqualFold(s[:len(n)], n) {
			rest := s[len(n):]
			// a following ASCII letter/digit means a different, longer name (e.g. @grok2)
			if r, _ := utf8.DecodeRuneInString(rest); r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
				continue
			}
			rest = strings.TrimLeftFunc(rest, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(":：,，", r) })
			return n, strings.TrimSpace(rest)
		}
	}
	return "", ""
}

// switchTo makes name the account's current conversation target.
func (h *Hub) switchTo(acc *Account, name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := ""
	if a := acc.currentAgent(); a != nil {
		prev = a.Name
	}
	acc.Current = name
	h.saveLocked()
	if prev != name {
		log.Printf("[%s] route -> @%s", acc.Name, name)
	}
	return prev != name
}

func (h *Hub) helpText(acc *Account) string {
	h.mu.Lock()
	names := agentNames(acc)
	cur := ""
	if a := acc.currentAgent(); a != nil {
		cur = a.Name
	}
	h.mu.Unlock()
	if len(names) == 0 {
		return "这个微信还没有连接任何智能体，请先在管理后台「新建连接」。"
	}
	var b strings.Builder
	b.WriteString("已连接的智能体：\n")
	for _, n := range names {
		if n == cur {
			b.WriteString("▶ @" + n + "（当前对话）\n")
		} else {
			b.WriteString("  @" + n + "\n")
		}
	}
	b.WriteString("\n@名字 内容 → 切换到它并发送，之后不带 @ 的消息都发给它\n@名字 → 只切换\n@all 内容 → 群发（不切换）\n/ping → 刷新推送通道")
	return b.String()
}

func (h *Hub) deliver(acc *Account, agent, from, text string, direct bool, refs []*mediaRef) {
	h.mu.Lock()
	a := acc.agent(agent)
	if a == nil {
		h.mu.Unlock()
		return
	}
	box := acc.boxes[strings.ToLower(a.Name)]
	h.nextID++
	msg := InMsg{ID: h.nextID, Text: text, At: time.Now(), Agent: a.Name, Direct: direct, from: from,
		Attachments: h.grantMediaLocked(a, refs)}
	box.msgs = append(box.msgs, msg)
	if len(box.msgs) > 500 {
		box.msgs = box.msgs[len(box.msgs)-500:]
	}
	close(box.wake)
	box.wake = make(chan struct{})
	ac := *a
	s, seenOK := acc.seen[strings.ToLower(a.Name)]
	h.mu.Unlock()

	if ac.Mode == "webhook" {
		go h.pushWebhook(acc, ac, msg)
		return
	}
	// link mode: tell the owner if the agent isn't polling (skip right after a restart)
	if time.Since(h.startedAt) > 2*time.Minute {
		var why string
		switch {
		case ac.FirstSeen.IsZero():
			why = "还从未连接过 Hub，可能还没完成配置"
		case !seenOK || time.Since(s.At) > 3*time.Minute:
			last := "很久以前"
			if seenOK {
				last = humanAgo(s.At)
			}
			why = "目前不在线（最后连接：" + last + "）"
		}
		if why != "" {
			h.hubReply(acc, from, "@"+ac.Name+" "+why+"。消息已暂存，它连上后会收到。")
		}
	}
}

func (h *Hub) fetchInbox(ctx context.Context, a *Agent, after int64, wait time.Duration) []InMsg {
	deadline := time.After(wait)
	for {
		h.mu.Lock()
		box := a.acc.boxes[strings.ToLower(a.Name)]
		if box == nil {
			h.mu.Unlock()
			return nil
		}
		var out []InMsg
		for _, m := range box.msgs {
			if m.ID > after {
				out = append(out, m)
			}
		}
		wake := box.wake
		h.mu.Unlock()
		if len(out) > 0 || wait <= 0 {
			return out
		}
		select {
		case <-wake:
		case <-deadline:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// ---------- sending ----------

func (h *Hub) hubReply(acc *Account, to, text string) {
	go func() {
		if err := h.enqueue(acc, "hub", "[hub]", to, text); err != nil {
			log.Printf("[%s] hub reply: %v", acc.Name, err)
		}
	}()
}

func (h *Hub) enqueue(acc *Account, name, label, to, text string) error {
	return h.enqueueReq(acc, &sendReq{name: name, label: label, to: to, text: text})
}

// enqueueMedia sends a file (image / video / file) with an optional caption.
func (h *Hub) enqueueMedia(acc *Account, name, label, caption string, m *OutMedia) error {
	return h.enqueueReq(acc, &sendReq{name: name, label: label, text: caption, media: m})
}

func (h *Hub) enqueueReq(acc *Account, r *sendReq) error {
	to := r.to
	if to == "" {
		h.mu.Lock()
		to = acc.owner()
		h.mu.Unlock()
	}
	if to == "" {
		return errNoContext
	}
	r.to, r.done = to, make(chan error, 1)
	select {
	case acc.sendQ <- r:
	case <-acc.ctx.Done():
		return errors.New("这个微信已被解绑")
	default:
		return errors.New("发送队列已满，请稍后再试")
	}
	select {
	case err := <-r.done:
		return err
	case <-acc.ctx.Done():
		return errors.New("这个微信已被解绑")
	}
}

func (h *Hub) sender(acc *Account) {
	interval := time.Duration(h.cfg.SendIntervalMs) * time.Millisecond
	for {
		select {
		case <-acc.ctx.Done():
			return
		case r := <-acc.sendQ:
			r.done <- h.sendNow(acc, r)
			time.Sleep(interval)
		}
	}
}

func (h *Hub) sendNow(acc *Account, r *sendReq) error {
	h.mu.Lock()
	if acc.Creds == nil {
		h.mu.Unlock()
		return errors.New("这个微信还没有扫码登录")
	}
	creds := *acc.Creds
	cv := acc.Convs[r.to]
	if cv == nil || cv.ContextToken == "" {
		h.mu.Unlock()
		return errNoContext
	}
	ctxTok := cv.ContextToken
	cv.SendsSince++
	warn := h.cfg.WarnAfterSends > 0 && cv.SendsSince == h.cfg.WarnAfterSends
	h.saveLocked()
	h.mu.Unlock()

	if r.media != nil {
		return h.sendMediaNow(acc, r, creds, ctxTok, warn)
	}
	text := r.label + " " + defangLinks(r.text)
	if warn {
		text += "\n\n（已连续推送多条，回复任意内容或 /ping 以保持推送通道）"
	}
	for i, chunk := range chunkText(text, 1800) {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		if err := h.api.sendText(acc.ctx, creds.BaseURL, creds.Token, r.to, ctxTok, chunk); err != nil {
			log.Printf("[%s] send %s failed: %v", acc.Name, r.name, err)
			return err
		}
	}
	log.Printf("[%s] sent %s: %q", acc.Name, r.name, trunc(r.text, 60))
	return nil
}

func chunkText(s string, max int) []string {
	var out []string
	for utf8.RuneCountInString(s) > max {
		runes := []rune(s)
		cut := max
		for i := max; i > max*7/10; i-- {
			if runes[i] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(runes[:cut]))
		s = strings.TrimLeft(string(runes[cut:]), "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// ---------- status ----------

type AccountStatus struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	LoggedIn    bool           `json:"logged_in"`
	BotID       string         `json:"bot_id,omitempty"`
	Owner       string         `json:"owner,omitempty"`
	Poller      string         `json:"poller"`
	Online      bool           `json:"online"`
	PausedUntil *time.Time     `json:"paused_until,omitempty"`
	HasContext  bool           `json:"has_context"`
	LastInbound *time.Time     `json:"last_inbound,omitempty"`
	SendsSince  int            `json:"sends_since_last_inbound"`
	Current     string         `json:"current"`
	Agents      []AgentBrief   `json:"agents"`
	Login       map[string]any `json:"login,omitempty"`
	Created     time.Time      `json:"created"`
}

type AgentBrief struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Healthy bool   `json:"healthy"`
}

func (h *Hub) accountStatus(acc *Account) AccountStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := AccountStatus{ID: acc.ID, Name: acc.Name, Poller: acc.pollStatus, Created: acc.Created, Agents: []AgentBrief{}}
	if acc.Creds != nil {
		s.LoggedIn = true
		s.BotID = acc.Creds.BotID
		s.Owner = short(acc.Creds.UserID)
		s.Online = acc.pollStatus == "在线"
	}
	if time.Now().Before(acc.pausedTill) {
		t := acc.pausedTill
		s.PausedUntil = &t
	}
	if cv := acc.Convs[acc.owner()]; cv != nil {
		s.HasContext = cv.ContextToken != ""
		if !cv.LastInbound.IsZero() {
			t := cv.LastInbound
			s.LastInbound = &t
		}
		s.SendsSince = cv.SendsSince
	}
	if a := acc.currentAgent(); a != nil {
		s.Current = a.Name
	}
	for _, a := range acc.Agents {
		sn, ok := acc.seen[strings.ToLower(a.Name)]
		s.Agents = append(s.Agents, AgentBrief{Name: a.Name, Kind: a.Kind, Healthy: ok && sn.OK})
	}
	if acc.login != nil {
		s.Login = acc.login.snapshot()
	}
	return s
}

func (h *Hub) listAccounts() []AccountStatus {
	h.mu.Lock()
	accs := append([]*Account(nil), h.accounts...)
	h.mu.Unlock()
	out := make([]AccountStatus, 0, len(accs))
	for _, acc := range accs {
		out = append(out, h.accountStatus(acc))
	}
	return out
}

// ---------- helpers ----------

func short(s string) string {
	if len(s) > 10 {
		return s[:6] + "…"
	}
	return s
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func humanAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	}
	return fmt.Sprintf("%d 天前", int(d.Hours()/24))
}

// sendMediaNow uploads r.media to the WeChat CDN and sends it, preceded by a
// text line carrying the agent label and caption.
func (h *Hub) sendMediaNow(acc *Account, r *sendReq, creds Creds, ctxTok string, warn bool) error {
	m := r.media
	caption := strings.TrimSpace(defangLinks(r.text))
	if caption == "" {
		caption = map[string]string{"image": "发来一张图片", "video": "发来一段视频"}[m.Kind]
		if caption == "" {
			caption = "发来一个文件：" + m.Name
		}
	}
	text := r.label + " " + caption
	if warn {
		text += "\n\n（已连续推送多条，回复任意内容或 /ping 以保持推送通道）"
	}
	mt := map[string]int{"image": uploadImage, "video": uploadVideo}[m.Kind]
	if mt == 0 {
		mt = uploadFile
	}
	up, err := h.api.uploadMedia(acc.ctx, creds.BaseURL, creds.Token, r.to, mt, m.Data)
	if err != nil {
		log.Printf("[%s] upload %s %s failed: %v", acc.Name, r.name, m.Kind, err)
		return err
	}
	up.fileName = m.Name
	for i, chunk := range chunkText(text, 1800) {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		if err := h.api.sendText(acc.ctx, creds.BaseURL, creds.Token, r.to, ctxTok, chunk); err != nil {
			log.Printf("[%s] send %s failed: %v", acc.Name, r.name, err)
			return err
		}
	}
	time.Sleep(300 * time.Millisecond)
	if err := h.api.sendItem(acc.ctx, creds.BaseURL, creds.Token, r.to, ctxTok, mediaItem(m.Kind, up)); err != nil {
		log.Printf("[%s] send %s %s failed: %v", acc.Name, r.name, m.Kind, err)
		return err
	}
	log.Printf("[%s] sent %s %s: %s (%d bytes)", acc.Name, r.name, m.Kind, m.Name, len(m.Data))
	return nil
}
