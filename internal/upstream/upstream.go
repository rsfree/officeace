// Package upstream 封装 xiaohuanxiong.com 的 OpenAI 兼容接口。
package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client 是上游 HTTP 客户端。
type Client struct {
	http *http.Client
}

// New 构造带连接池的客户端。
func New() *Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 180 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Client{http: &http.Client{Transport: tr}}
}

// Error 是上游返回的结构化错误。
type Error struct {
	Status  int
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details string `json:"details"`
	Raw     string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("upstream %d: %s (%s)", e.Status, e.Message, e.Details)
	}
	return fmt.Sprintf("upstream %d: %s", e.Status, truncate(e.Raw, 200))
}

// IsAuth 判断是否为凭证失效。
func (e *Error) IsAuth() bool {
	return e.Status == 401 || e.Status == 403 || e.Code == 200003
}

// IsRateLimit 判断是否被限流。
func (e *Error) IsRateLimit() bool {
	return e.Status == 429 || e.Code == 429
}

// Post 发起 JSON POST，返回原始响应。调用方负责关闭 Body。
func (c *Client) Post(ctx context.Context, endpoint, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(token, "Bearer "))
	req.Header.Set("User-Agent", "officeace2api/1.0")
	return c.http.Do(req)
}

// ReadError 把非 2xx 响应读成 *Error。
func ReadError(resp *http.Response) *Error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Status: resp.StatusCode, Raw: string(raw)}
	e.Code, e.Message, e.Details = parseErrorBody(raw)
	return e
}

func parseErrorBody(raw []byte) (int, string, string) {
	s := string(raw)
	code := 0
	msg := ""
	details := ""
	code = jsonInt(s, `"code":`)
	msg = jsonStr(s, `"message":`)
	details = jsonStr(s, `"details":`)
	if msg == "" {
		msg = truncate(strings.TrimSpace(s), 200)
	}
	return code, msg, details
}

func jsonInt(s, key string) int {
	i := strings.Index(s, key)
	if i < 0 {
		return 0
	}
	i += len(key)
	j := i
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	start := j
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if start == j {
		return 0
	}
	n := 0
	for _, ch := range s[start:j] {
		n = n*10 + int(ch-'0')
	}
	return n
}

func jsonStr(s, key string) string {
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	i += len(key)
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i >= len(s) || s[i] != '"' {
		return ""
	}
	i++
	var b strings.Builder
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(s[i+1])
			}
			i += 2
			continue
		}
		if s[i] == '"' {
			break
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// Classify 把非 2xx 响应归类成冷却建议。
func Classify(status int, code int) (cooldown time.Duration, reason string) {
	switch {
	case status == 401 || status == 403 || code == 200003:
		return 30 * time.Minute, "凭证失效"
	case status == 429 || code == 429:
		return 2 * time.Minute, "上游限流"
	case status == 402 || code == 402:
		return 12 * time.Hour, "额度不足"
	case status >= 500:
		return 30 * time.Second, "上游异常"
	default:
		return 0, "上游拒绝"
	}
}

// IsRetryable 判断是否值得换号重试。
func IsRetryable(status int, code int) bool {
	if status == 401 || status == 403 || status == 429 || status >= 500 {
		return true
	}
	if code == 200003 || code == 429 || code == 402 {
		return true
	}
	return false
}

// ErrEmpty 空响应。
var ErrEmpty = errors.New("upstream 返回空响应")
