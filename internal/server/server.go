// Package server 提供 HTTP 前门（OpenAI 兼容）与管理面板 API。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"officeace2api/internal/account"
	"officeace2api/internal/config"
	"officeace2api/internal/upstream"
	"officeace2api/internal/web"
)

// LogEntry 是一条请求日志。
type LogEntry struct {
	Time       int64  `json:"time"`
	IP         string `json:"ip"`
	UA         string `json:"ua"`
	Path       string `json:"path"`
	Model      string `json:"model"`
	Account    string `json:"account"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	TTFBMS     int64  `json:"ttfb_ms"`
	Prompt     int    `json:"prompt_tokens"`
	Output     int    `json:"output_tokens"`
	Stream     bool   `json:"stream"`
	Error      string `json:"error,omitempty"`
}

// LogStore 是定长环形日志。
type LogStore struct {
	mu   sync.RWMutex
	buf  []LogEntry
	next int
	full bool
	cap  int
}

// NewLogStore 创建日志存储。
func NewLogStore(capacity int) *LogStore {
	if capacity <= 0 {
		capacity = 500
	}
	return &LogStore{buf: make([]LogEntry, capacity), cap: capacity}
}

// Add 追加一条日志。
func (l *LogStore) Add(e LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf[l.next] = e
	l.next = (l.next + 1) % l.cap
	if l.next == 0 {
		l.full = true
	}
}

// List 返回按时间倒序的日志。
func (l *LogStore) List(limit int) []LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := l.next
	if l.full {
		n = l.cap
	}
	out := make([]LogEntry, 0, n)
	for i := 0; i < n; i++ {
		idx := (l.next - 1 - i + l.cap*2) % l.cap
		out = append(out, l.buf[idx])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Version 由 main 注入，用于 healthz 与面板展示。
var Version = "dev"

// Server 聚合全部依赖。
type Server struct {
	cfg     *config.Manager
	pool    *account.Pool
	up      *upstream.Client
	logs    *LogStore
	started time.Time

	reqTotal  atomic.Int64
	reqOK     atomic.Int64
	reqFail   atomic.Int64
	tokensIn  atomic.Int64
	tokensOut atomic.Int64
}

// New 构造 Server。
func New(cfg *config.Manager, pool *account.Pool) *Server {
	// Basic 认证的上游用不透明凭据（app_key:secret），不是 JWT，需要放开账号准入。
	pool.AllowOpaque = cfg.Get().UpstreamAuth == upstream.AuthBasic
	return &Server{
		cfg:     cfg,
		pool:    pool,
		up:      upstream.New(),
		logs:    NewLogStore(cfg.Get().LogCapacity),
		started: time.Now(),
	}
}

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"ok": true, "service": "officeace2api", "version": Version, "uptime": int64(time.Since(s.started).Seconds()),
			"accounts": s.pool.Snapshot(),
		})
	})

	// OpenAI 兼容前门
	mux.HandleFunc("POST /v1/chat/completions", s.authAPI(s.handleChat))
	mux.HandleFunc("GET /v1/models", s.authAPI(s.handleModels))
	mux.HandleFunc("POST /v1/images/generations", s.authAPI(s.handleImages))
	mux.HandleFunc("POST /v1/messages", s.authAPI(s.handleAnthropic))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.authAPI(s.handleCountTokens))

	// 面板 API
	mux.HandleFunc("GET /panel/api/overview", s.authPanel(s.panelOverview))
	mux.HandleFunc("GET /panel/api/accounts", s.authPanel(s.panelAccounts))
	mux.HandleFunc("POST /panel/api/accounts", s.authPanel(s.panelAddAccount))
	mux.HandleFunc("DELETE /panel/api/accounts/{id}", s.authPanel(s.panelDeleteAccount))
	mux.HandleFunc("POST /panel/api/accounts/{id}/toggle", s.authPanel(s.panelToggleAccount))
	mux.HandleFunc("POST /panel/api/accounts/{id}/note", s.authPanel(s.panelNoteAccount))
	mux.HandleFunc("POST /panel/api/accounts/{id}/check", s.authPanel(s.panelCheckAccount))
	mux.HandleFunc("POST /panel/api/import-local", s.authPanel(s.panelImportLocal))
	mux.HandleFunc("GET /panel/api/local-creds", s.authPanel(s.panelScanLocal))
	mux.HandleFunc("GET /panel/api/logs", s.authPanel(s.panelLogs))
	mux.HandleFunc("GET /panel/api/config", s.authPanel(s.panelGetConfig))
	mux.HandleFunc("PUT /panel/api/config", s.authPanel(s.panelPutConfig))
	mux.HandleFunc("POST /panel/api/test/chat", s.authPanel(s.panelTestChat))

	// 静态面板 + 兜底：未注册的路径一律返回 OpenAI 风格 404，
	// 而不是 mux 因「路径匹配但方法不符」给出的 405（那会让客户端误以为端点存在）
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(web.Index())
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"message": "未知端点: " + r.Method + " " + r.URL.Path,
			"type":    "invalid_request_error",
			"code":    "unknown_endpoint",
		}})
	})

	return s.recoverer(mux)
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) authAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Get()
		token := bearer(r)
		if token == "" {
			token = r.Header.Get("x-api-key")
		}
		if token == "" {
			token = r.URL.Query().Get("key")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.APIKey)) != 1 {
			writeJSON(w, 401, map[string]any{"error": map[string]any{
				"message": "invalid api key", "type": "invalid_request_error", "code": "invalid_api_key",
			}})
			return
		}
		next(w, r)
	}
}

func (s *Server) authPanel(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Get()
		key := r.Header.Get("X-Panel-Key")
		if key == "" {
			key = bearer(r)
		}
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(cfg.PanelKey)) != 1 {
			writeJSON(w, 401, map[string]any{"error": "面板密钥无效"})
			return
		}
		next(w, r)
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func timeoutCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
