package server

import (
	"strings"
	"testing"
)

func anyRefs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = "https://example.com/ref.png"
	}
	return out
}

func TestParseImageRefs(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		wantLen int
		wantErr string
	}{
		{"没传 → 文生图", nil, 0, ""},
		{"空字符串", "", 0, "不能为空"},
		{"单个字符串", "https://example.com/a.png", 1, ""},
		{"空数组等价没传", []any{}, 0, ""},
		{"32 张（上限内）", anyRefs(32), 32, ""},
		{"33 张（超上限）", anyRefs(33), 0, "最多 32 张"},
		{"元素不是字符串", []any{1}, 0, "必须是字符串引用"},
		{"元素是空串", []any{"  "}, 0, "必须是字符串引用"},
		{"非法类型", 42, 0, "必须是字符串或字符串数组"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseImageRefs(c.in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("期望错误含 %q，实际 %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if len(got) != c.wantLen {
				t.Fatalf("张数 = %d, want %d", len(got), c.wantLen)
			}
		})
	}
}

// 回归：[]string 分支曾完全绕过张数上限，只有 []any 被拦。
func TestParseImageRefsStringSliceAlsoCapped(t *testing.T) {
	in := make([]string, maxImageRefs+1)
	for i := range in {
		in[i] = "https://example.com/ref.png"
	}
	if _, err := ParseImageRefs(in); err == nil {
		t.Fatalf("[]string 传 %d 张应被拦，实际通过", len(in))
	}
}
