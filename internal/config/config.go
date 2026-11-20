// Package config 负责配置的加载、默认值填充、热更新与落盘。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Model 描述一个对外暴露的模型。
type Model struct {
	ID       string `json:"id"`                 // 对外模型名，如 sn-kimi-k3
	Upstream string `json:"upstream,omitempty"` // 上游模型名，缺省同 ID
	Label    string `json:"label,omitempty"`    // 面板展示名
	Kind     string `json:"kind,omitempty"`     // chat | image
	Enabled  bool   `json:"enabled"`
}

// Config 是全部可热更新的运行参数。
type Config struct {
	Listen         string   `json:"listen"`
	APIKey         string   `json:"api_key"`
	PanelKey       string   `json:"panel_key"`
	UpstreamBase   string   `json:"upstream_base"`
	ImageEndpoint  string   `json:"image_endpoint"`
	DefaultModel   string   `json:"default_model"`
	Models         []Model  `json:"models"`
	SyncLocal      bool     `json:"sync_local"`
	LocalAuthPaths []string `json:"local_auth_paths"`
	SyncInterval   int      `json:"sync_interval_sec"`
	MaxRetries     int      `json:"max_retries"`
	CooldownRate   int      `json:"cooldown_rate_limit_sec"`
	CooldownError  int      `json:"cooldown_error_sec"`
	StickyTTL      int      `json:"sticky_ttl_sec"`
	RequestTimeout int      `json:"request_timeout_sec"`
	LogCapacity    int      `json:"log_capacity"`
	UpstreamStream bool     `json:"upstream_always_stream"`
}

type Manager struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

var defaultModels = []Model{
	{ID: "sn-kimi-k3", Label: "Kimi K3", Kind: "chat", Enabled: true},
	{ID: "sn-deepseek-v4-1-flash-1017", Label: "DeepSeek V4 Flash", Kind: "chat", Enabled: true},
	{ID: "sn-image", Label: "图像生成", Kind: "image", Enabled: true},
}

// Default 返回带随机密钥的默认配置。
func Default() Config {
	return Config{
		Listen:        "127.0.0.1:8787",
		APIKey:        "sk-" + randomHex(16),
		PanelKey:      "pk-" + randomHex(12),
		UpstreamBase:  "https://xiaohuanxiong.com/api/web/llm/v2",
		ImageEndpoint: "https://xiaohuanxiong.com/api/web/llm/v2/images/gen",
		DefaultModel:  "sn-kimi-k3",
		Models:        defaultModels,
		SyncLocal:     true,
		LocalAuthPaths: []string{
			"~/Library/Application Support/office-raccoon/agent/config/auth.json",
			"~/Library/Application Support/*/agent/config/auth.json",
			"~/Library/Application Support/*/auth.json",
		},
		SyncInterval:   30,
		MaxRetries:     2,
		CooldownRate:   60,
		CooldownError:  30,
		StickyTTL:      1800,
		RequestTimeout: 600,
		LogCapacity:    500,
	}
}

// Load 读取配置文件，不存在则写入默认值。
func Load(path string) *Manager {
	m := &Manager{path: path, cfg: Default()}
	raw, err := os.ReadFile(path)
	if err == nil {
		var c Config
		if json.Unmarshal(raw, &c) == nil {
			m.cfg = normalize(c, m.cfg)
		}
	} else {
		_ = m.saveLocked()
	}
	return m
}

func normalize(c Config, def Config) Config {
	if c.Listen == "" {
		c.Listen = def.Listen
	}
	if c.APIKey == "" {
		c.APIKey = def.APIKey
	}
	if c.PanelKey == "" {
		c.PanelKey = def.PanelKey
	}
	if c.UpstreamBase == "" {
		c.UpstreamBase = def.UpstreamBase
	}
	if c.ImageEndpoint == "" {
		c.ImageEndpoint = def.ImageEndpoint
	}
	if len(c.Models) == 0 {
		c.Models = def.Models
	}
	if c.DefaultModel == "" {
		c.DefaultModel = def.DefaultModel
	}
	if len(c.LocalAuthPaths) == 0 {
		c.LocalAuthPaths = def.LocalAuthPaths
	}
	if c.SyncInterval <= 0 {
		c.SyncInterval = def.SyncInterval
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = def.MaxRetries
	}
	if c.CooldownRate <= 0 {
		c.CooldownRate = def.CooldownRate
	}
	if c.CooldownError <= 0 {
		c.CooldownError = def.CooldownError
	}
	if c.StickyTTL <= 0 {
		c.StickyTTL = def.StickyTTL
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = def.RequestTimeout
	}
	if c.LogCapacity <= 0 {
		c.LogCapacity = def.LogCapacity
	}
	return c
}

// Get 返回配置快照（值拷贝，含切片克隆）。
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := m.cfg
	c.Models = append([]Model(nil), m.cfg.Models...)
	c.LocalAuthPaths = append([]string(nil), m.cfg.LocalAuthPaths...)
	return c
}

// Update 用函数修改配置并落盘。
func (m *Manager) Update(fn func(*Config)) (Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.cfg)
	m.cfg = normalize(m.cfg, Default())
	if err := m.saveLocked(); err != nil {
		return m.cfg, err
	}
	c := m.cfg
	c.Models = append([]Model(nil), m.cfg.Models...)
	c.LocalAuthPaths = append([]string(nil), m.cfg.LocalAuthPaths...)
	return c, nil
}

func (m *Manager) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// ExpandPath 展开 ~ 与通配符（调用方自行处理多命中）。
func ExpandPath(p string) []string {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if strings.ContainsAny(p, "*?[") {
		matches, _ := filepath.Glob(p)
		return matches
	}
	return []string{p}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "fallback00000000"
	}
	return hex.EncodeToString(b)
}
