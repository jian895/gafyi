package main

// QR-code login for a WeChat account (iLink "ClawBot" binding).

import (
	"context"
	"log"
	"sync"
	"time"
)

type loginState struct {
	mu         sync.Mutex
	Status     string // starting, waiting, scaned, need_verifycode, confirmed, failed
	QRURL      string
	Err        string
	MergedInto string // set when this WeChat was already bound to another account
	verifyCh   chan string
	cancel     context.CancelFunc
}

func (l *loginState) snapshot() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]any{"status": l.Status, "qr_url": l.QRURL, "error": l.Err, "merged_into": l.MergedInto}
}

func (l *loginState) set(status, qr, err string) {
	l.mu.Lock()
	l.Status = status
	if qr != "" {
		l.QRURL = qr
	}
	l.Err = err
	l.mu.Unlock()
}

// startLogin begins (or restarts) the QR flow for acc.
func (h *Hub) startLogin(acc *Account) {
	h.mu.Lock()
	if acc.login != nil && acc.login.cancel != nil {
		acc.login.cancel()
	}
	ctx, cancel := context.WithTimeout(acc.ctx, 10*time.Minute)
	l := &loginState{Status: "starting", verifyCh: make(chan string, 1), cancel: cancel}
	acc.login = l
	var local []string
	if acc.Creds != nil {
		local = []string{acc.Creds.Token}
	}
	h.mu.Unlock()
	go h.runLogin(ctx, acc, l, local)
}

func (h *Hub) submitVerifyCode(acc *Account, code string) bool {
	h.mu.Lock()
	l := acc.login
	h.mu.Unlock()
	if l == nil {
		return false
	}
	select {
	case l.verifyCh <- code:
		return true
	default:
		return false
	}
}

func (h *Hub) runLogin(ctx context.Context, acc *Account, l *loginState, local []string) {
	defer l.cancel()
	for attempt := 0; attempt < 3; attempt++ {
		qr, err := h.api.getQRCode(ctx, local)
		if err != nil {
			l.set("failed", "", "获取二维码失败："+err.Error())
			return
		}
		log.Printf("[%s] login QR ready", acc.Name)
		l.set("waiting", qr.ImgURL, "")
		pollBase := defaultBaseURL
		pendingCode := ""
	poll:
		for {
			if ctx.Err() != nil {
				l.set("failed", "", "扫码超时，请重新开始")
				return
			}
			s, err := h.api.pollQRStatus(ctx, pollBase, qr.QRCode, pendingCode)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("[%s] qr status: %v", acc.Name, err)
					sleepCtx(ctx, 2*time.Second)
				}
				continue
			}
			switch s.Status {
			case "scaned":
				pendingCode = ""
				l.set("scaned", "", "")
			case "confirmed":
				if s.BotToken == "" || s.UserID == "" {
					l.set("failed", "", "登录已确认但缺少凭据")
					return
				}
				base := s.BaseURL
				if base == "" {
					base = defaultBaseURL
				}
				h.finishLogin(acc, l, &Creds{Token: s.BotToken, BaseURL: base, BotID: s.BotID, UserID: s.UserID, SavedAt: time.Now().UTC().Format(time.RFC3339)})
				return
			case "binded_redirect":
				h.mu.Lock()
				ok := acc.Creds != nil
				h.mu.Unlock()
				if ok {
					l.set("confirmed", "", "")
					h.startPoller(acc)
				} else {
					l.set("failed", "", "服务器提示该 bot 已绑定，但本地没有凭据")
				}
				return
			case "need_verifycode":
				l.set("need_verifycode", "", "")
				select {
				case code := <-l.verifyCh:
					pendingCode = code
				case <-ctx.Done():
				}
				continue
			case "verify_code_blocked", "expired":
				break poll
			case "scaned_but_redirect":
				if s.RedirectHost != "" {
					pollBase = "https://" + s.RedirectHost
				}
			}
			sleepCtx(ctx, 2*time.Second)
		}
	}
	l.set("failed", "", "二维码已过期 3 次，请重新开始")
}

// finishLogin stores credentials. If the same WeChat user is already bound to
// another account, that account is updated instead and the new (empty) one removed.
func (h *Hub) finishLogin(acc *Account, l *loginState, c *Creds) {
	h.mu.Lock()
	var target *Account = acc
	for _, other := range h.accounts {
		if other != acc && other.Creds != nil && other.Creds.UserID == c.UserID {
			target = other
			break
		}
	}
	target.Creds = c
	target.Cursor = ""
	h.saveLocked()
	h.mu.Unlock()

	if target != acc {
		log.Printf("[%s] this WeChat is already bound as %q - updated that account", acc.Name, target.Name)
		l.mu.Lock()
		l.Status, l.MergedInto = "confirmed", target.ID
		l.mu.Unlock()
		h.startPoller(target)
		if len(acc.Agents) == 0 {
			go func() { time.Sleep(10 * time.Second); h.deleteAccount(acc) }() // let the UI read merged_into first
		}
		return
	}
	l.set("confirmed", "", "")
	log.Printf("[%s] login confirmed: bot=%s owner=%s", acc.Name, c.BotID, short(c.UserID))
	h.startPoller(acc)
}
