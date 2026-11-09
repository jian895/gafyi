package main

// Minimal client for the WeChat iLink Bot API (the protocol used by
// Tencent/openclaw-weixin). Media upload/download lives in media.go.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	defaultBaseURL = "https://ilinkai.weixin.qq.com"
	channelVersion = "0.1.0"
	botAgent       = "gafyi/" + version
)

type APIError struct {
	HTTPStatus int
	Code       int
	Msg        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ilink: %s (http=%d code=%d)", e.Msg, e.HTTPStatus, e.Code)
}

type ilinkClient struct{ http *http.Client }

func newIlinkClient() *ilinkClient {
	return &ilinkClient{http: &http.Client{Timeout: 60 * time.Second}}
}

func randomUIN() string {
	var b [4]byte
	rand.Read(b[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[:])), 10)))
}

func setCommon(h http.Header) {
	h.Set("iLink-App-Id", "bot")
	h.Set("iLink-App-ClientVersion", "256")
}

func baseInfo() map[string]string {
	return map[string]string{"channel_version": channelVersion, "bot_agent": botAgent}
}

func (c *ilinkClient) post(ctx context.Context, base, path, token string, body map[string]any, timeout time.Duration) (json.RawMessage, error) {
	body["base_info"] = baseInfo()
	data, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", base+path, bytes.NewReader(data))
	setCommon(req.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-WECHAT-UIN", randomUIN())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, &APIError{HTTPStatus: resp.StatusCode, Msg: string(raw)}
	}
	var chk struct {
		Ret     int    `json:"ret"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	json.Unmarshal(raw, &chk)
	if chk.Ret != 0 || chk.ErrCode != 0 {
		code := chk.ErrCode
		if code == 0 {
			code = chk.Ret
		}
		msg := chk.ErrMsg
		if msg == "" {
			msg = fmt.Sprintf("ret=%d", chk.Ret)
		}
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: code, Msg: msg}
	}
	return raw, nil
}

// ---- login ----

type qrCode struct {
	QRCode string `json:"qrcode"`
	ImgURL string `json:"qrcode_img_content"`
}

type qrStatus struct {
	Status       string `json:"status"`
	BotToken     string `json:"bot_token"`
	BotID        string `json:"ilink_bot_id"`
	UserID       string `json:"ilink_user_id"`
	BaseURL      string `json:"baseurl"`
	RedirectHost string `json:"redirect_host"`
}

func (c *ilinkClient) getQRCode(ctx context.Context, localTokens []string) (*qrCode, error) {
	if localTokens == nil {
		localTokens = []string{}
	}
	data, _ := json.Marshal(map[string]any{"local_token_list": localTokens})
	req, _ := http.NewRequestWithContext(ctx, "POST", defaultBaseURL+"/ilink/bot/get_bot_qrcode?bot_type=3", bytes.NewReader(data))
	setCommon(req.Header)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var q qrCode
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		return nil, err
	}
	if q.QRCode == "" {
		return nil, fmt.Errorf("empty qrcode in response")
	}
	return &q, nil
}

func (c *ilinkClient) pollQRStatus(ctx context.Context, base, qr, verifyCode string) (*qrStatus, error) {
	u := base + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qr)
	if verifyCode != "" {
		u += "&verify_code=" + url.QueryEscape(verifyCode)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	setCommon(req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var s qrStatus
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ---- messaging ----

type textItem struct {
	Text string `json:"text"`
}

type msgItem struct {
	Type      int        `json:"type"`
	TextItem  *textItem  `json:"text_item,omitempty"`
	ImageItem *imageItem `json:"image_item,omitempty"`
	VoiceItem *voiceItem `json:"voice_item,omitempty"`
	FileItem  *fileItem  `json:"file_item,omitempty"`
	VideoItem *videoItem `json:"video_item,omitempty"`
}

type wireMsg struct {
	MessageID    int64     `json:"message_id"`
	FromUserID   string    `json:"from_user_id"`
	ToUserID     string    `json:"to_user_id"`
	CreateTimeMs int64     `json:"create_time_ms"`
	MessageType  int       `json:"message_type"`
	ContextToken string    `json:"context_token"`
	ItemList     []msgItem `json:"item_list"`
}

type updates struct {
	Msgs          []wireMsg `json:"msgs"`
	GetUpdatesBuf string    `json:"get_updates_buf"`
}

func (c *ilinkClient) getUpdates(ctx context.Context, base, token, cursor string) (*updates, error) {
	raw, err := c.post(ctx, base, "/ilink/bot/getupdates", token, map[string]any{"get_updates_buf": cursor}, 45*time.Second)
	if err != nil {
		return nil, err
	}
	var u updates
	json.Unmarshal(raw, &u)
	return &u, nil
}

func (c *ilinkClient) sendText(ctx context.Context, base, token, to, contextToken, text string) error {
	msg := map[string]any{
		"from_user_id":  "",
		"to_user_id":    to,
		"client_id":     newID(),
		"message_type":  2,
		"message_state": 2,
		"context_token": contextToken,
		"item_list":     []any{map[string]any{"type": 1, "text_item": map[string]string{"text": text}}},
	}
	_, err := c.post(ctx, base, "/ilink/bot/sendmessage", token, map[string]any{"msg": msg}, 15*time.Second)
	return err
}

func (c *ilinkClient) getTypingTicket(ctx context.Context, base, token, user, contextToken string) (string, error) {
	raw, err := c.post(ctx, base, "/ilink/bot/getconfig", token, map[string]any{"ilink_user_id": user, "context_token": contextToken}, 15*time.Second)
	if err != nil {
		return "", err
	}
	var r struct {
		TypingTicket string `json:"typing_ticket"`
	}
	json.Unmarshal(raw, &r)
	return r.TypingTicket, nil
}

func (c *ilinkClient) sendTyping(ctx context.Context, base, token, user, ticket string, status int) error {
	_, err := c.post(ctx, base, "/ilink/bot/sendtyping", token, map[string]any{"ilink_user_id": user, "typing_ticket": ticket, "status": status}, 15*time.Second)
	return err
}

func (c *ilinkClient) notifyStart(ctx context.Context, base, token string) error {
	_, err := c.post(ctx, base, "/ilink/bot/msg/notifystart", token, map[string]any{}, 15*time.Second)
	return err
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// textOf flattens an inbound message to plain text.
func textOf(m wireMsg) string {
	var out string
	for _, it := range m.ItemList {
		var s string
		switch it.Type {
		case 1:
			if it.TextItem != nil {
				s = it.TextItem.Text
			}
		case 2:
			s = "[图片]"
		case 3:
			if it.VoiceItem != nil && it.VoiceItem.Text != "" {
				s = it.VoiceItem.Text
			} else {
				s = "[语音]"
			}
		case 4:
			s = "[文件]"
			if it.FileItem != nil && it.FileItem.FileName != "" {
				s = "[文件 " + it.FileItem.FileName + "]"
			}
		case 5:
			s = "[视频]"
		}
		if s != "" {
			if out != "" {
				out += "\n"
			}
			out += s
		}
	}
	return out
}
