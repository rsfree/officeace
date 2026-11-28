// Package account 管理上游账号凭证：解析 JWT、维护池状态、选号与冷却。
package account

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Claims 是从 access_token 中解出的关键字段。
type Claims struct {
	SID string `json:"sid"`
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
	Iss string `json:"iss"`
	JTI string `json:"jti"`
}

// ParseToken 解析 JWT payload（不校验签名，签名由上游校验）。
func ParseToken(token string) (Claims, error) {
	var c Claims
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, errors.New("token 不是合法 JWT（应为 3 段）")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return c, fmt.Errorf("payload base64 解码失败: %w", err)
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, fmt.Errorf("payload JSON 解析失败: %w", err)
	}
	if c.Exp == 0 {
		return c, errors.New("token 缺少 exp 字段")
	}
	return c, nil
}

// Fingerprint 返回 token 的短指纹，用于日志与去重展示。
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

// Account 是可持久化的账号记录。
type Account struct {
	ID        string `json:"id"`    // 上游用户 id（sub），无 sub 时用指纹
	SID       string `json:"sid"`   // 会话 id
	Token     string `json:"token"` // access_token
	ExpiresAt int64  `json:"expires_at"`
	Identity  string `json:"identity,omitempty"` // office_identity
	Note      string `json:"note,omitempty"`
	Source    string `json:"source,omitempty"` // manual | local-sync
	Disabled  bool   `json:"disabled"`
	AddedAt   int64  `json:"added_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Runtime 是进程内状态，不落盘。
type Runtime struct {
	Requests     int64  `json:"requests"`
	Success      int64  `json:"success"`
	Failed       int64  `json:"failed"`
	PromptTokens int64  `json:"prompt_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	LastUsed     int64  `json:"last_used"`
	LastError    string `json:"last_error"`
	LastStatus   int    `json:"last_status"`
	CooldownTill int64  `json:"cooldown_till"`
	Inflight     int    `json:"inflight"`
	State        string `json:"state"` // healthy | cooling | expired | disabled
}

// View 是面板展示用的合并结构（不包含明文 token）。
type View struct {
	ID          string `json:"id"`
	SID         string `json:"sid"`
	ExpiresAt   int64  `json:"expires_at"`
	Identity    string `json:"identity,omitempty"`
	Note        string `json:"note,omitempty"`
	Source      string `json:"source,omitempty"`
	Disabled    bool   `json:"disabled"`
	AddedAt     int64  `json:"added_at"`
	UpdatedAt   int64  `json:"updated_at"`
	Runtime
	TTLSeconds  int64  `json:"ttl_seconds"`
	Fingerprint string `json:"fingerprint"`
}

type sticky struct {
	ID      string
	Expires int64
}

// Pool 是账号池。
type Pool struct {
	mu       sync.RWMutex
	path     string
	// AllowOpaque 允许非 JWT 的不透明凭据（如 Basic 的 app_key:secret），到期时间记 0 = 永不过期。
	AllowOpaque bool
	accounts map[string]*Account
	runtime  map[string]*Runtime
	sticky   map[string]sticky
	rr       uint64
}

// NewPool 加载账号池文件（不存在则视为空池）。
func NewPool(path string) *Pool {
	p := &Pool{
		path:     path,
		accounts: map[string]*Account{},
		runtime:  map[string]*Runtime{},
		sticky:   map[string]sticky{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	var list []*Account
	if json.Unmarshal(raw, &list) != nil {
		return p
	}
	for _, a := range list {
		if a == nil || a.Token == "" {
			continue
		}
		if a.ID == "" {
			a.ID = Fingerprint(a.Token)
		}
		p.accounts[a.ID] = a
		p.runtime[a.ID] = &Runtime{}
	}
	return p
}

// Add 结果语义。
type AddResult struct {
	Account Account `json:"account"`
	Created bool    `json:"created"`
	Updated bool    `json:"updated"`
}

// Add 导入或刷新一个 token。同账号（sub）视为刷新，保留统计与开关。
func (p *Pool) Add(token, note, source, identity string) (AddResult, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		return AddResult{}, errors.New("token 为空")
	}
	claims, err := ParseToken(token)
	if err != nil {
		if !p.AllowOpaque {
			return AddResult{}, err
		}
		// 不透明凭据：无法解析出 sub / exp，用指纹当 id，到期时间 0 表示永不过期。
		claims = Claims{}
	}
	id := claims.Sub
	if id == "" {
		id = Fingerprint(token)
	}
	now := time.Now().Unix()

	p.mu.Lock()
	defer p.mu.Unlock()

	if a, ok := p.accounts[id]; ok {
		changed := a.Token != token || a.ExpiresAt != claims.Exp
		identityChanged := identity != "" && a.Identity != identity
		noteChanged := note != "" && a.Note != note
		sourceChanged := source != "" && a.Source != source
		a.Token = token
		a.SID = claims.SID
		a.ExpiresAt = claims.Exp
		a.UpdatedAt = now
		if identityChanged {
			a.Identity = identity
		}
		if noteChanged {
			a.Note = note
		}
		if sourceChanged {
			a.Source = source
		}
		rt := p.runtime[id]
		if rt == nil {
			rt = &Runtime{}
			p.runtime[id] = rt
		}
		if changed {
			rt.CooldownTill = 0
			rt.LastError = ""
		}
		if changed || identityChanged || noteChanged || sourceChanged {
			if err := p.saveLocked(); err != nil {
				return AddResult{}, err
			}
		}
		return AddResult{Account: *a, Updated: true}, nil
	}

	a := &Account{
		ID: id, SID: claims.SID, Token: token, ExpiresAt: claims.Exp,
		Identity: identity, Note: note, Source: source, AddedAt: now, UpdatedAt: now,
	}
	p.accounts[id] = a
	p.runtime[id] = &Runtime{}
	if err := p.saveLocked(); err != nil {
		return AddResult{}, err
	}
	return AddResult{Account: *a, Created: true}, nil
}

// Remove 删除账号。
func (p *Pool) Remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.accounts[id]; !ok {
		return false
	}
	delete(p.accounts, id)
	delete(p.runtime, id)
	_ = p.saveLocked()
	return true
}

// SetDisabled 启用/禁用账号。
func (p *Pool) SetDisabled(id string, disabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[id]
	if !ok {
		return false
	}
	a.Disabled = disabled
	_ = p.saveLocked()
	return true
}

// SetNote 更新备注。
func (p *Pool) SetNote(id, note string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[id]
	if !ok {
		return false
	}
	a.Note = note
	_ = p.saveLocked()
	return true
}

// List 返回全部账号视图，按到期时间升序。
func (p *Pool) List() []View {
	now := time.Now().Unix()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]View, 0, len(p.accounts))
	for _, a := range p.accounts {
		rt := p.runtime[a.ID]
		if rt == nil {
			rt = &Runtime{}
		}
		cp := *rt
		switch {
		case a.Disabled:
			cp.State = "disabled"
		case a.ExpiresAt > 0 && a.ExpiresAt <= now:
			cp.State = "expired"
		case cp.CooldownTill > now:
			cp.State = "cooling"
		default:
			cp.State = "healthy"
		}
		out = append(out, View{
			ID: a.ID, SID: a.SID, ExpiresAt: a.ExpiresAt, Identity: a.Identity,
			Note: a.Note, Source: a.Source, Disabled: a.Disabled,
			AddedAt: a.AddedAt, UpdatedAt: a.UpdatedAt,
			Runtime: cp, TTLSeconds: a.ExpiresAt - now, Fingerprint: Fingerprint(a.Token),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt < out[j].ExpiresAt })
	return out
}

// Token 返回账号明文凭证（仅服务内部使用）。
func (p *Pool) Token(id string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	a, ok := p.accounts[id]
	if !ok {
		return "", false
	}
	return a.Token, true
}

// Get 返回单个账号视图。
func (p *Pool) Get(id string) (View, bool) {
	for _, v := range p.List() {
		if v.ID == id {
			return v, true
		}
	}
	return View{}, false
}

// ErrNoAccount 表示没有可用账号。
var ErrNoAccount = errors.New("账号池没有可用账号")

// Pick 选出一个可用账号。stickyKey 非空时优先复用绑定账号。
func (p *Pool) Pick(stickyKey string, exclude map[string]bool) (*Account, error) {
	now := time.Now().Unix()
	p.mu.Lock()
	defer p.mu.Unlock()

	if stickyKey != "" {
		if s, ok := p.sticky[stickyKey]; ok {
			if s.Expires > now {
				if a := p.accounts[s.ID]; a != nil && p.usableLocked(a, now) && !exclude[s.ID] {
					p.runtime[a.ID].Inflight++
					return a, nil
				}
			}
			delete(p.sticky, stickyKey)
		}
	}

	type cand struct {
		a *Account
		w float64
	}
	var cands []cand
	for id, a := range p.accounts {
		if exclude[id] || !p.usableLocked(a, now) {
			continue
		}
		rt := p.runtime[id]
		if rt == nil {
			rt = &Runtime{}
			p.runtime[id] = rt
		}
		// 冷却刚结束时优先，最近用过的降权，失败率高的降权，在途多的降权。
		w := 1.0 + rand.Float64()*0.35
		if rt.LastUsed > 0 {
			age := float64(now - rt.LastUsed)
			w *= 0.35 + age/300.0
			if w > 3 {
				w = 3
			}
		}
		if rt.Failed > 0 {
			w *= float64(rt.Success+1) / float64(rt.Success+rt.Failed+1)
		}
		w /= float64(rt.Inflight + 1)
		cands = append(cands, cand{a: a, w: w})
	}
	if len(cands) == 0 {
		return nil, ErrNoAccount
	}
	total := 0.0
	for _, c := range cands {
		total += c.w
	}
	x := rand.Float64() * total
	var chosen *Account
	for _, c := range cands {
		x -= c.w
		if x <= 0 {
			chosen = c.a
			break
		}
	}
	if chosen == nil {
		chosen = cands[len(cands)-1].a
	}
	rt := p.runtime[chosen.ID]
	if rt == nil {
		rt = &Runtime{}
		p.runtime[chosen.ID] = rt
	}
	rt.Inflight++
	cp := *chosen
	return &cp, nil
}

func (p *Pool) usableLocked(a *Account, now int64) bool {
	if a.Disabled || a.Token == "" {
		return false
	}
	if a.ExpiresAt > 0 && a.ExpiresAt <= now+30 {
		return false
	}
	rt := p.runtime[a.ID]
	if rt != nil && rt.CooldownTill > now {
		return false
	}
	return true
}

// Bind 绑定粘性会话。
func (p *Pool) Bind(stickyKey, id string, ttl time.Duration) {
	if stickyKey == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().Unix()
	if len(p.sticky) > 4096 {
		for k, v := range p.sticky {
			if v.Expires <= now {
				delete(p.sticky, k)
			}
		}
	}
	p.sticky[stickyKey] = sticky{ID: id, Expires: time.Now().Add(ttl).Unix()}
}

// Release 释放租约并记录结果。
func (p *Pool) Release(id string, status int, promptTok, outTok int, errMsg string, cooldown time.Duration) {
	now := time.Now().Unix()
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.runtime[id]
	if rt == nil {
		rt = &Runtime{}
		p.runtime[id] = rt
	}
	if rt.Inflight > 0 {
		rt.Inflight--
	}
	rt.Requests++
	rt.LastUsed = now
	rt.LastStatus = status
	if errMsg != "" {
		rt.Failed++
		rt.LastError = errMsg
		if cooldown > 0 {
			rt.CooldownTill = now + int64(cooldown.Seconds())
		}
	} else {
		rt.Success++
		rt.LastError = ""
	}
	rt.PromptTokens += int64(promptTok)
	rt.OutputTokens += int64(outTok)
}

// Cool 主动让账号进入冷却。
func (p *Pool) Cool(id string, d time.Duration, reason string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.runtime[id]
	if rt == nil {
		rt = &Runtime{}
		p.runtime[id] = rt
	}
	rt.CooldownTill = time.Now().Add(d).Unix()
	rt.LastError = reason
	rt.LastStatus = status
}

// Snapshot 返回汇总统计。
type Snapshot struct {
	Total        int   `json:"total"`
	Healthy      int   `json:"healthy"`
	Cooling      int   `json:"cooling"`
	Expired      int   `json:"expired"`
	Disabled     int   `json:"disabled"`
	Requests     int64 `json:"requests"`
	Success      int64 `json:"success"`
	Failed       int64 `json:"failed"`
	PromptTokens int64 `json:"prompt_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Snapshot 计算池的整体状态。
func (p *Pool) Snapshot() Snapshot {
	var s Snapshot
	for _, v := range p.List() {
		s.Total++
		switch v.State {
		case "healthy":
			s.Healthy++
		case "cooling":
			s.Cooling++
		case "expired":
			s.Expired++
		case "disabled":
			s.Disabled++
		}
		s.Requests += v.Requests
		s.Success += v.Success
		s.Failed += v.Failed
		s.PromptTokens += v.PromptTokens
		s.OutputTokens += v.OutputTokens
	}
	return s
}

func (p *Pool) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	list := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ExpiresAt < list[j].ExpiresAt })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}
