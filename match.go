package httptape

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// MatchConfig 控制回放时如何判定两条请求“匹配”。
type MatchConfig struct {
	// Method 是否纳入方法（默认纳入）。
	Method bool
	// Path 是否纳入路径（默认纳入）。
	Path bool
	// Query 是否纳入查询参数（默认纳入），参数顺序会被归一化。
	Query bool
	// Headers 是否纳入请求头。
	Headers bool
	// Body 是否纳入请求体。
	Body bool
	// QueryParams 白名单：非空时只匹配这些查询参数（忽略其它参数）。
	QueryParams []string
	// HeaderNames 非空时只匹配这些请求头（大小写不敏感）。
	HeaderNames []string
	// NormalizeHeaderWhitespace 归一化请求头值两端空白。
	NormalizeHeaderWhitespace bool
	// NormalizeBodyWhitespace 归一化请求体空白（折叠连续空白）。
	NormalizeBodyWhitespace bool
}

// DefaultMatchConfig 返回默认匹配规则：方法 + 路径 + 查询参数。
func DefaultMatchConfig() MatchConfig {
	return MatchConfig{Method: true, Path: true, Query: true}
}

// normalizedRequest 是从请求中提取的、用于计算匹配键的归一化数据。
type normalizedRequest struct {
	method  string
	path    string
	query   string
	headers string
	body    string
}

// matchKey 根据匹配配置计算请求的确定性哈希键。
// 语义等价的请求（查询顺序、头部大小写、空白差异）会得到相同的键。
func (mc MatchConfig) matchKey(method, rawURL string, header http.Header, body []byte) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		// 录制时的请求必然可解析；兜底使用原始字符串，保证不崩溃。
		u = &url.URL{Path: rawURL}
	}
	n := normalizedRequest{}
	if mc.Method {
		n.method = strings.ToUpper(method)
	}
	if mc.Path {
		n.path = u.Path
	}
	if mc.Query {
		n.query = normalizeQuery(u.Query(), mc.QueryParams)
	}
	if mc.Headers {
		n.headers = normalizeHeaders(header, mc.HeaderNames, mc.NormalizeHeaderWhitespace)
	}
	if mc.Body {
		n.body = hashBody(body, mc.NormalizeBodyWhitespace)
	}
	var b strings.Builder
	b.WriteString(n.method)
	b.WriteByte(0)
	b.WriteString(n.path)
	b.WriteByte(0)
	b.WriteString(n.query)
	b.WriteByte(0)
	b.WriteString(n.headers)
	b.WriteByte(0)
	b.WriteString(n.body)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// normalizeQuery 把查询参数归一化为顺序无关的字符串。
func normalizeQuery(q url.Values, whitelist []string) string {
	allow := make(map[string]bool, len(whitelist))
	for _, p := range whitelist {
		allow[p] = true
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		if len(whitelist) > 0 && !allow[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "&")
}

// normalizeHeaders 把请求头归一化为大小写不敏感、顺序确定的字符串。
func normalizeHeaders(h http.Header, names []string, trimWS bool) string {
	// 先把原始头归一化为 canonical 键 -> 值的映射。
	canonicalVals := make(map[string][]string, len(h))
	for k, vals := range h {
		c := http.CanonicalHeaderKey(k)
		canonicalVals[c] = append(canonicalVals[c], vals...)
	}
	allow := make(map[string]bool, len(names))
	for _, n := range names {
		allow[http.CanonicalHeaderKey(n)] = true
	}
	keys := make([]string, 0, len(canonicalVals))
	for k := range canonicalVals {
		if len(names) > 0 && !allow[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), canonicalVals[k]...)
		if trimWS {
			for i := range vals {
				vals[i] = strings.TrimSpace(vals[i])
			}
		}
		parts = append(parts, k+"="+strings.Join(vals, ","))
	}
	return strings.Join(parts, "\n")
}

// hashBody 计算请求体的哈希；开启空白归一化时先消除不改变语义的空白差异。
// JSON 内容使用 json.Compact 去除无意义空白，其它内容折叠连续空白。
func hashBody(body []byte, normalizeWS bool) string {
	if len(body) == 0 {
		return ""
	}
	if normalizeWS {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			var compacted bytes.Buffer
			if err := json.Compact(&compacted, trimmed); err == nil {
				body = compacted.Bytes()
			} else {
				body = []byte(strings.Join(strings.Fields(string(body)), " "))
			}
		} else {
			body = []byte(strings.Join(strings.Fields(string(body)), " "))
		}
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
