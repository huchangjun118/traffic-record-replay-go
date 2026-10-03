package httptape

import (
	"encoding/json"
	"net/http"
	"strings"
)

// SanitizeConfig 控制落盘前的脱敏行为。脱敏只发生在写入前，
// 回放过程不依赖任何真实凭据。
type SanitizeConfig struct {
	// RedactHeaders 需要脱敏的请求/响应头名称，大小写不敏感。
	RedactHeaders []string
	// RedactBodyFields 需要脱敏的 JSON 字段，使用点分路径（如 "user.token"）。
	RedactBodyFields []string
	// Placeholder 脱敏占位符，为空时使用默认占位符。
	Placeholder string
}

const defaultPlaceholder = "***REDACTED***"

// placeholder 返回生效的占位符。
func (s SanitizeConfig) placeholder() string {
	if s.Placeholder != "" {
		return s.Placeholder
	}
	return defaultPlaceholder
}

// headerRedacted 判断指定头是否需要脱敏（大小写不敏感）。
func (s SanitizeConfig) headerRedacted(name string) bool {
	for _, h := range s.RedactHeaders {
		if strings.EqualFold(h, name) {
			return true
		}
	}
	return false
}

// sanitizeHeaders 返回脱敏后的头副本，不修改原头。
func (s SanitizeConfig) sanitizeHeaders(h http.Header) http.Header {
	if len(s.RedactHeaders) == 0 {
		return h
	}
	out := h.Clone()
	ph := s.placeholder()
	for name, vals := range out {
		if s.headerRedacted(name) {
			for i := range vals {
				vals[i] = ph
			}
		}
	}
	return out
}

// sanitizeBody 对 JSON 请求体按点分路径脱敏；非 JSON 或解析失败时原样返回。
func (s SanitizeConfig) sanitizeBody(body []byte) []byte {
	if len(s.RedactBodyFields) == 0 || len(body) == 0 {
		return body
	}
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return body
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	ph := s.placeholder()
	for _, field := range s.RedactBodyFields {
		path := strings.Split(field, ".")
		redactPath(v, path, ph)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

// redactPath 沿点分路径遍历，把命中的字段替换为占位符；
// 遇到数组时对每个元素继续应用剩余路径。
func redactPath(v any, path []string, ph string) {
	if len(path) == 0 {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		key := path[0]
		if len(path) == 1 {
			if _, ok := t[key]; ok {
				t[key] = ph
			}
			return
		}
		if child, ok := t[key]; ok {
			redactPath(child, path[1:], ph)
		}
	case []any:
		for _, item := range t {
			redactPath(item, path, ph)
		}
	}
}
