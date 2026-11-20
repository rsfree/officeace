package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"officeace2api/internal/upstream"
)

// ---------- Anthropic Messages 前门 ----------

type aBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source,omitempty"`
}

type aMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type aRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []aMessage      `json:"messages"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Tools         []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
}

func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	raw, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "读取请求体失败"}})
		return
	}
	var req aRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "请求体不是合法 JSON"}})
		return
	}
	modelID := req.Model
	if modelID == "" {
		modelID = s.cfg.Get().DefaultModel
	}
	upModel, kind, ok := s.resolveModel(modelID)
	if !ok || kind != "chat" {
		writeJSON(w, 404, map[string]any{"type": "error", "error": map[string]any{"type": "not_found_error", "message": "未知模型: " + modelID}})
		return
	}

	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if sys := anthropicText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, convertAnthropicMessage(m)...)
	}

	payload := map[string]any{
		"model":    upModel,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.Name, "description": t.Description,
					"parameters": rawOrEmptyObject(t.InputSchema),
				},
			})
		}
		payload["tools"] = tools
	}
	if req.Stream {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	out, _ := json.Marshal(payload)

	cfg := s.cfg.Get()
	acc, err := s.pool.Pick("", map[string]bool{})
	if err != nil {
		writeJSON(w, 503, map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": err.Error()}})
		return
	}
	ctx, cancel := timeoutCtx(r, time.Duration(cfg.RequestTimeout)*time.Second)
	resp, err := s.up.Post(ctx, strings.TrimRight(cfg.UpstreamBase, "/")+"/chat/completions", acc.Token, out)
	if err != nil {
		cancel()
		s.pool.Release(acc.ID, 0, 0, 0, err.Error(), 10*time.Second)
		writeJSON(w, 502, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
		return
	}
	if resp.StatusCode >= 300 {
		ue := upstream.ReadError(resp)
		cancel()
		cool, reason := upstream.Classify(ue.Status, ue.Code)
		s.pool.Release(acc.ID, ue.Status, 0, 0, reason, cool)
		writeJSON(w, ue.Status, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": ue.Message}})
		return
	}
	if req.Stream {
		s.streamAnthropic(w, resp, acc.ID, modelID, start, cancel, r)
		return
	}
	s.plainAnthropicReal(w, resp, acc.ID, modelID, start, cancel, r)
}

func rawOrEmptyObject(r json.RawMessage) any {
	if len(r) == 0 || string(r) == "null" {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	var v any
	if json.Unmarshal(r, &v) != nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return v
}

func anthropicText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []aBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" {
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

func convertAnthropicMessage(m aMessage) []map[string]any {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []map[string]any{{"role": m.Role, "content": s}}
	}
	var blocks []aBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		return []map[string]any{{"role": m.Role, "content": ""}}
	}

	if m.Role == "assistant" {
		var text strings.Builder
		var calls []map[string]any
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "tool_use":
				args := "{}"
				if len(b.Input) > 0 {
					args = string(b.Input)
				}
				calls = append(calls, map[string]any{
					"id": b.ID, "type": "function",
					"function": map[string]any{"name": b.Name, "arguments": args},
				})
			}
		}
		msg := map[string]any{"role": "assistant", "content": text.String()}
		if len(calls) > 0 {
			msg["content"] = nil
			msg["tool_calls"] = calls
		}
		return []map[string]any{msg}
	}

	// user：可能同时含文本、图片与 tool_result
	var out []map[string]any
	var parts []map[string]any
	var plain strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			plain.WriteString(b.Text)
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case "image":
			if b.Source != nil && b.Source.Data != "" {
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{"url": "data:" + b.Source.MediaType + ";base64," + b.Source.Data},
				})
			}
		case "tool_result":
			content := anthropicText(b.Content)
			if content == "" {
				var raw any
				if len(b.Content) > 0 && json.Unmarshal(b.Content, &raw) == nil {
					if by, e := json.Marshal(raw); e == nil {
						content = string(by)
					}
				}
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": content})
		}
	}
	if len(parts) > 0 && len(out) == 0 {
		if len(parts) == 1 && parts[0]["type"] == "text" {
			out = append(out, map[string]any{"role": "user", "content": plain.String()})
		} else {
			out = append(out, map[string]any{"role": "user", "content": parts})
		}
	} else if plain.Len() > 0 {
		out = append(out, map[string]any{"role": "user", "content": plain.String()})
	}
	return out
}

type oaiResp struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (s *Server) plainAnthropicReal(w http.ResponseWriter, resp *http.Response, accID, model string, start time.Time, cancel func(), req *http.Request) {
	defer cancel()
	defer resp.Body.Close()
	var o oaiResp
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		writeJSON(w, 502, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "上游响应解析失败"}})
		return
	}
	content := make([]map[string]any, 0, 2)
	text := ""
	stop := "end_turn"
	if len(o.Choices) > 0 {
		text = o.Choices[0].Message.Content
		stop = mapStop(o.Choices[0].FinishReason, len(o.Choices[0].Message.ToolCalls) > 0)
		for _, tc := range o.Choices[0].Message.ToolCalls {
			var input any = map[string]any{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
			})
		}
	}
	if text != "" {
		content = append([]map[string]any{{"type": "text", "text": text}}, content...)
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	writeJSON(w, 200, map[string]any{
		"id": "msg_" + o.ID, "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": o.Usage.PromptTokens, "output_tokens": o.Usage.CompletionTokens},
	})
	s.record(start, req, "/v1/messages", model, accID, 200, o.Usage.PromptTokens, o.Usage.CompletionTokens, 0, false, "")
}

func mapStop(fr string, hasTool bool) string {
	if hasTool {
		return "tool_use"
	}
	switch fr {
	case "length":
		return "max_tokens"
	case "stop", "":
		return "end_turn"
	default:
		return fr
	}
}

func (s *Server) streamAnthropic(w http.ResponseWriter, resp *http.Response, accID, model string, start time.Time, cancel func(), req *http.Request) {
	defer cancel()
	defer resp.Body.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]any{"error": "stream unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	id := "msg_" + fmt.Sprintf("%d", time.Now().UnixNano())
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	blockIndex := -1
	textOpen := false
	toolIndex := -1
	toolArgs := map[string]string{}
	toolNames := map[string]string{}
	promptTokens, outTokens := 0, 0
	finish := "end_turn"

	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			t := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(t, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(t, "data:"))
				if payload != "" && payload != "[DONE]" {
					p, o := parseUsage(payload)
					if p > 0 {
						promptTokens = p
					}
					if o > 0 {
						outTokens = o
					}
					var chunk struct {
						Choices []struct {
							Delta struct {
								Content   string `json:"content"`
								ToolCalls []struct {
									Index    int    `json:"index"`
									ID       string `json:"id"`
									Function struct {
										Name      string `json:"name"`
										Arguments string `json:"arguments"`
									} `json:"function"`
								} `json:"tool_calls"`
							} `json:"delta"`
							FinishReason string `json:"finish_reason"`
						} `json:"choices"`
					}
					if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Choices) > 0 {
						ch := chunk.Choices[0]
						if ch.FinishReason != "" {
							finish = mapStop(ch.FinishReason, toolIndex >= 0)
						}
						if ch.Delta.Content != "" {
							if !textOpen {
								blockIndex++
								textOpen = true
								send("content_block_start", map[string]any{
									"type": "content_block_start", "index": blockIndex,
									"content_block": map[string]any{"type": "text", "text": ""},
								})
							}
							send("content_block_delta", map[string]any{
								"type": "content_block_delta", "index": blockIndex,
								"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
							})
						}
						for _, tc := range ch.Delta.ToolCalls {
							if tc.ID != "" || tc.Function.Name != "" {
								// 新工具块：先关掉文本块
								if textOpen {
									send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
									textOpen = false
								}
								blockIndex++
								toolIndex = blockIndex
								toolArgs[tc.ID] = ""
								toolNames[tc.ID] = tc.Function.Name
								send("content_block_start", map[string]any{
									"type": "content_block_start", "index": blockIndex,
									"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}},
								})
							}
							if tc.Function.Arguments != "" && toolIndex >= 0 {
								toolArgs[tc.ID] += tc.Function.Arguments
								send("content_block_delta", map[string]any{
									"type": "content_block_delta", "index": toolIndex,
									"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
								})
							}
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if textOpen || toolIndex >= 0 {
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
	}
	send("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{"stop_reason": finish, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTokens},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
	flusher.Flush()

	s.pool.Release(accID, 200, promptTokens, outTokens, "", 0)
	s.record(start, req, "/v1/messages", model, accID, 200, promptTokens, outTokens, 0, true, "")
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "读取请求体失败"})
		return
	}
	// 粗略估算：ASCII 约 4 字符/token，CJK 约 1.2 字符/token。
	ascii, other := 0, 0
	for _, ch := range string(raw) {
		if ch < 128 {
			ascii++
		} else {
			other++
		}
	}
	n := ascii/4 + other*4/5
	if n < 1 {
		n = 1
	}
	writeJSON(w, 200, map[string]any{"input_tokens": n})
}

