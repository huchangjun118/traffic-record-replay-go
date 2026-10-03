package replay

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// MaskConfig 脱敏配置。
type MaskConfig struct {
	// Headers 需要脱敏的请求/响应头部名（大小写不敏感）。
	Headers []string
	// BodyFields 需要脱敏的 JSON 顶层字段名。
	BodyFields []string
	// Replacement 脱敏替换值，缺省为 "***"。
	Replacement string
}

// Masker 在写入记录前对敏感数据脱敏，保证落盘内容不含真实凭据。
type Masker struct {
	cfg         MaskConfig
	replacement string
	headers     map[string]bool // canonical 头部名集合
	fields      map[string]bool
}

// NewMasker 创建脱敏器。
func NewMasker(cfg MaskConfig) *Masker {
	repl := cfg.Replacement
	if repl == "" {
		repl = "***"
	}
	m := &Masker{
		cfg:         cfg,
		replacement: repl,
		headers:     map[string]bool{},
		fields:      map[string]bool{},
	}
	for _, h := range cfg.Headers {
		m.headers[http.CanonicalHeaderKey(h)] = true
	}
	for _, f := range cfg.BodyFields {
		m.fields[f] = true
	}
	return m
}

// MaskRequest 返回脱敏后的请求副本（不修改入参）。
func (m *Masker) MaskRequest(req RecordedRequest) RecordedRequest {
	out := req
	out.Header = m.maskHeader(req.Header)
	out.Body = m.maskBody(req.Body)
	return out
}

// MaskResponse 返回脱敏后的响应副本（不修改入参）。
func (m *Masker) MaskResponse(resp RecordedResponse) RecordedResponse {
	out := resp
	out.Header = m.maskHeader(resp.Header)
	out.Body = m.maskBody(resp.Body)
	return out
}

func (m *Masker) maskHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		if m.headers[http.CanonicalHeaderKey(k)] {
			cp := make([]string, len(vs))
			for i := range cp {
				cp[i] = m.replacement
			}
			out[k] = cp
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

// maskBody 对 JSON 顶层字段脱敏；非 JSON 或无匹配字段时原样返回。
func (m *Masker) maskBody(body []byte) []byte {
	if len(body) == 0 || len(m.fields) == 0 {
		return body
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return body
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return body
	}
	changed := false
	for k := range obj {
		if m.fields[k] {
			obj[k] = m.replacement
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
