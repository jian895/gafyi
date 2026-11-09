package main

// Data model and persistence. Everything lives in <data_dir>/hub.json.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------- config ----------

type Config struct {
	Listen         string `json:"listen"`
	DataDir        string `json:"data_dir"`
	AdminToken     string `json:"admin_token"`
	PublicURL      string `json:"public_url"`       // e.g. https://example.com/wxhub — used in agent links
	SendIntervalMs int    `json:"send_interval_ms"` // min gap between outbound messages per account
	WarnAfterSends int    `json:"warn_after_sends"` // keep-alive hint after N pushes without an inbound message
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Listen: "127.0.0.1:8787", DataDir: "/var/lib/gafyi", SendIntervalMs: 1200, WarnAfterSends: 8}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if len(c.AdminToken) < 16 {
		return nil, errors.New("admin_token must be at least 16 characters (generate one with: gafyi genkey)")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	return c, nil
}

// ---------- persisted model ----------

type Creds struct {
	Token   string `json:"token"`
	BaseURL string `json:"base_url"`
	BotID   string `json:"bot_id"`
	UserID  string `json:"user_id"` // the WeChat user who scanned = owner of this account
	SavedAt string `json:"saved_at"`
}

type ConvState struct {
	ContextToken string    `json:"context_token"`
	LastInbound  time.Time `json:"last_inbound"`
	SendsSince   int       `json:"sends_since"`
}

type Agent struct {
	Name           string            `json:"name"`
	Key            string            `json:"key"`
	Kind           string            `json:"kind"` // catalog id
	Mode           string            `json:"mode"` // "webhook" | "link"
	Webhook        string            `json:"webhook,omitempty"`
	WebhookHeaders map[string]string `json:"webhook_headers,omitempty"`
	Created        time.Time         `json:"created"`
	FirstSeen      time.Time         `json:"first_seen,omitempty"`

	acc *Account // runtime back-pointer
}

func (a *Agent) label() string { return "[" + a.Name + "]" }

type Account struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Created time.Time             `json:"created"`
	Creds   *Creds                `json:"creds,omitempty"`
	Cursor  string                `json:"cursor"`
	Convs   map[string]*ConvState `json:"convs"`
	Current string                `json:"current"` // current conversation target (agent name)
	Agents  []*Agent              `json:"agents"`

	// runtime
	boxes      map[string]*inbox // lower(agent name) -> inbox
	seen       map[string]seenInfo
	pollCancel context.CancelFunc
	pollStatus string
	pausedTill time.Time
	login      *loginState
	sendQ      chan *sendReq
	ctx        context.Context
	stop       context.CancelFunc
}

func (acc *Account) agent(name string) *Agent {
	name = strings.TrimLeft(strings.TrimSpace(name), "@＠")
	for _, a := range acc.Agents {
		if strings.EqualFold(a.Name, name) {
			return a
		}
	}
	return nil
}

// currentAgent returns the current target, falling back to the first agent.
func (acc *Account) currentAgent() *Agent {
	if a := acc.agent(acc.Current); a != nil {
		return a
	}
	if len(acc.Agents) > 0 {
		return acc.Agents[0]
	}
	return nil
}

func (acc *Account) owner() string {
	if acc.Creds != nil && acc.Creds.UserID != "" {
		return acc.Creds.UserID
	}
	var best string
	var t time.Time
	for u, c := range acc.Convs {
		if c.LastInbound.After(t) {
			best, t = u, c.LastInbound
		}
	}
	return best
}

type storeFile struct {
	Version  int        `json:"version"`
	Accounts []*Account `json:"accounts"`
}

// ---------- hub ----------

type Hub struct {
	cfg       *Config
	api       *ilinkClient
	mu        sync.Mutex
	accounts  []*Account
	byKey     map[string]*Agent
	nextID    int64
	startedAt time.Time
	media     map[string]*mediaRef // inbound media references, by id
}

func newHub(cfg *Config) (*Hub, error) {
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, err
	}
	h := &Hub{cfg: cfg, api: newIlinkClient(), byKey: map[string]*Agent{}, nextID: time.Now().UnixMilli(), startedAt: time.Now()}
	if err := h.load(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Hub) storePath() string { return filepath.Join(h.cfg.DataDir, "hub.json") }

func (h *Hub) load() error {
	var f storeFile
	b, err := os.ReadFile(h.storePath())
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &f); err != nil {
			return fmt.Errorf("hub.json: %w", err)
		}
	case os.IsNotExist(err):
		acc, err := h.migrateV1()
		if err != nil {
			return fmt.Errorf("migrate v1 data: %w", err)
		}
		if acc != nil {
			f.Accounts = []*Account{acc}
		}
	default:
		return err
	}
	for _, acc := range f.Accounts {
		h.attachLocked(acc)
	}
	h.saveLocked()
	return nil
}

// attachLocked wires up runtime state for a (loaded or new) account.
func (h *Hub) attachLocked(acc *Account) {
	if acc.Convs == nil {
		acc.Convs = map[string]*ConvState{}
	}
	acc.boxes = map[string]*inbox{}
	acc.seen = map[string]seenInfo{}
	acc.pollStatus = "未登录"
	acc.sendQ = make(chan *sendReq, 256)
	acc.ctx, acc.stop = context.WithCancel(context.Background())
	for _, a := range acc.Agents {
		h.attachAgentLocked(acc, a)
	}
	h.accounts = append(h.accounts, acc)
}

func (h *Hub) attachAgentLocked(acc *Account, a *Agent) {
	a.acc = acc
	if kindByID(a.Kind) == nil {
		a.Kind = "generic"
	}
	if a.Mode != "webhook" {
		a.Mode = "link"
	}
	h.byKey[a.Key] = a
	acc.boxes[strings.ToLower(a.Name)] = &inbox{wake: make(chan struct{})}
}

func (h *Hub) saveLocked() {
	b, _ := json.MarshalIndent(storeFile{Version: 2, Accounts: h.accounts}, "", "  ")
	tmp := h.storePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		log.Printf("save: %v", err)
		return
	}
	if err := os.Rename(tmp, h.storePath()); err != nil {
		log.Printf("save: %v", err)
	}
}

func (h *Hub) accountByID(id string) *Account {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (h *Hub) agentByKey(key string) *Agent {
	if key == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byKey[key]
}

func genKey() string {
	var b [24]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func genID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------- v1 migration ----------

// migrateV1 converts the single-account v0.1 files (state.json + agents.json)
// into one account. Existing agent keys are kept so agents need no changes.
func (h *Hub) migrateV1() (*Account, error) {
	statePath := filepath.Join(h.cfg.DataDir, "state.json")
	agentsPath := filepath.Join(h.cfg.DataDir, "agents.json")
	sb, err1 := os.ReadFile(statePath)
	ab, err2 := os.ReadFile(agentsPath)
	if err1 != nil && err2 != nil {
		return nil, nil // fresh install
	}
	acc := &Account{ID: genID(), Name: "我的微信", Created: time.Now(), Convs: map[string]*ConvState{}}
	if err1 == nil {
		var st struct {
			Creds  *Creds                `json:"creds"`
			Cursor string                `json:"cursor"`
			Convs  map[string]*ConvState `json:"convs"`
		}
		if err := json.Unmarshal(sb, &st); err != nil {
			return nil, err
		}
		acc.Creds, acc.Cursor = st.Creds, st.Cursor
		if st.Convs != nil {
			acc.Convs = st.Convs
		}
	}
	if err2 == nil {
		var af struct {
			Default string   `json:"default"`
			Agents  []*Agent `json:"agents"`
		}
		if err := json.Unmarshal(ab, &af); err != nil {
			return nil, err
		}
		acc.Agents, acc.Current = af.Agents, af.Default
	}
	for _, p := range []string{statePath, agentsPath} {
		if _, err := os.Stat(p); err == nil {
			os.Rename(p, p+".v1.bak")
		}
	}
	log.Printf("migrated v0.1 data into account %q (%d agents)", acc.Name, len(acc.Agents))
	return acc, nil
}
