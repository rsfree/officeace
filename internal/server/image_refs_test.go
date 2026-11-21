package server

import (
	"encoding/json"
	"testing"
)

func intPtr(v int) *int { return &v }

// 垫图丢弃的判定边界：宁可漏判，也不能把「字段缺失」当成「明确为 0」。
func TestImageRefsDropped(t *testing.T) {
	cases := []struct {
		name   string
		refs   int
		tokens *int
		want   bool
	}{
		{"纯文生图，无该字段", 0, nil, false},
		{"纯文生图，上游报 0", 0, intPtr(0), false},
		{"带垫图，字段缺失（上游改版/老格式）", 3, nil, false},
		{"带垫图，明确为 0 → 被丢弃", 82, intPtr(0), true},
		{"带垫图，正常吃掉", 37, intPtr(56832), false},
		{"单张垫图，明确为 0", 1, intPtr(0), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := imageRefsDropped(c.refs, c.tokens); got != c.want {
				t.Fatalf("imageRefsDropped(%d, %v) = %v, want %v", c.refs, c.tokens, got, c.want)
			}
		})
	}
}

// 真实上游报文（2026-11-21 抓取），锁死「字段缺失」与「明确 0」的区分。
func TestImageUsageRealPayloads(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantImage *int // nil 表示字段缺失
		wantIn    int
	}{
		{
			name:      "37 张 1024x1536 正常吃进 56832 token",
			body:      `{"usage":{"total_tokens":57044,"input_tokens":56848,"input_tokens_details":{"image_tokens":56832,"text_tokens":16},"output_tokens":196}}`,
			wantImage: intPtr(56832),
			wantIn:    56848,
		},
		{
			name:      "82 张被静默丢弃，image_tokens=0",
			body:      `{"usage":{"total_tokens":204,"input_tokens":8,"input_tokens_details":{"image_tokens":0,"text_tokens":8},"output_tokens":196}}`,
			wantImage: intPtr(0),
			wantIn:    8,
		},
		{
			name:      "不含 input_tokens_details 的老格式",
			body:      `{"usage":{"input_tokens":123,"output_tokens":196}}`,
			wantImage: nil,
			wantIn:    123,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var meta struct {
				Usage imageUsage `json:"usage"`
			}
			if err := json.Unmarshal([]byte(c.body), &meta); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			got := meta.Usage.InputTokensDetails.ImageTokens
			if (got == nil) != (c.wantImage == nil) {
				t.Fatalf("image_tokens 存在性不符: got=%v want=%v", got, c.wantImage)
			}
			if got != nil && *got != *c.wantImage {
				t.Fatalf("image_tokens = %d, want %d", *got, *c.wantImage)
			}
			if meta.Usage.InputTokens != c.wantIn {
				t.Fatalf("input_tokens = %d, want %d", meta.Usage.InputTokens, c.wantIn)
			}
		})
	}
}
