package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// 载体转换层：把 canonical 契约里的「字符串引用」变成上游 edits 端点要求的文件流。
//
// canonical（对外）只收字符串，这是 image-adapter 的地基：
//
//	https://example.com/a.png        公网地址
//	data:image/png;base64,iVBORw0…   data URI
//	iVBORw0KGgo…                     裸 base64
//
// 上游（officeace /images/edits）只收 multipart UploadFile，传字符串会被 422 拒绝
// （实测：Expected UploadFile, received: <class 'str'>）。所以「下载 + 内联 + 分片」
// 这一步发生在网关内部，对外契约不变。

const (
	maxRefBytes    = 10 << 20 // 单张引用 10MB
	maxTotalRefs   = 40 << 20 // 全部引用合计 40MB
	maxRefRedirect = 3        // 重定向最多 3 跳，每跳都重新校验
	refFetchTimout = 30 * time.Second
)

// imageBlob 是一张已经拿到字节的引用图。
type imageBlob struct {
	Data []byte
	Mime string
}

// blockedCIDRs 覆盖回环、私网、链路本地与容器网段，用于阻断 SSRF。
var blockedCIDRs = func() []*net.IPNet {
	list := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16",
		"::1/128", "fc00::/7", "fe80::/10",
	}
	out := make([]*net.IPNet, 0, len(list))
	for _, c := range list {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// maxImageRefs 单次请求允许的垫图张数。
//
// 上游并没有明确的"张数"上限（实测 995 张极小图仍返回 200），真正的闸是两条：
//   - 图像 token 预算约 5.7 万：1024×1536 约 37 张、512×768 约 81 张，
//     超了上游不报错，而是丢掉全部参考图照常出图（见 imageRefsDropped）；
//   - 网关自己的 maxTotalRefs=40MB：1024×1536 的 PNG 约 36 张。
//
// 取 32 是两条闸的共同安全区：32×1536=49,152 token、32×1.1MB≈35MB。
const maxImageRefs = 32

// clampImageRefs 收口张数上限 —— []any 与 []string 两条路径必须同一规则，
// 否则绕过 JSON 解码的调用可以无限塞图，直到上游把垫图全丢掉。
func clampImageRefs(refs []string) ([]string, error) {
	if len(refs) > maxImageRefs {
		return nil, fmt.Errorf("image 最多 %d 张（上游按图像 token 计费，1024×1536 实测约 37 张封顶，超出会被静默丢弃）", maxImageRefs)
	}
	return refs, nil
}

// ParseImageRefs 从请求体取出 image 字段：string 或 []string。
// 返回 nil 表示没有图（文生图）；单元素数组保持数组语义，不压成标量。
func ParseImageRefs(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, errors.New("image 不能为空字符串")
		}
		return []string{t}, nil
	case []any:
		if len(t) == 0 {
			return nil, nil // 空数组等价于没传，走文生图
		}
		out := make([]string, 0, len(t))
		for i, item := range t {
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("image[%d] 必须是字符串引用", i)
			}
			out = append(out, s)
		}
		return clampImageRefs(out)
	case []string:
		if len(t) == 0 {
			return nil, nil
		}
		return clampImageRefs(t)
	default:
		return nil, errors.New("image 必须是字符串或字符串数组")
	}
}

// LoadImageRef 把一个引用取成字节：data URI 解码、http(s) 下载、裸 base64 解码。
func LoadImageRef(ctx context.Context, ref string) (*imageBlob, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "data:"):
		return decodeDataURI(ref)
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		return fetchRef(ctx, ref)
	default:
		return decodeBareBase64(ref)
	}
}

func decodeDataURI(ref string) (*imageBlob, error) {
	comma := strings.Index(ref, ",")
	if comma < 0 {
		return nil, errors.New("data URI 缺少逗号分隔符")
	}
	meta := ref[5:comma]
	if !strings.Contains(meta, "base64") {
		return nil, errors.New("data URI 仅支持 base64 编码")
	}
	mime := strings.TrimSuffix(strings.SplitN(meta, ";", 2)[0], " ")
	raw, err := base64.StdEncoding.DecodeString(ref[comma+1:])
	if err != nil {
		return nil, errors.New("data URI base64 解码失败")
	}
	if len(raw) == 0 || len(raw) > maxRefBytes {
		return nil, fmt.Errorf("单张图片大小不合法（上限 %d MB）", maxRefBytes>>20)
	}
	if mime == "" {
		mime = sniffMime(raw)
	}
	return &imageBlob{Data: raw, Mime: mime}, nil
}

func decodeBareBase64(ref string) (*imageBlob, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, ref)
	raw, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(cleaned); err != nil {
			return nil, errors.New("无法识别的图片引用：不是 http(s) 地址、data URI 或合法 base64")
		}
	}
	if len(raw) == 0 || len(raw) > maxRefBytes {
		return nil, fmt.Errorf("单张图片大小不合法（上限 %d MB）", maxRefBytes>>20)
	}
	return &imageBlob{Data: raw, Mime: sniffMime(raw)}, nil
}

func fetchRef(ctx context.Context, ref string) (*imageBlob, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, errors.New("图片地址无法解析")
	}
	if err := checkRefHost(ctx, u.Hostname()); err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: refFetchTimout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRefRedirect {
				return errors.New("重定向次数过多")
			}
			return checkRefHost(req.Context(), req.URL.Hostname())
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, errors.New("构造下载请求失败")
	}
	req.Header.Set("User-Agent", "officeace2api/image-ref")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("下载引用图失败: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载引用图失败: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRefBytes+1))
	if err != nil {
		return nil, errors.New("读取引用图失败")
	}
	if len(raw) > maxRefBytes {
		return nil, fmt.Errorf("引用图超过 %d MB", maxRefBytes>>20)
	}
	if len(raw) == 0 {
		return nil, errors.New("引用图内容为空")
	}
	return &imageBlob{Data: raw, Mime: sniffMime(raw)}, nil
}

// checkRefHost 在真正建立连接前拦截内网地址（含 DNS 解析结果）。
func checkRefHost(ctx context.Context, host string) error {
	if host == "" {
		return errors.New("图片地址缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return errors.New("拒绝访问内网地址")
		}
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return errors.New("图片地址无法解析")
	}
	if len(ips) == 0 {
		return errors.New("图片地址无法解析")
	}
	for _, a := range ips {
		if isBlockedIP(a.IP) {
			return errors.New("拒绝访问内网地址")
		}
	}
	return nil
}

// sniffMime 按字节判定，不信 Content-Type（CDN 常回 octet-stream）。
func sniffMime(b []byte) string {
	if len(b) < 12 {
		return "image/png"
	}
	ct := http.DetectContentType(b)
	if strings.HasPrefix(ct, "image/") {
		return ct
	}
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF8")):
		return "image/gif"
	case len(b) > 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return "image/png"
}

// BuildImageEditForm 组装上游 /images/edits 需要的 multipart。
// 字段名 image[] 是上游实测接受的写法（多图保序）。
func BuildImageEditForm(fields map[string]string, images []*imageBlob) (body *bytes.Buffer, contentType string, err error) {
	if len(images) == 0 {
		return nil, "", errors.New("没有可上传的图片")
	}
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	for k, v := range fields {
		if v == "" {
			continue
		}
		if err := mw.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	for i, img := range images {
		h := make(textproto.MIMEHeader)
		ext := ".png"
		switch img.Mime {
		case "image/jpeg":
			ext = ".jpg"
		case "image/webp":
			ext = ".webp"
		case "image/gif":
			ext = ".gif"
		}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image[]"; filename="ref%d%s"`, i, ext))
		h.Set("Content-Type", img.Mime)
		part, err := mw.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(img.Data); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf, mw.FormDataContentType(), nil
}
