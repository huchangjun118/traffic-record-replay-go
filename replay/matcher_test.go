package replay

import (
	"net/http"
	"testing"
)

func req(method, url string, hdr http.Header, body []byte) RecordedRequest {
	return RecordedRequest{Method: method, URL: url, Header: hdr, Body: body}
}

func TestMatcherQueryOrderNormalized(t *testing.T) {
	m := NewMatcher(DefaultMatchConfig())
	a := m.Key(req("GET", "http://h/p?a=1&b=2", nil, nil))
	b := m.Key(req("GET", "http://h/p?b=2&a=1", nil, nil))
	if a != b {
		t.Fatal("query order should be normalized")
	}
	// 语义不同（参数值不同）必须区分开。
	c := m.Key(req("GET", "http://h/p?a=1&b=3", nil, nil))
	if a == c {
		t.Fatal("different query values must differ")
	}
	// 关闭归一时顺序差异应体现。
	m2 := NewMatcher(MatchConfig{IgnoreQueryOrder: false})
	if m2.Key(req("GET", "http://h/p?a=1&b=2", nil, nil)) == m2.Key(req("GET", "http://h/p?b=2&a=1", nil, nil)) {
		t.Fatal("IgnoreQueryOrder=false should distinguish order")
	}
}

func TestMatcherMethodAndPath(t *testing.T) {
	m := NewMatcher(DefaultMatchConfig())
	if m.Key(req("GET", "http://h/p", nil, nil)) == m.Key(req("POST", "http://h/p", nil, nil)) {
		t.Fatal("method must participate in matching")
	}
	if m.Key(req("GET", "http://h/p1", nil, nil)) == m.Key(req("GET", "http://h/p2", nil, nil)) {
		t.Fatal("path must participate in matching")
	}
	// 方法大小写不敏感。
	if m.Key(req("get", "http://h/p", nil, nil)) != m.Key(req("GET", "http://h/p", nil, nil)) {
		t.Fatal("method case should be normalized")
	}
}

func TestMatcherHeaderCaseInsensitive(t *testing.T) {
	m := NewMatcher(MatchConfig{
		IgnoreQueryOrder: true,
		MatchHeaders:     []string{"x-tenant"},
	})
	h1 := http.Header{"X-Tenant": []string{"t1"}}
	h2 := http.Header{"x-TENANT": []string{"t1"}}
	if m.Key(req("GET", "http://h/p", h1, nil)) != m.Key(req("GET", "http://h/p", h2, nil)) {
		t.Fatal("header name case should be normalized")
	}
	h3 := http.Header{"X-Tenant": []string{"t2"}}
	if m.Key(req("GET", "http://h/p", h1, nil)) == m.Key(req("GET", "http://h/p", h3, nil)) {
		t.Fatal("configured header value must participate")
	}
	// 未配置的头部不参与匹配。
	mNoHdr := NewMatcher(DefaultMatchConfig())
	if mNoHdr.Key(req("GET", "http://h/p", h1, nil)) != mNoHdr.Key(req("GET", "http://h/p", h3, nil)) {
		t.Fatal("unconfigured headers must not participate")
	}
}

func TestMatcherBodyMatching(t *testing.T) {
	m := NewMatcher(MatchConfig{
		IgnoreQueryOrder:        true,
		MatchBody:               true,
		NormalizeBodyWhitespace: true,
	})
	a := m.Key(req("POST", "http://h/p", nil, []byte(`{ "a": 1, "b": 2 }`)))
	b := m.Key(req("POST", "http://h/p", nil, []byte(`{"a":1,"b":2}`)))
	if a != b {
		t.Fatal("JSON whitespace should be normalized")
	}
	c := m.Key(req("POST", "http://h/p", nil, []byte(`{"a":9,"b":2}`)))
	if a == c {
		t.Fatal("different body must differ")
	}
	// 未开启 MatchBody 时请求体不参与。
	m2 := NewMatcher(DefaultMatchConfig())
	if m2.Key(req("POST", "http://h/p", nil, []byte("x"))) != m2.Key(req("POST", "http://h/p", nil, []byte("y"))) {
		t.Fatal("body must not participate when MatchBody=false")
	}
}

func TestMaskerHeadersAndBodyFields(t *testing.T) {
	mk := NewMasker(MaskConfig{
		Headers:    []string{"Authorization"},
		BodyFields: []string{"password"},
	})
	out := mk.MaskRequest(RecordedRequest{
		Method: "POST",
		URL:    "http://h/login",
		Header: http.Header{
			"Authorization": []string{"Bearer secret"},
			"X-Other":       []string{"keep"},
		},
		Body: []byte(`{"user":"u","password":"p@ss"}`),
	})
	if got := out.Header.Get("Authorization"); got != "***" {
		t.Fatalf("authorization not masked: %q", got)
	}
	if got := out.Header.Get("X-Other"); got != "keep" {
		t.Fatalf("unrelated header changed: %q", got)
	}
	if string(out.Body) != `{"password":"***","user":"u"}` {
		t.Fatalf("body not masked as expected: %s", out.Body)
	}
	// 非 JSON 体原样保留。
	raw := mk.MaskRequest(RecordedRequest{Method: "POST", URL: "http://h/", Body: []byte("not json")})
	if string(raw.Body) != "not json" {
		t.Fatalf("non-JSON body altered: %q", raw.Body)
	}
	// 自定义替换值。
	mk2 := NewMasker(MaskConfig{Headers: []string{"Cookie"}, Replacement: "[redacted]"})
	r2 := mk2.MaskRequest(RecordedRequest{Header: http.Header{"Cookie": []string{"sid=1"}}})
	if r2.Header.Get("Cookie") != "[redacted]" {
		t.Fatalf("custom replacement not used: %q", r2.Header.Get("Cookie"))
	}
}

func TestMaskerDoesNotMutateInput(t *testing.T) {
	mk := NewMasker(MaskConfig{Headers: []string{"Authorization"}})
	orig := RecordedRequest{Header: http.Header{"Authorization": []string{"secret"}}}
	mk.MaskRequest(orig)
	if orig.Header.Get("Authorization") != "secret" {
		t.Fatal("masker mutated its input")
	}
}
