package replay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// MatchConfig 配置请求匹配规则。
type MatchConfig struct {
	// MatchBody 是否将请求体纳入匹配。
	MatchBody bool
	// MatchHeaders 需要纳入匹配的头部名（大小写不敏感）。
	MatchHeaders []string
	// IgnoreQueryOrder 查询参数顺序是否归一（默认 true 归一）。
	IgnoreQueryOrder bool
	// NormalizeBodyWhitespace 是否归一 JSON 请求体空白。
	NormalizeBodyWhitespace bool
}

// DefaultMatchConfig 默认匹配配置：方法+路径+查询参数，查询顺序归一。
func DefaultMatchConfig() MatchConfig {
	return MatchConfig{IgnoreQueryOrder: true}
}

// Matcher 根据配置计算请求的匹配键。
// 匹配键是确定性的：相同语义的请求（无论查询顺序、头部大小写、
// JSON 空白差异）产生相同的键。
type Matcher struct {
	cfg MatchConfig
	// headers 预归一化（canonical 形式）的待匹配头部名，排序去重。
	headers []string
}

// NewMatcher 创建匹配器。
func NewMatcher(cfg MatchConfig) *Matcher {
	hs := make([]string, 0, len(cfg.MatchHeaders))
	seen := map[string]bool{}
	for _, h := range cfg.MatchHeaders {
		c := http.CanonicalHeaderKey(h)
		if !seen[c] {
			seen[c] = true
			hs = append(hs, c)
		}
	}
	sort.Strings(hs)
	return &Matcher{cfg: cfg, headers: hs}
}

// Key 计算一条录制请求的匹配键。
func (m *Matcher) Key(req RecordedRequest) string {
	var sb strings.Builder
	sb.WriteString(strings.ToUpper(req.Method))
	sb.WriteString("\x00")
	u, err := url.Parse(req.URL)
	if err != nil {
		// 无法解析的 URL 退化为原始字符串，仍保持确定性。
		sb.WriteString(req.URL)
		return sb.String()
	}
	sb.WriteString(strings.ToLower(u.Scheme))
	sb.WriteString("://")
	sb.WriteString(strings.ToLower(u.Host))
	sb.WriteString(u.Path)
	sb.WriteString("\x00")
	sb.WriteString(m.normalizedQuery(u))
	for _, h := range m.headers {
		sb.WriteString("\x00")
		sb.WriteString(h)
		sb.WriteString("=")
		sb.WriteString(strings.Join(headerValues(req, h), "\x01"))
	}
	if m.cfg.MatchBody {
		sb.WriteString("\x00")
		sb.Write(m.normalizedBody(req.Body))
	}
	return sb.String()
}

// normalizedQuery 归一化查询串。
func (m *Matcher) normalizedQuery(u *url.URL) string {
	raw := u.RawQuery
	if raw == "" {
		return ""
	}
	if !m.cfg.IgnoreQueryOrder {
		return raw
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	// url.Values.Encode 按 key 排序、同 key 内保持值顺序，结果确定。
	return vals.Encode()
}

// headerValues 取头部值（键名大小写不敏感，兼容非 canonical 存储键），
// 多值排序保证确定性。
func headerValues(req RecordedRequest, name string) []string {
	var out []string
	for k, vs := range req.Header {
		if http.CanonicalHeaderKey(k) == name {
			out = append(out, vs...)
		}
	}
	sort.Strings(out)
	return out
}

// normalizedBody 归一化请求体：JSON 且开启空白归一时重新编码为紧凑形式，
// 键序按 encoding/json 的确定性规则（map 键排序）输出。
func (m *Matcher) normalizedBody(body []byte) []byte {
	if !m.cfg.NormalizeBodyWhitespace || len(body) == 0 {
		return body
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return body
	}
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}
