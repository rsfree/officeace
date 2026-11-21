package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"net/http"

	"officeace2api/internal/account"
	"officeace2api/internal/config"
	"officeace2api/internal/upstream"
)

const maxBody = 64 << 20

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, maxBody))
}

// resolveModel 把对外模型名映射到上游模型名与类型。
// defaultUpstreamOfKind 取某类型下第一个启用条目的上游名。
// 用于「端点优先」的兜底：客户端传了个不该出现在这个端点上的模型名时，
// 用该端点的默认模型接住，而不是把错误的名字转给上游。
func (s *Server) defaultUpstreamOfKind(kind, fallback string) string {
	cfg := s.cfg.Get()
	for _, m := range cfg.Models {
		if m.Enabled && m.Kind == kind {
			if m.Upstream != "" {
				return m.Upstream
			}
			return m.ID
		}
	}
	return fallback
}

func (s *Server) resolveModel(id string) (string, string, bool) {
	cfg := s.cfg.Get()
	pick := func(m config.Model) (string, string) {
		up := m.Upstream
		if up == "" {
			up = m.ID
		}
		kind := m.Kind
		if kind == "" {
			kind = "chat"
		}
		return up, kind
	}

	// 1) 精确命中 ID（最快路径，也是唯一保证不被别名覆盖的）
	for _, m := range cfg.Models {
		if m.Enabled && m.ID == id {
			up, kind := pick(m)
			return up, kind, true
		}
	}
	// 2) 精确命中别名
	for _, m := range cfg.Models {
		if !m.Enabled {
			continue
		}
		for _, a := range m.Aliases {
			if a == id {
				up, kind := pick(m)
				return up, kind, true
			}
		}
	}
	// 3) 通配别名（gpt-image-* 之类）。放在最后，避免抢走精确匹配。
	for _, m := range cfg.Models {
		if !m.Enabled {
			continue
		}
		for _, a := range m.Aliases {
			if !strings.Contains(a, "*") {
				continue
			}
			if ok, err := path.Match(a, id); err == nil && ok {
				up, kind := pick(m)
				return up, kind, true
			}
		}
	}
	return "", "", false
}

func stickyKeyOf(body map[string]any, r *http.Request) string {
	if v := r.Header.Get("X-Conversation-Id"); v != "" {
		return "h:" + v
	}
	if md, ok := body["metadata"].(map[string]any); ok {
		if v, ok := md["conversation_id"].(string); ok && v != "" {
			return "m:" + v
		}
	}
	if v, ok := body["conversation_id"].(string); ok && v != "" {
		return "b:" + v
	}
	if v, ok := body["user"].(string); ok && v != "" {
		return "u:" + v
	}
	return ""
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	raw, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, oaiErr("读取请求体失败: "+err.Error()))
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, 400, oaiErr("请求体不是合法 JSON"))
		return
	}
	modelID, _ := body["model"].(string)
	if modelID == "" {
		modelID = s.cfg.Get().DefaultModel
	}
	upModel, kind, ok := s.resolveModel(modelID)
	if !ok {
		writeJSON(w, 404, oaiErr("未知模型: "+modelID))
		return
	}
	if kind != "chat" {
		writeJSON(w, 400, oaiErr("模型 "+modelID+" 不是对话模型，请用 /v1/images/generations"))
		return
	}
	body["model"] = upModel

	stream, _ := body["stream"].(bool)
	wantUsage := false
	if so, ok := body["stream_options"].(map[string]any); ok {
		wantUsage, _ = so["include_usage"].(bool)
	}
	if stream && !wantUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	out, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, 500, oaiErr("序列化请求失败"))
		return
	}

	cfg := s.cfg.Get()
	sticky := stickyKeyOf(body, r)
	exclude := map[string]bool{}
	var lastErr error

	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		acc, err := s.pool.Pick(sticky, exclude)
		if err != nil {
			lastErr = err
			break
		}
		exclude[acc.ID] = true

		ctx, cancel := timeoutCtx(r, time.Duration(cfg.RequestTimeout)*time.Second)
		resp, err := s.up.Post(ctx, strings.TrimRight(cfg.UpstreamBase, "/")+"/chat/completions", acc.Token, out)
		if err != nil {
			cancel()
			s.pool.Release(acc.ID, 0, 0, 0, "连接失败: "+err.Error(), 10*time.Second)
			lastErr = err
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			ue := upstream.ReadError(resp)
			cancel()
			cool, reason := upstream.Classify(ue.Status, ue.Code)
			s.pool.Release(acc.ID, ue.Status, 0, 0, reason+": "+ue.Message, cool)
			lastErr = ue
			if upstream.IsRetryable(ue.Status, ue.Code) && attempt < cfg.MaxRetries {
				s.logs.Add(LogEntry{
					Time: time.Now().Unix(), IP: clientIP(r), UA: r.UserAgent(),
					Path: "/v1/chat/completions", Model: modelID, Account: acc.ID,
					Status: ue.Status, DurationMS: time.Since(start).Milliseconds(), Stream: stream,
					Error: "换号重试: " + ue.Error(),
				})
				continue
			}
			s.record(start, r, "/v1/chat/completions", modelID, acc.ID, ue.Status, 0, 0, 0, stream, ue.Error())
			writeJSON(w, ue.Status, oaiErr(ue.Error()))
			return
		}

		s.pool.Bind(sticky, acc.ID, time.Duration(cfg.StickyTTL)*time.Second)
		if stream {
			s.proxyStream(w, r, resp, acc, modelID, start, wantUsage, cancel)
			return
		}
		s.proxyPlain(w, r, resp, acc, modelID, start, cancel)
		return
	}

	msg := "没有可用账号"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	status := 503
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		status = ue.Status
	}
	s.record(start, r, "/v1/chat/completions", modelID, "", status, 0, 0, 0, stream, msg)
	writeJSON(w, status, oaiErr(msg))
}

func (s *Server) proxyPlain(w http.ResponseWriter, r *http.Request, resp *http.Response, acc *account.Account, model string, start time.Time, cancel func()) {
	defer cancel()
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		s.pool.Release(acc.ID, 502, 0, 0, "读取上游响应失败", 10*time.Second)
		writeJSON(w, 502, oaiErr("读取上游响应失败: "+err.Error()))
		return
	}
	p, o := parseUsage(string(data))
	s.pool.Release(acc.ID, resp.StatusCode, p, o, "", 0)
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
	s.record(start, r, "/v1/chat/completions", model, acc.ID, resp.StatusCode, p, o, 0, false, "")
}

func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, resp *http.Response, acc *account.Account, model string, start time.Time, wantUsage bool, cancel func()) {
	defer cancel()
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, oaiErr("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	var ttfb int64
	prompt, outTok := 0, 0
	var firstErr string

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload != "" && payload != "[DONE]" {
					if p, o := parseUsage(payload); p > 0 || o > 0 {
						if p > 0 {
							prompt = p
						}
						if o > 0 {
							outTok = o
						}
					}
					if !wantUsage && isUsageOnlyChunk(payload) {
						goto next
					}
					if strings.Contains(payload, `"error"`) && firstErr == "" {
						firstErr = truncateStr(payload, 200)
					}
				}
			}
			if ttfb == 0 && trimmed != "" {
				ttfb = time.Since(start).Milliseconds()
			}
			if _, werr := io.WriteString(w, line); werr != nil {
				break
			}
			flusher.Flush()
		}
	next:
		if err != nil {
			break
		}
	}
	flusher.Flush()

	if firstErr != "" {
		s.pool.Release(acc.ID, 200, prompt, outTok, "流内错误: "+firstErr, 30*time.Second)
	} else {
		s.pool.Release(acc.ID, 200, prompt, outTok, "", 0)
	}
	s.record(start, r, "/v1/chat/completions", model, acc.ID, 200, prompt, outTok, ttfb, true, firstErr)
}

func isUsageOnlyChunk(s string) bool {
	c := strings.ReplaceAll(s, " ", "")
	return strings.Contains(c, `"choices":[]`) || strings.Contains(c, `"choices":null`)
}

func parseUsage(s string) (int, int) {
	p := intField(s, `"prompt_tokens":`)
	o := intField(s, `"completion_tokens":`)
	return p, o
}

func intField(s, key string) int {
	i := strings.Index(s, key)
	if i < 0 {
		return 0
	}
	i += len(key)
	for i < len(s) && (s[i] == ' ' || s[i] == '"') {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if start == i {
		return 0
	}
	n, _ := strconv.Atoi(s[start:i])
	return n
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	data := make([]map[string]any, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if !m.Enabled {
			continue
		}
		data = append(data, map[string]any{
			"id": m.ID, "object": "model", "owned_by": "officeace", "created": 0,
		})
		// 别名也列出来，客户端才能在模型下拉里看到它们；
		// 通配符不是具体模型名，不列。
		for _, a := range m.Aliases {
			if strings.Contains(a, "*") {
				continue
			}
			data = append(data, map[string]any{
				"id": a, "object": "model", "owned_by": "alias:" + m.ID, "created": 0,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	raw, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, oaiErr("读取请求体失败"))
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, 400, oaiErr("请求体不是合法 JSON"))
		return
	}
	modelID, _ := body["model"].(string)
	if modelID == "" {
		modelID = "sn-image"
	}
	// 端点优先：走到 /v1/images/generations 就是出图，model 只是「用哪个上游模型」。
	// 因此不按 kind 做准入 —— 名字没指向图片模型（未注册 / 注册成 chat）时，
	// 回落到默认图片模型，而不是把 chat 模型名发给图片端点（那必然上游 400）。
	upModel, kind, ok := s.resolveModel(modelID)
	if !ok || kind != "image" {
		upModel = s.defaultUpstreamOfKind("image", modelID)
	}

	// canonical 契约：image 是字符串引用或引用数组，代表垫图。
	// 没有它 → 文生图；有 → 图生图。同一个端点，靠字段存在性分流。
	refs, refErr := ParseImageRefs(body["image"])
	if refErr != nil {
		writeJSON(w, 400, oaiBadRequest(refErr.Error()))
		return
	}
	delete(body, "image") // 上游不吃该字段，引用已单独解析为字节
	body["model"] = upModel

	cfg := s.cfg.Get()
	acc, err := s.pool.Pick("", map[string]bool{})
	if err != nil {
		writeJSON(w, 503, oaiErr(err.Error()))
		return
	}
	ctx, cancel := timeoutCtx(r, time.Duration(cfg.RequestTimeout)*time.Second)
	defer cancel()

	var resp *http.Response
	if len(refs) > 0 {
		blobs, loadErr := s.loadImageRefs(ctx, refs)
		if loadErr != nil {
			s.pool.Release(acc.ID, 0, 0, 0, "", 0)
			writeJSON(w, 400, oaiBadRequest(loadErr.Error()))
			return
		}
		resp, err = s.postImageEdit(ctx, cfg, acc.Token, body, blobs)
	} else {
		endpoint := cfg.ImageEndpoint
		if endpoint == "" {
			endpoint = strings.TrimRight(cfg.UpstreamBase, "/") + "/images/gen"
		}
		out, _ := json.Marshal(body)
		resp, err = s.up.Post(ctx, endpoint, acc.Token, out)
	}
	if err != nil {
		s.pool.Release(acc.ID, 0, 0, 0, "连接失败", 10*time.Second)
		writeJSON(w, 502, oaiErr("上游连接失败: "+err.Error()))
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ue := upstream.ReadError(resp)
		cool, reason := upstream.Classify(ue.Status, ue.Code)
		s.pool.Release(acc.ID, ue.Status, 0, 0, reason, cool)
		s.record(start, r, "/v1/images/generations", modelID, acc.ID, ue.Status, 0, 0, 0, false, ue.Error())
		writeJSON(w, ue.Status, oaiErr(ue.Error()))
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		s.pool.Release(acc.ID, 502, 0, 0, "读取图片响应失败", 10*time.Second)
		writeJSON(w, 502, oaiErr("读取上游图片失败"))
		return
	}
	// 图片接口的用量在响应顶层 usage 里，字段名与 chat 不同（input_tokens / output_tokens）。
	var meta struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	promptTok, outTok := 0, 0
	if json.Unmarshal(data, &meta) == nil {
		promptTok, outTok = meta.Usage.InputTokens, meta.Usage.OutputTokens
	}
	s.pool.Release(acc.ID, 200, promptTok, outTok, "", 0)
	// 成功体理论上不含内部代号，仍做一次低成本守卫，避免上游改版后漏出。
	if bytes.Contains(bytes.ToLower(data), []byte("raccoon")) {
		data = []byte(upstream.Sanitize(string(data)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(data)
	s.record(start, r, "/v1/images/generations", modelID, acc.ID, 200, promptTok, outTok, 0, false, "")
}

// loadImageRefs 把 canonical 的字符串引用统一取成字节，并守住总量上限。
func (s *Server) loadImageRefs(ctx context.Context, refs []string) ([]*imageBlob, error) {
	blobs := make([]*imageBlob, 0, len(refs))
	total := 0
	for i, ref := range refs {
		blob, err := LoadImageRef(ctx, ref)
		if err != nil {
			return nil, errors.New("image[" + strconv.Itoa(i) + "]: " + err.Error())
		}
		total += len(blob.Data)
		if total > maxTotalRefs {
			return nil, errors.New("垫图合计超过 " + strconv.Itoa(maxTotalRefs>>20) + " MB")
		}
		blobs = append(blobs, blob)
	}
	return blobs, nil
}

// postImageEdit 反向适配：上游只收 multipart 文件流，这里把已取到的字节组装成表单。
func (s *Server) postImageEdit(ctx context.Context, cfg config.Config, token string, body map[string]any, blobs []*imageBlob) (*http.Response, error) {
	fields := map[string]string{"model": fmt.Sprint(body["model"])}
	for _, k := range []string{"prompt", "n", "size", "quality"} {
		if v, ok := body[k]; ok && v != nil {
			fields[k] = fmt.Sprint(v)
		}
	}
	if strings.TrimSpace(fields["prompt"]) == "" {
		fields["prompt"] = "按参考图生成图片"
	}
	form, contentType, err := BuildImageEditForm(fields, blobs)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(cfg.UpstreamBase, "/") + "/images/edits"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, form)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	client := &http.Client{}
	return client.Do(req)
}

func (s *Server) record(start time.Time, r *http.Request, path, model, accID string, status, prompt, out int, ttfb int64, stream bool, errMsg string) {
	s.reqTotal.Add(1)
	if status >= 200 && status < 300 {
		s.reqOK.Add(1)
	} else {
		s.reqFail.Add(1)
	}
	s.tokensIn.Add(int64(prompt))
	s.tokensOut.Add(int64(out))
	ip, ua := "", ""
	if r != nil {
		ip = clientIP(r)
		ua = r.UserAgent()
		if len(ua) > 160 {
			ua = ua[:160]
		}
	}
	s.logs.Add(LogEntry{
		Time: time.Now().Unix(), IP: ip, UA: ua, Path: path, Model: model,
		Account: accID, Status: status, DurationMS: time.Since(start).Milliseconds(),
		TTFBMS: ttfb, Prompt: prompt, Output: out, Stream: stream, Error: errMsg,
	})
}

// oaiBadRequest 用于「调用方字段/引用不合法」的归因：400 + invalid_request_error。
// 不能复用 oaiErr —— 那会把客户端错误标成 upstream_error，让调用方去排查上游。
func oaiBadRequest(msg string) map[string]any {
	return map[string]any{"error": map[string]any{
		"message": upstream.Sanitize(msg),
		"type":    "invalid_request_error",
		"code":    "invalid_image_reference",
	}}
}

func oaiErr(msg string) map[string]any {
	// 兜底：任何来源的错误文本都不允许带出上游内部代号（raccoon-* / LiteLLM）。
	return map[string]any{"error": map[string]any{"message": upstream.Sanitize(msg), "type": "upstream_error"}}
}
