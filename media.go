package main

// Media (image / file / video / voice) over the iLink CDN.
//
// Protocol (reconstructed from Tencent/openclaw-weixin, MIT):
//   outbound: AES-128-ECB encrypt → POST ilink/bot/getuploadurl → POST ciphertext
//             to the CDN → read x-encrypted-param → sendmessage with an
//             image/video/file item referencing it.
//   inbound:  items carry {encrypt_query_param, aes_key}; download from the CDN
//             and decrypt.
// Inbound media is not stored: the Hub keeps only the CDN reference and
// decrypts on demand when an agent fetches <link>/media/<id>.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	cdnBaseURL    = "https://novac2c.cdn.weixin.qq.com/c2c"
	maxMediaBytes = 25 << 20 // outbound limit (same as openclaw-weixin's media cache)
	maxInBytes    = 100 << 20
	mediaTTL      = 7 * 24 * time.Hour
	maxMediaRefs  = 5000
)

// ---------- wire types ----------

type cdnMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param,omitempty"`
	AESKey            string `json:"aes_key,omitempty"`
	EncryptType       int    `json:"encrypt_type,omitempty"`
	FullURL           string `json:"full_url,omitempty"`
}

type imageItem struct {
	Media   *cdnMedia `json:"media,omitempty"`
	AESKey  string    `json:"aeskey,omitempty"` // hex, preferred for inbound images
	MidSize int64     `json:"mid_size,omitempty"`
}

type voiceItem struct {
	Media      *cdnMedia `json:"media,omitempty"`
	Text       string    `json:"text,omitempty"`
	EncodeType int       `json:"encode_type,omitempty"`
	Playtime   int       `json:"playtime,omitempty"`
}

type fileItem struct {
	Media    *cdnMedia `json:"media,omitempty"`
	FileName string    `json:"file_name,omitempty"`
	Len      string    `json:"len,omitempty"`
}

type videoItem struct {
	Media     *cdnMedia `json:"media,omitempty"`
	VideoSize int64     `json:"video_size,omitempty"`
}

// ---------- AES-128-ECB ----------

func aesECBEncrypt(plain, key []byte) []byte {
	b, _ := aes.NewCipher(key)
	pad := 16 - len(plain)%16
	buf := make([]byte, len(plain)+pad)
	copy(buf, plain)
	for i := len(plain); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	for i := 0; i < len(buf); i += 16 {
		b.Encrypt(buf[i:i+16], buf[i:i+16])
	}
	return buf
}

func aesECBDecrypt(ct, key []byte) ([]byte, error) {
	if len(ct) == 0 || len(ct)%16 != 0 {
		return nil, errors.New("ciphertext is not a multiple of the block size")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(ct))
	for i := 0; i < len(ct); i += 16 {
		b.Decrypt(out[i:i+16], ct[i:i+16])
	}
	pad := int(out[len(out)-1])
	if pad < 1 || pad > 16 || pad > len(out) {
		return nil, errors.New("bad padding")
	}
	return out[:len(out)-pad], nil
}

// parseAESKey accepts base64(16 raw bytes) or base64(32-char hex), both seen in the wild.
func parseAESKey(b64 string) ([]byte, error) {
	d, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(d) == 16 {
		return d, nil
	}
	if len(d) == 32 {
		if k, err := hex.DecodeString(string(d)); err == nil {
			return k, nil
		}
	}
	return nil, fmt.Errorf("unexpected aes_key length %d", len(d))
}

// ---------- outbound upload ----------

const (
	uploadImage = 1
	uploadVideo = 2
	uploadFile  = 3
)

var cdnClient = &http.Client{Timeout: 3 * time.Minute}

type uploaded struct {
	param    string // download encrypt_query_param
	aesHex   string
	rawSize  int
	encSize  int
	fileName string
}

func (c *ilinkClient) uploadMedia(ctx context.Context, base, token, to string, mediaType int, data []byte) (*uploaded, error) {
	key := make([]byte, 16)
	rand.Read(key)
	fk := make([]byte, 16)
	rand.Read(fk)
	filekey := hex.EncodeToString(fk)
	sum := md5.Sum(data)
	ct := aesECBEncrypt(data, key)
	raw, err := c.post(ctx, base, "/ilink/bot/getuploadurl", token, map[string]any{
		"filekey":       filekey,
		"media_type":    mediaType,
		"to_user_id":    to,
		"rawsize":       len(data),
		"rawfilemd5":    hex.EncodeToString(sum[:]),
		"filesize":      len(ct),
		"no_need_thumb": true,
		"aeskey":        hex.EncodeToString(key),
	}, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("getuploadurl: %w", err)
	}
	var u struct {
		UploadParam string `json:"upload_param"`
		FullURL     string `json:"upload_full_url"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	target := strings.TrimSpace(u.FullURL)
	if target == "" {
		if u.UploadParam == "" {
			return nil, errors.New("getuploadurl returned no upload url")
		}
		target = cdnBaseURL + "/upload?encrypted_query_param=" + url.QueryEscape(u.UploadParam) + "&filekey=" + url.QueryEscape(filekey)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(ct))
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := cdnClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fmt.Errorf("cdn upload rejected: http %d %s", resp.StatusCode, resp.Header.Get("x-error-message"))
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("cdn upload: http %d %s", resp.StatusCode, resp.Header.Get("x-error-message"))
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		p := resp.Header.Get("x-encrypted-param")
		if p == "" {
			lastErr = errors.New("cdn upload: missing x-encrypted-param")
			continue
		}
		return &uploaded{param: p, aesHex: hex.EncodeToString(key), rawSize: len(data), encSize: len(ct)}, nil
	}
	return nil, lastErr
}

// mediaItem builds the sendmessage item for an uploaded file. Note aes_key is
// base64 of the *hex string*, exactly as openclaw-weixin does.
func mediaItem(kind string, u *uploaded) map[string]any {
	m := map[string]any{
		"encrypt_query_param": u.param,
		"aes_key":             base64.StdEncoding.EncodeToString([]byte(u.aesHex)),
		"encrypt_type":        1,
	}
	switch kind {
	case "image":
		return map[string]any{"type": 2, "image_item": map[string]any{"media": m, "mid_size": u.encSize}}
	case "video":
		return map[string]any{"type": 5, "video_item": map[string]any{"media": m, "video_size": u.encSize}}
	default:
		return map[string]any{"type": 4, "file_item": map[string]any{"media": m, "file_name": u.fileName, "len": strconv.Itoa(u.rawSize)}}
	}
}

func (c *ilinkClient) sendItem(ctx context.Context, base, token, to, contextToken string, item map[string]any) error {
	msg := map[string]any{
		"from_user_id":  "",
		"to_user_id":    to,
		"client_id":     newID(),
		"message_type":  2,
		"message_state": 2,
		"context_token": contextToken,
		"item_list":     []any{item},
	}
	_, err := c.post(ctx, base, "/ilink/bot/sendmessage", token, map[string]any{"msg": msg}, 20*time.Second)
	return err
}

// ---------- outbound file preparation ----------

// OutMedia is a file an agent wants to send to WeChat.
type OutMedia struct {
	Data []byte
	Name string
	Kind string // image | video | file
}

// classify picks how WeChat should show the file. `as` may force a kind.
func classify(name string, data []byte, as string) (kind, mimeType string) {
	mimeType = mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	mimeType, _, _ = strings.Cut(mimeType, ";")
	switch {
	case as == "image" || as == "video" || as == "file":
		kind = as
	case strings.HasPrefix(mimeType, "image/") && mimeType != "image/svg+xml":
		kind = "image"
	case mimeType == "video/mp4" || mimeType == "video/quicktime":
		kind = "video"
	default:
		kind = "file"
	}
	return
}

func newOutMedia(data []byte, name, as string) (*OutMedia, error) {
	if len(data) == 0 {
		return nil, errors.New("file is empty")
	}
	if len(data) > maxMediaBytes {
		return nil, fmt.Errorf("file too large: %d bytes (max %d MB)", len(data), maxMediaBytes>>20)
	}
	kind, mt := classify(name, data, as)
	name = sanitizeFileName(name)
	if name == "" || !strings.Contains(name, ".") {
		ext := ""
		if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
			ext = exts[0]
		}
		if name == "" {
			name = map[string]string{"image": "image", "video": "video"}[kind]
			if name == "" {
				name = "file"
			}
		}
		name += ext
	}
	return &OutMedia{Data: data, Name: name, Kind: kind}, nil
}

func sanitizeFileName(s string) string {
	s = path.Base(strings.ReplaceAll(strings.TrimSpace(s), "\\", "/"))
	if s == "." || s == "/" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 120 {
		s = string(r[len(r)-120:])
	}
	return s
}

// fetchURL downloads a public URL for sending. Private, loopback and
// link-local addresses are refused so the Hub can't be used to reach
// internal services.
var safeClient = &http.Client{
	Timeout: 90 * time.Second,
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(network, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if ip == nil || !publicIP(ip) {
					return fmt.Errorf("refusing to fetch from non-public address %s", host)
				}
				return nil
			},
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("redirect to unsupported scheme")
		}
		return nil
	},
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func publicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!ip.IsUnspecified() && !cgnat.Contains(ip)
}

func fetchURL(ctx context.Context, raw string) (data []byte, name string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", errors.New("file_url must be an http(s) URL")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", botAgent)
	resp, err := safeClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("download failed: http %d", resp.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxMediaBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("download failed: %v", err)
	}
	if len(data) > maxMediaBytes {
		return nil, "", fmt.Errorf("file too large (max %d MB)", maxMediaBytes>>20)
	}
	if _, p, e := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); e == nil && p["filename"] != "" {
		name = p["filename"]
	} else {
		name = path.Base(resp.Request.URL.Path)
		if name == "/" || name == "." {
			name = ""
		}
		if n, e := url.PathUnescape(name); e == nil {
			name = n
		}
	}
	if path.Ext(name) == "" {
		if exts, _ := mime.ExtensionsByType(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])); len(exts) > 0 {
			name += exts[0]
		}
	}
	return data, name, nil
}

// ---------- inbound media references ----------

// Attachment is what agents see in a message's "attachments".
type Attachment struct {
	ID   string `json:"id"`
	Type string `json:"type"` // image | file | video | voice
	Name string `json:"name,omitempty"`
	Size int64  `json:"size,omitempty"`
	URL  string `json:"url"`
}

type mediaRef struct {
	id      string
	acc     *Account
	typ     string
	name    string
	size    int64
	media   cdnMedia
	keyHex  string // image_item.aeskey, if present
	created time.Time
	agents  map[string]bool // lower(agent name) allowed to fetch
}

// extractMedia registers the media items of an inbound message.
func (h *Hub) extractMedia(acc *Account, m wireMsg) []*mediaRef {
	var refs []*mediaRef
	add := func(typ, name string, size int64, md *cdnMedia, keyHex string) {
		if md == nil || (md.EncryptQueryParam == "" && md.FullURL == "") {
			return
		}
		refs = append(refs, &mediaRef{id: genID() + genID(), acc: acc, typ: typ, name: name, size: size,
			media: *md, keyHex: keyHex, created: time.Now(), agents: map[string]bool{}})
	}
	for _, it := range m.ItemList {
		switch it.Type {
		case 2:
			if it.ImageItem != nil {
				add("image", "", 0, it.ImageItem.Media, it.ImageItem.AESKey)
			}
		case 3:
			if it.VoiceItem != nil {
				add("voice", "", 0, it.VoiceItem.Media, "")
			}
		case 4:
			if it.FileItem != nil {
				n, _ := strconv.ParseInt(it.FileItem.Len, 10, 64)
				add("file", sanitizeFileName(it.FileItem.FileName), n, it.FileItem.Media, "")
			}
		case 5:
			if it.VideoItem != nil {
				add("video", "", 0, it.VideoItem.Media, "")
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.media == nil {
		h.media = map[string]*mediaRef{}
	}
	now := time.Now()
	for id, r := range h.media {
		if now.Sub(r.created) > mediaTTL || len(h.media) > maxMediaRefs {
			delete(h.media, id) // expired, or over the cap (arbitrary eviction)
		}
	}
	for _, r := range refs {
		h.media[r.id] = r
	}
	return refs
}

// grantMedia lets agent a fetch refs and returns the attachments it will see.
// Caller holds h.mu.
func (h *Hub) grantMediaLocked(a *Agent, refs []*mediaRef) []Attachment {
	if len(refs) == 0 {
		return nil
	}
	link := h.cfg.PublicURL + "/a/" + a.Key
	out := make([]Attachment, 0, len(refs))
	for _, r := range refs {
		r.agents[strings.ToLower(a.Name)] = true
		out = append(out, Attachment{ID: r.id, Type: r.typ, Name: r.name, Size: r.size, URL: link + "/media/" + r.id})
	}
	return out
}

func (h *Hub) mediaFor(a *Agent, id string) *mediaRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.media[id]
	if r == nil || r.acc != a.acc || !r.agents[strings.ToLower(a.Name)] || time.Since(r.created) > mediaTTL {
		return nil
	}
	return r
}

func downloadInbound(ctx context.Context, r *mediaRef) ([]byte, error) {
	var key []byte
	var err error
	if r.keyHex != "" {
		key, err = hex.DecodeString(r.keyHex)
	} else {
		key, err = parseAESKey(r.media.AESKey)
	}
	if err != nil || len(key) != 16 {
		return nil, fmt.Errorf("bad media key: %v", err)
	}
	u := r.media.FullURL
	if u == "" {
		u = cdnBaseURL + "/download?encrypted_query_param=" + url.QueryEscape(r.media.EncryptQueryParam)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := cdnClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cdn download: http %d", resp.StatusCode)
	}
	ct, err := io.ReadAll(io.LimitReader(resp.Body, maxInBytes+16))
	if err != nil {
		return nil, err
	}
	return aesECBDecrypt(ct, key)
}

func (h *Hub) serveMedia(w http.ResponseWriter, r *http.Request, a *Agent) {
	ref := h.mediaFor(a, r.PathValue("id"))
	if ref == nil {
		errJSON(w, 404, "media not found or expired (media links are kept for 7 days and reset when the Hub restarts)")
		return
	}
	data, err := downloadInbound(r.Context(), ref)
	if err != nil {
		errJSON(w, 502, "could not fetch media from WeChat: "+err.Error())
		return
	}
	name := ref.name
	mt := ""
	if name != "" {
		mt = mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	}
	if mt == "" {
		mt = http.DetectContentType(data)
	}
	if name == "" {
		ext := ""
		switch {
		case ref.typ == "voice":
			ext = ".silk"
			mt = "audio/silk"
		default:
			if exts, _ := mime.ExtensionsByType(strings.Split(mt, ";")[0]); len(exts) > 0 {
				ext = exts[0]
			}
		}
		name = ref.typ + "-" + ref.id[:8] + ext
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(data)
}
