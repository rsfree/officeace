package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"officeace2api/internal/account"
	"officeace2api/internal/config"
	"officeace2api/internal/upstream"
)

func (s *Server) panelOverview(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	snap := s.pool.Snapshot()
	writeJSON(w, 200, map[string]any{
		"pool":         snap,
		"requests":     s.reqTotal.Load(),
		"success":      s.reqOK.Load(),
		"failed":       s.reqFail.Load(),
		"tokens_in":    s.tokensIn.Load(),
		"tokens_out":   s.tokensOut.Load(),
		"uptime":       int64(time.Since(s.started).Seconds()),
		"upstream":     cfg.UpstreamBase,
		"listen":       cfg.Listen,
		"models":       cfg.Models,
		"default":      cfg.DefaultModel,
		"sync_local":   cfg.SyncLocal,
		"started_at":   s.started.Unix(),
		"service":      "officeace2api",
		"api_key_tail": tail(cfg.APIKey, 6),
		"panel_key_tail": tail(cfg.PanelKey, 6),
	})
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func (s *Server) panelAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"data": s.pool.List()})
}

type addAccountReq struct {
	Token    string `json:"token"`
	Note     string `json:"note"`
	Identity string `json:"identity"`
}

func (s *Server) panelAddAccount(w http.ResponseWriter, r *http.Request) {
	var req addAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败"})
		return
	}
	// 支持一次粘贴多个 token（换行/逗号分隔）
	parts := strings.FieldsFunc(req.Token, func(r rune) bool {
		return r == '\n' || r == ',' || r == ';' || r == ' '
	})
	if len(parts) == 0 {
		writeJSON(w, 400, map[string]any{"error": "token 为空"})
		return
	}
	var added, updated int
	var errs []string
	for _, p := range parts {
		p = strings.TrimSpace(strings.TrimPrefix(p, "Bearer "))
		if len(p) < 40 {
			continue
		}
		res, err := s.pool.Add(p, req.Note, "manual", req.Identity)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if res.Created {
			added++
		}
		if res.Updated {
			updated++
		}
	}
	writeJSON(w, 200, map[string]any{"added": added, "updated": updated, "errors": errs})
}

func (s *Server) panelDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.pool.Remove(id) {
		writeJSON(w, 404, map[string]any{"error": "账号不存在"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) panelToggleAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Disabled bool `json:"disabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !s.pool.SetDisabled(id, req.Disabled) {
		writeJSON(w, 404, map[string]any{"error": "账号不存在"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) panelNoteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !s.pool.SetNote(id, req.Note) {
		writeJSON(w, 404, map[string]any{"error": "账号不存在"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// panelCheckAccount 用最小对话请求验证凭证是否仍被上游接受（会消耗个位数 token）。
func (s *Server) panelCheckAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token, ok := s.pool.Token(id)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "账号不存在"})
		return
	}
	cfg := s.cfg.Get()
	payload, _ := json.Marshal(map[string]any{
		"model":      s.firstChatModel(),
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	ctx, cancel := timeoutCtx(r, 30*time.Second)
	defer cancel()
	resp, err := s.up.Post(ctx, strings.TrimRight(cfg.UpstreamBase, "/")+"/chat/completions", token, payload)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if resp.StatusCode >= 300 {
		ue := upstream.ReadError(resp)
		cool, reason := upstream.Classify(ue.Status, ue.Code)
		s.pool.Cool(id, cool, reason+": "+ue.Message, ue.Status)
		writeJSON(w, 200, map[string]any{"ok": false, "status": ue.Status, "error": ue.Message, "reason": reason})
		return
	}
	resp.Body.Close()
	s.pool.Cool(id, 0, "", 200)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) firstChatModel() string {
	cfg := s.cfg.Get()
	for _, m := range cfg.Models {
		if m.Enabled && (m.Kind == "" || m.Kind == "chat") {
			if m.Upstream != "" {
				return m.Upstream
			}
			return m.ID
		}
	}
	return cfg.DefaultModel
}

// LocalCred 是本机发现的凭证。
type LocalCred struct {
	Path       string `json:"path"`
	Token      string `json:"-"`
	Fingerprint string `json:"fingerprint"`
	SID        string `json:"sid"`
	UserID     string `json:"user_id"`
	ExpiresAt  int64  `json:"expires_at"`
	Identity   string `json:"identity"`
	Valid      bool   `json:"valid"`
	ParseError string `json:"parse_error,omitempty"`
}

// scanLocal 扫描客户端落盘的 auth.json。
func (s *Server) scanLocal() []LocalCred {
	cfg := s.cfg.Get()
	seen := map[string]bool{}
	var out []LocalCred
	for _, pattern := range cfg.LocalAuthPaths {
		for _, path := range config.ExpandPath(pattern) {
			if seen[path] {
				continue
			}
			seen[path] = true
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var f struct {
				AccessToken    string `json:"access_token"`
				OfficeIdentity string `json:"office_identity"`
			}
			if json.Unmarshal(raw, &f) != nil || f.AccessToken == "" {
				continue
			}
			cred := LocalCred{
				Path: path, Token: f.AccessToken, Fingerprint: account.Fingerprint(f.AccessToken),
				Identity: f.OfficeIdentity,
			}
			if c, err := account.ParseToken(f.AccessToken); err == nil {
				cred.SID, cred.UserID, cred.ExpiresAt = c.SID, c.Sub, c.Exp
				cred.Valid = c.Exp > time.Now().Unix()
			} else {
				cred.ParseError = err.Error()
			}
			out = append(out, cred)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt > out[j].ExpiresAt })
	return out
}

func (s *Server) panelScanLocal(w http.ResponseWriter, r *http.Request) {
	creds := s.scanLocal()
	for i := range creds {
		creds[i].Token = ""
	}
	writeJSON(w, 200, map[string]any{"data": creds})
}

func (s *Server) panelImportLocal(w http.ResponseWriter, r *http.Request) {
	creds := s.scanLocal()
	var added, updated int
	var errs []string
	for _, c := range creds {
		res, err := s.pool.Add(c.Token, "本机 OfficeAce", "local-sync", c.Identity)
		if err != nil {
			errs = append(errs, filepath.Base(c.Path)+": "+err.Error())
			continue
		}
		if res.Created {
			added++
		}
		if res.Updated {
			updated++
		}
	}
	writeJSON(w, 200, map[string]any{"scanned": len(creds), "added": added, "updated": updated, "errors": errs})
}

func (s *Server) panelLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	writeJSON(w, 200, map[string]any{"data": s.logs.List(limit)})
}

func (s *Server) panelGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.cfg.Get())
}

type cfgPatch struct {
	DefaultModel   *string       `json:"default_model"`
	Models         *[]config.Model `json:"models"`
	SyncLocal      *bool         `json:"sync_local"`
	LocalAuthPaths *[]string     `json:"local_auth_paths"`
	MaxRetries     *int          `json:"max_retries"`
	CooldownRate   *int          `json:"cooldown_rate_limit_sec"`
	CooldownError  *int          `json:"cooldown_error_sec"`
	StickyTTL      *int          `json:"sticky_ttl_sec"`
	RequestTimeout *int          `json:"request_timeout_sec"`
	SyncInterval   *int          `json:"sync_interval_sec"`
	UpstreamBase   *string       `json:"upstream_base"`
	APIKey         *string       `json:"api_key"`
	PanelKey       *string       `json:"panel_key"`
}

func (s *Server) panelPutConfig(w http.ResponseWriter, r *http.Request) {
	var p cfgPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败"})
		return
	}
	cfg, err := s.cfg.Update(func(c *config.Config) {
		if p.DefaultModel != nil && *p.DefaultModel != "" {
			c.DefaultModel = *p.DefaultModel
		}
		if p.Models != nil {
			c.Models = *p.Models
		}
		if p.SyncLocal != nil {
			c.SyncLocal = *p.SyncLocal
		}
		if p.LocalAuthPaths != nil {
			c.LocalAuthPaths = *p.LocalAuthPaths
		}
		if p.MaxRetries != nil {
			c.MaxRetries = *p.MaxRetries
		}
		if p.CooldownRate != nil {
			c.CooldownRate = *p.CooldownRate
		}
		if p.CooldownError != nil {
			c.CooldownError = *p.CooldownError
		}
		if p.StickyTTL != nil {
			c.StickyTTL = *p.StickyTTL
		}
		if p.RequestTimeout != nil {
			c.RequestTimeout = *p.RequestTimeout
		}
		if p.SyncInterval != nil && *p.SyncInterval > 0 {
			c.SyncInterval = *p.SyncInterval
		}
		if p.UpstreamBase != nil && *p.UpstreamBase != "" {
			c.UpstreamBase = strings.TrimRight(*p.UpstreamBase, "/")
		}
		if p.APIKey != nil && len(*p.APIKey) >= 8 {
			c.APIKey = *p.APIKey
		}
		if p.PanelKey != nil && len(*p.PanelKey) >= 8 {
			c.PanelKey = *p.PanelKey
		}
	})
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, cfg)
}

func (s *Server) panelTestChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败"})
		return
	}
	if req.Prompt == "" {
		writeJSON(w, 400, map[string]any{"error": "prompt 为空"})
		return
	}
	model := req.Model
	if model == "" {
		model = s.cfg.Get().DefaultModel
	}
	upModel, kind, ok := s.resolveModel(model)
	if !ok || kind != "chat" {
		writeJSON(w, 400, map[string]any{"error": "模型不可用"})
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      upModel,
		"messages":   []map[string]any{{"role": "user", "content": req.Prompt}},
		"max_tokens": 64,
		"stream":     false,
	})
	cfg := s.cfg.Get()
	acc, err := s.pool.Pick("", map[string]bool{})
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := timeoutCtx(r, 120*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := s.up.Post(ctx, strings.TrimRight(cfg.UpstreamBase, "/")+"/chat/completions", acc.Token, payload)
	if err != nil {
		s.pool.Release(acc.ID, 0, 0, 0, err.Error(), 10*time.Second)
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	if resp.StatusCode >= 300 {
		ue := upstream.ReadError(resp)
		s.pool.Release(acc.ID, ue.Status, 0, 0, ue.Message, 30*time.Second)
		writeJSON(w, ue.Status, map[string]any{"error": ue.Error()})
		return
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	text := ""
	if len(out.Choices) > 0 {
		text = out.Choices[0].Message.Content
	}
	s.pool.Release(acc.ID, 200, out.Usage.PromptTokens, out.Usage.CompletionTokens, "", 0)
	writeJSON(w, 200, map[string]any{
		"text": text, "model": model, "account": acc.ID,
		"duration_ms": time.Since(start).Milliseconds(),
		"usage":       out.Usage,
	})
}

// StartLocalSync 后台定期把本机 OfficeAce 客户端的登录态同步进账号池。
func (s *Server) StartLocalSync(ctx context.Context) {
	go func() {
		s.syncLocalOnce()
		for {
			d := time.Duration(s.cfg.Get().SyncInterval) * time.Second
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
				s.syncLocalOnce()
			}
		}
	}()
}

func (s *Server) syncLocalOnce() {
	if !s.cfg.Get().SyncLocal {
		return
	}
	for _, c := range s.scanLocal() {
		if !c.Valid {
			continue
		}
		if _, err := s.pool.Add(c.Token, "本机 OfficeAce", "local-sync", c.Identity); err == nil {
			continue
		}
	}
}
