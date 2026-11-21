package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// 真实上游报文（2026-11-21 抓取），用于锁死脱敏行为。
func TestSanitizeRealPayloads(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustHave []string
		mustNot  []string
	}{
		{
			name: "4096 尺寸拒绝（带 LiteLLM 尾注）",
			in: "[BadRequestError] Invalid size '4096x4096'. The longest edge must be less than or equal to 3840. " +
				"(request_id: chatimagegen-8cc57fe55ec1490ebd021b9199339c73)\n\n" +
				"LiteLLM: model group 'raccoon-image-gen' failed with the error above and no fallback model group " +
				"was found for it, so the request was not retried on another model. Fallbacks are configured for: " +
				"raccoon-090dd3-p-cresp, raccoon-other",
			mustHave: []string{"Invalid size '4096x4096'", "3840", "request_id", "chatimagegen-"},
			mustNot:  []string{"raccoon", "LiteLLM", "090dd3"},
		},
		{
			name:     "xhigh 不支持",
			in:       "[BadRequestError] The model 'raccoon-image' does not support quality 'xhigh'. (request_id: chatimagegen-a4ae2b2f3e524b458e1e234293e9c40b)",
			mustHave: []string{"The model 'gpt-image' does not support quality", "xhigh"},
			mustNot:  []string{"raccoon"},
		},
		{
			name:     "下划线/大写变体",
			in:       "model RACCOON_image and Raccoon-Image-Gen both leaked",
			mustHave: []string{"gpt-image"},
			mustNot:  []string{"RACCOON", "raccoon", "Raccoon"},
		},
		{
			name:     "空串",
			in:       "",
			mustHave: []string{""},
			mustNot:  []string{"gpt-image"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sanitize(c.in)
			for _, want := range c.mustHave {
				if !strings.Contains(got, want) {
					t.Fatalf("缺少 %q\n输入: %s\n输出: %s", want, c.in, got)
				}
			}
			for _, bad := range c.mustNot {
				if strings.Contains(got, bad) {
					t.Fatalf("仍含敏感串 %q\n输出: %s", bad, got)
				}
			}
		})
	}
}

func TestReadErrorAppliesSanitize(t *testing.T) {
	body := `{"error":{"code":"400","message":"[BadRequestError] The model 'raccoon-image' does not support quality 'xhigh'.","type":"invalid_request_error"}}`
	resp := newErrResponse(400, body)
	e := ReadError(resp)
	if strings.Contains(strings.ToLower(e.Message), "raccoon") {
		t.Fatalf("Message 未脱敏: %s", e.Message)
	}
	if strings.Contains(strings.ToLower(e.Error()), "raccoon") {
		t.Fatalf("Error() 未脱敏: %s", e.Error())
	}
	if !strings.Contains(e.Message, "gpt-image") {
		t.Fatalf("未替换为 gpt-image: %s", e.Message)
	}
}

func newErrResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
