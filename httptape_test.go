package httptape

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// newEchoServer 返回一个把请求信息回显到响应体与头部的测试服务器。
func newEchoServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo-Method", r.Method)
		w.Header().Set("X-Echo-Path", r.URL.Path)
		w.Header().Set("X-Echo-Query", r.URL.RawQuery)
		w.Header().Set("X-Echo-Body", string(body))
		w.Header().Set("X-Request-Id", r.Header.Get("X-Request-Id"))
		w.Header().Set("X-Secret-Resp", r.Header.Get("X-Secret-Req"))
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "resp:%s:%s:%s", r.Method, r.URL.Path, string(body))
	}))
}

func newRecorderCfg(t *testing.T, dir, session string) Config {
	t.Helper()
	return Config{
		Mode:        ModeRecord,
		StoragePath: dir,
		Session:     session,
		Match:       DefaultMatchConfig(),
	}
}

func doRequest(t *testing.T, client *http.Client, method, url string, body string) (int, http.Header, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Clone(), data
}

// TestRecordAndReplay 录制后回放，状态/头部/主体逐字节一致。
func TestRecordAndReplay(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()
	session := "basic"

	rec, err := New(newRecorderCfg(t, dir, session))
	if err != nil {
		t.Fatal(err)
	}
	recClient := rec.Client()
	wantCode, wantHeader, wantBody := doRequest(t, recClient, http.MethodPost, srv.URL+"/foo?a=1", `{"k":"v"}`)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	replayer, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: session, Match: DefaultMatchConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer replayer.Close()
	gotCode, gotHeader, gotBody := doRequest(t, replayer.Client(), http.MethodPost, srv.URL+"/foo?a=1", `{"k":"v"}`)

	if gotCode != wantCode {
		t.Fatalf("status: got %d want %d", gotCode, wantCode)
	}
	if !headersEqual(gotHeader, wantHeader) {
		t.Fatalf("headers differ:\n got: %v\nwant: %v", gotHeader, wantHeader)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("body: got %q want %q", gotBody, wantBody)
	}
	st := replayer.Stats()
	if st.Hits != 1 || st.Misses != 0 || st.Records != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

// failingTransport 一旦被调用就报错，用于证明回放不触达真实网络。
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("real network was called during replay")
}

// TestReplayNeverDials 回放模式即使配置了真实传输也不会调用它。
func TestReplayNeverDials(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()
	session := "nodial"

	rec, err := New(newRecorderCfg(t, dir, session))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = doRequest(t, rec.Client(), http.MethodGet, srv.URL+"/x", "")
	_ = rec.Close()

	replayer, err := New(Config{
		Mode:        ModeReplay,
		StoragePath: dir,
		Session:     session,
		Match:       DefaultMatchConfig(),
		Transport:   failingTransport{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer replayer.Close()
	code, _, body := doRequest(t, replayer.Client(), http.MethodGet, srv.URL+"/x", "")
	if code != 200 || string(body) != "resp:GET:/x:" {
		t.Fatalf("unexpected replay result: %d %q", code, body)
	}
}

// TestModeSwitch 同一会话先录制后回放，模式显式切换。
func TestModeSwitch(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()
	session := "switch"

	rec, err := New(newRecorderCfg(t, dir, session))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = doRequest(t, rec.Client(), http.MethodGet, srv.URL+"/m", "")
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	// 同一会话名切换到回放模式
	replayer, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: session, Match: DefaultMatchConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer replayer.Close()
	code, _, _ := doRequest(t, replayer.Client(), http.MethodGet, srv.URL+"/m", "")
	if code != 200 {
		t.Fatalf("replay status %d", code)
	}
}

// TestMatchAmbiguity 同一匹配键对应多条记录时，PolicyFirst 始终返回第一条，
// 高并发下结果逐字节一致，与请求到达顺序无关。
func TestMatchAmbiguity(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()
	session := "ambiguous"

	rec, err := New(newRecorderCfg(t, dir, session))
	if err != nil {
		t.Fatal(err)
	}
	// 方法/路径/查询相同，请求体不同；默认规则不匹配请求体 → 同一匹配键。
	_, _, _ = doRequest(t, rec.Client(), http.MethodPost, srv.URL+"/same?z=1", `body-A`)
	_, _, _ = doRequest(t, rec.Client(), http.MethodPost, srv.URL+"/same?z=1", `body-B`)
	_ = rec.Close()

	replayer, err := New(Config{
		Mode:        ModeReplay,
		StoragePath: dir,
		Session:     session,
		Match:       DefaultMatchConfig(),
		Selection:   PolicyFirst,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer replayer.Close()

	var wg sync.WaitGroup
	results := make([]string, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, body := doRequest(t, replayer.Client(), http.MethodPost, srv.URL+"/same?z=1", `body-C`)
			results[i] = string(body)
		}(i)
	}
	wg.Wait()
	want := "resp:POST:/same:body-A"
	for i, got := range results {
		if got != want {
			t.Fatalf("result[%d] = %q, want %q (PolicyFirst must always pick the first record)", i, got, want)
		}
	}
}

// TestRepeatedRequests 重复请求在 PolicyFirst 下返回一致；
// PolicyRoundRobin 下按录制顺序循环。
func TestRepeatedRequests(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()
	session := "repeat"

	rec, err := New(newRecorderCfg(t, dir, session))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = doRequest(t, rec.Client(), http.MethodGet, srv.URL+"/r", "")
	_, _, _ = doRequest(t, rec.Client(), http.MethodGet, srv.URL+"/r", "")
	_ = rec.Close()

	// PolicyFirst
	first, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: session, Match: DefaultMatchConfig(), Selection: PolicyFirst})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	for i := 0; i < 3; i++ {
		_, _, b := doRequest(t, first.Client(), http.MethodGet, srv.URL+"/r", "")
		if string(b) != "resp:GET:/r:" {
			t.Fatalf("PolicyFirst repeat %d: %q", i, b)
		}
	}

	// PolicyRoundRobin
	rr, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: session, Match: DefaultMatchConfig(), Selection: PolicyRoundRobin})
	if err != nil {
		t.Fatal(err)
	}
	defer rr.Close()
	var bodies []string
	for i := 0; i < 4; i++ {
		_, _, b := doRequest(t, rr.Client(), http.MethodGet, srv.URL+"/r", "")
		bodies = append(bodies, string(b))
	}
	want := []string{"resp:GET:/r:", "resp:GET:/r:", "resp:GET:/r:", "resp:GET:/r:"}
	if strings.Join(bodies, ",") != strings.Join(want, ",") {
		t.Fatalf("round-robin: %v", bodies)
	}
}

// TestNormalization 查询参数顺序、头部大小写与空白、请求体空白不改变语义，
// 应归一为同一匹配键。
func TestNormalization(t *testing.T) {
	mc := MatchConfig{
		Method:                    true,
		Path:                      true,
		Query:                     true,
		Headers:                   true,
		Body:                      true,
		NormalizeHeaderWhitespace: true,
		NormalizeBodyWhitespace:   true,
	}
	h1 := http.Header{"X-Foo": []string{"  bar "}, "X-Bar": []string{"a"}}
	h2 := http.Header{"x-foo": []string{"bar"}, "x-bar": []string{"a"}}
	k1 := mc.matchKey("POST", "http://example.com/p?a=1&b=2", h1, []byte(`{"x":  1}`))
	k2 := mc.matchKey("post", "http://example.com/p?b=2&a=1", h2, []byte(`{"x":1}`))
	if k1 != k2 {
		t.Fatalf("normalized keys differ:\n%s\n%s", k1, k2)
	}
	// 语义不同（路径不同）必须不同键。
	k3 := mc.matchKey("POST", "http://example.com/other?a=1&b=2", h1, []byte(`{"x":  1}`))
	if k1 == k3 {
		t.Fatal("different paths produced the same key")
	}
}

// TestHeaderWhitespaceNoNormalization 不开启归一化时，头部空白差异产生不同键。
func TestHeaderWhitespaceNoNormalization(t *testing.T) {
	mc := MatchConfig{Method: true, Headers: true}
	h1 := http.Header{"X-Foo": []string{"bar"}}
	h2 := http.Header{"X-Foo": []string{" bar"}}
	k1 := mc.matchKey("GET", "http://example.com/", h1, nil)
	k2 := mc.matchKey("GET", "http://example.com/", h2, nil)
	if k1 == k2 {
		t.Fatal("header whitespace should distinguish keys when normalization is off")
	}
}

// TestMissPolicy MissError 返回明确错误；MissStatus 返回确定性响应。
func TestMissPolicy(t *testing.T) {
	dir := t.TempDir()
	session := "miss"
	// 空录制
	rec, err := New(newRecorderCfg(t, dir, session))
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	errR, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: session, Match: DefaultMatchConfig(), Miss: MissError})
	if err != nil {
		t.Fatal(err)
	}
	defer errR.Close()
	_, err = errR.Client().Get("http://example.com/nope")
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
	if errR.Stats().Misses != 1 {
		t.Fatalf("misses = %d", errR.Stats().Misses)
	}

	statusR, err := New(Config{
		Mode:        ModeReplay,
		StoragePath: dir,
		Session:     session,
		Match:       DefaultMatchConfig(),
		Miss:        MissStatus,
		MissResponse: MissResponse{
			StatusCode: http.StatusTeapot,
			Header:     http.Header{"X-Miss": []string{"1"}},
			Body:       []byte("missing"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer statusR.Close()
	resp, err := statusR.Client().Get("http://example.com/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "missing" || resp.Header.Get("X-Miss") != "1" {
		t.Fatalf("unexpected miss response: %q %v", b, resp.Header)
	}
}

// TestSanitization 敏感头与 JSON 字段在落盘前脱敏，回放仍可正常匹配。
func TestSanitization(t *testing.T) {
	// 独立 handler：返回固定 JSON，不回显请求体，避免响应体携带敏感信息。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Secret-Resp", r.Header.Get("X-Secret-Req"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	session := "sanitize"

	cfg := newRecorderCfg(t, dir, session)
	cfg.Sanitize = SanitizeConfig{
		RedactHeaders:    []string{"Authorization", "X-Secret-Req", "X-Secret-Resp"},
		RedactBodyFields: []string{"password", "user.token"},
	}
	rec, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/login", strings.NewReader(`{"user":"bob","password":"s3cret","user":{"token":"tok-123"}}`))
	req.Header.Set("Authorization", "Bearer real-credential")
	req.Header.Set("X-Secret-Req", "hidden-header-value")
	req.Header.Set("Content-Type", "application/json")
	resp, err := rec.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	_ = rec.Close()

	// 落盘文件中不得出现任何敏感明文。
	raw, err := os.ReadFile(filepath.Join(dir, session+".tape"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"s3cret", "tok-123", "real-credential", "hidden-header-value"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("tape file contains secret %q", secret)
		}
	}
	if !bytes.Contains(raw, []byte(defaultPlaceholder)) {
		t.Fatal("tape file missing redaction placeholder")
	}

	// 回放：入站请求带真实凭据也能命中（键口径一致），且响应正常。
	replayer, err := New(Config{
		Mode:        ModeReplay,
		StoragePath: dir,
		Session:     session,
		Match:       DefaultMatchConfig(),
		Sanitize:    cfg.Sanitize,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer replayer.Close()
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/login", strings.NewReader(`{"user":"bob","password":"s3cret","user":{"token":"tok-123"}}`))
	req2.Header.Set("Authorization", "Bearer real-credential")
	resp2, err := replayer.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("replay status %d", resp2.StatusCode)
	}
	b, _ := io.ReadAll(resp2.Body)
	if string(b) != `{"ok":true}` {
		t.Fatalf("unexpected body %q", b)
	}
}

func headersEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		bv, ok := b[k]
		if !ok || len(bv) != len(v) {
			return false
		}
		for i := range v {
			if bv[i] != v[i] {
				return false
			}
		}
	}
	return true
}

// writeTapeFile 直接写录制文件，用于构造损坏/版本场景。
func writeTapeFile(t *testing.T, dir, session string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, session+".tape"), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCorruptedTape 结构损坏、缺少提交标记、校验和不符都必须在加载阶段
// 被拒绝，返回可区分的错误，且不得崩溃。
func TestCorruptedTape(t *testing.T) {
	dir := t.TempDir()

	cases := map[string][]byte{
		"garbage":      []byte("this is not a tape file at all"),
		"empty":        []byte(""),
		"magic-only":   []byte("TRTAPE1\n"),
		"no-commit":    []byte("TRTAPE1\n" + `{"seq":1,"request":{"method":"GET","url":"http://x/"},"response":{"status_code":200}}` + "\n"),
		"bad-json":     []byte("TRTAPE1\n{not json\n"),
		"bad-checksum": []byte("TRTAPE1\n" + `{"seq":1,"request":{"method":"GET","url":"http://x/"},"response":{"status_code":200}}` + "\n" + `{"type":"commit","records":1,"checksum":"deadbeef"}` + "\n"),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			writeTapeFile(t, dir, name, content)
			_, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: name, Match: DefaultMatchConfig()})
			if !errors.Is(err, ErrCorrupted) {
				t.Fatalf("want ErrCorrupted, got %v", err)
			}
		})
	}
}

// TestUnsupportedVersion 新版本魔数必须返回 ErrUnsupportedVersion。
func TestUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	writeTapeFile(t, dir, "v2", []byte("TRTAPE2\n"+`{"type":"commit","records":0,"checksum":"x"}`+"\n"))
	_, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: "v2", Match: DefaultMatchConfig()})
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("want ErrUnsupportedVersion, got %v", err)
	}
	// 旧版本（v1 空录制）仍能正确加载。
	writeTapeFile(t, dir, "v1", []byte("TRTAPE1\n"+`{"type":"commit","records":0,"checksum":"`+func() string {
		// 空录制的校验和
		return "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	}()+`"}`+"\n"))
	r, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: "v1", Match: DefaultMatchConfig()})
	if err != nil {
		t.Fatalf("v1 should load: %v", err)
	}
	defer r.Close()
}

// TestConcurrencyIsolation 多个用例在同一进程内同时录制/回放，
// 各自记录不串扰，结果逐字节一致。
func TestConcurrencyIsolation(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()

	const sessions = 8
	var wg sync.WaitGroup
	for s := 0; s < sessions; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			session := fmt.Sprintf("case-%d", s)
			rec, err := New(newRecorderCfg(t, dir, session))
			if err != nil {
				t.Error(err)
				return
			}
			for i := 0; i < 20; i++ {
				_, _, _ = doRequest(t, rec.Client(), http.MethodGet, fmt.Sprintf("%s/p-%d?i=%d", srv.URL, s, i), "")
			}
			if err := rec.Close(); err != nil {
				t.Error(err)
			}
		}(s)
	}
	wg.Wait()

	// 并发回放，每个会话只能命中自己的记录。
	for s := 0; s < sessions; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			session := fmt.Sprintf("case-%d", s)
			r, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: session, Match: DefaultMatchConfig()})
			if err != nil {
				t.Error(err)
				return
			}
			defer r.Close()
			for i := 0; i < 20; i++ {
				_, _, body := doRequest(t, r.Client(), http.MethodGet, fmt.Sprintf("%s/p-%d?i=%d", srv.URL, s, i), "")
				want := fmt.Sprintf("resp:GET:/p-%d:", s)
				if string(body) != want {
					t.Errorf("session %d req %d: got %q want %q", s, i, body, want)
				}
			}
			st := r.Stats()
			if st.Hits != 20 || st.Misses != 0 {
				t.Errorf("session %d stats: %+v", s, st)
			}
		}(s)
	}
	wg.Wait()
}

// TestReplayConcurrentDeterminism 同一份录制在高并发下重复回放，
// 所有响应逐字节一致。
func TestReplayConcurrentDeterminism(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()
	session := "det"

	rec, err := New(newRecorderCfg(t, dir, session))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = doRequest(t, rec.Client(), http.MethodPost, srv.URL+"/det?q=1", `payload`)
	_ = rec.Close()

	r, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: session, Match: DefaultMatchConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var wg sync.WaitGroup
	results := make([][]byte, 128)
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, b := doRequest(t, r.Client(), http.MethodPost, srv.URL+"/det?q=1", `payload`)
			results[i] = b
		}(i)
	}
	wg.Wait()
	want := []byte("resp:POST:/det:payload")
	for i, b := range results {
		if !bytes.Equal(b, want) {
			t.Fatalf("result %d differs: %q", i, b)
		}
	}
}

// TestInterruptRecovery 中断恢复：
// 1) Abort 不产生最终录制文件；
// 2) 残留的临时文件不影响回放；
// 3) 回放被取消时返回 ctx 错误且不留下半成品状态。
func TestInterruptRecovery(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()

	// 1) Abort
	rec, err := New(newRecorderCfg(t, dir, "aborted"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = doRequest(t, rec.Client(), http.MethodGet, srv.URL+"/a", "")
	if err := rec.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "aborted.tape")); !os.IsNotExist(err) {
		t.Fatalf("aborted tape should not exist, stat err=%v", err)
	}
	// 重复 Abort 安全
	if err := rec.Abort(); err != nil {
		t.Fatal(err)
	}

	// 2) 残留临时文件 + 正常录制后回放不受影响
	rec2, err := New(newRecorderCfg(t, dir, "keep"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = doRequest(t, rec2.Client(), http.MethodGet, srv.URL+"/k", "")
	_ = rec2.Close()
	// 模拟崩溃残留的临时文件
	if err := os.WriteFile(filepath.Join(dir, ".keep.tape.tmp-garbage"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: "keep", Match: DefaultMatchConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, _, body := doRequest(t, r.Client(), http.MethodGet, srv.URL+"/k", "")
	if string(body) != "resp:GET:/k:" {
		t.Fatalf("replay with leftover temp: %q", body)
	}

	// 3) 回放取消
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/k", nil)
	_, err = r.Client().Do(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestBodyMatching 开启请求体匹配后，不同请求体不会互相命中。
func TestBodyMatching(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()
	session := "bodymatch"

	rec, err := New(newRecorderCfg(t, dir, session))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = doRequest(t, rec.Client(), http.MethodPost, srv.URL+"/b", `one`)
	_ = rec.Close()

	mc := DefaultMatchConfig()
	mc.Body = true
	r, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: session, Match: mc})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// 同路径同方法，不同请求体 → 不命中
	_, err = r.Client().Post(srv.URL+"/b", "text/plain", strings.NewReader(`two`))
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("want ErrNoMatch for different body, got %v", err)
	}
	// 相同请求体 → 命中
	code, _, body := doRequest(t, r.Client(), http.MethodPost, srv.URL+"/b", `one`)
	if code != 200 || string(body) != "resp:POST:/b:one" {
		t.Fatalf("body match replay: %d %q", code, body)
	}
}

// TestListSessions 列出已提交会话。
func TestListSessions(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []string{"s1", "s2"} {
		rec, err := New(newRecorderCfg(t, dir, s))
		if err != nil {
			t.Fatal(err)
		}
		_ = rec.Close()
	}
	sessions, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %v", sessions)
	}
}

// TestInvalidSession 非法会话名被拒绝。
func TestInvalidSession(t *testing.T) {
	if _, err := New(Config{Mode: ModeRecord, StoragePath: t.TempDir(), Session: "../escape"}); err == nil {
		t.Fatal("want error for path-traversal session")
	}
	if _, err := New(Config{Mode: ModeRecord, StoragePath: t.TempDir(), Session: ""}); err == nil {
		t.Fatal("want error for empty session")
	}
}

// roundTripFunc 便于在测试中用函数实现 http.RoundTripper。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestMatchWhitelists 白名单外的头/查询参数不影响匹配键，白名单内的影响。
func TestMatchWhitelists(t *testing.T) {
	mc := MatchConfig{Method: true, Path: true, Headers: true, HeaderNames: []string{"X-Important"}}
	h1 := http.Header{"X-Important": []string{"a"}, "X-Ignored": []string{"1"}}
	h2 := http.Header{"X-Important": []string{"a"}, "X-Ignored": []string{"2"}}
	if mc.matchKey("GET", "http://x/", h1, nil) != mc.matchKey("GET", "http://x/", h2, nil) {
		t.Fatal("non-whitelisted headers should not affect the match key")
	}
	h3 := http.Header{"X-Important": []string{"b"}}
	if mc.matchKey("GET", "http://x/", h1, nil) == mc.matchKey("GET", "http://x/", h3, nil) {
		t.Fatal("whitelisted header change must affect the match key")
	}

	qmc := MatchConfig{Method: true, Path: true, Query: true, QueryParams: []string{"a"}}
	if qmc.matchKey("GET", "http://x/?a=1&b=2", nil, nil) != qmc.matchKey("GET", "http://x/?a=1&b=3", nil, nil) {
		t.Fatal("non-whitelisted query params should not affect the match key")
	}
	if qmc.matchKey("GET", "http://x/?a=1&b=2", nil, nil) == qmc.matchKey("GET", "http://x/?a=2&b=2", nil, nil) {
		t.Fatal("whitelisted query param change must affect the match key")
	}
}

// TestCloseIdempotent 重复关闭安全；关闭后再录制返回明确错误。
func TestCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	rec, err := New(newRecorderCfg(t, dir, "idem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Client().Get("http://example.com/"); err == nil {
		t.Fatal("expected error when recording after close")
	}
}

// TestReplayMissStatsConcurrent 并发未命中统计准确。
func TestReplayMissStatsConcurrent(t *testing.T) {
	dir := t.TempDir()
	rec, err := New(newRecorderCfg(t, dir, "missc"))
	if err != nil {
		t.Fatal(err)
	}
	_ = rec.Close()
	r, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: "missc", Match: DefaultMatchConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Client().Get("http://example.com/miss")
		}()
	}
	wg.Wait()
	if st := r.Stats(); st.Misses != 50 || st.Hits != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// TestRecordUsesConfiguredTransport 录制模式使用配置的传输。
func TestRecordUsesConfiguredTransport(t *testing.T) {
	srv := newEchoServer()
	defer srv.Close()
	dir := t.TempDir()
	var called int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&called, 1)
		return http.DefaultTransport.RoundTrip(req)
	})
	rec, err := New(Config{
		Mode:        ModeRecord,
		StoragePath: dir,
		Session:     "transport",
		Transport:   transport,
		Match:       DefaultMatchConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = doRequest(t, rec.Client(), http.MethodGet, srv.URL+"/t", "")
	_ = rec.Close()
	if atomic.LoadInt32(&called) != 1 {
		t.Fatalf("configured transport called %d times, want 1", called)
	}
}

// BenchmarkReplayLookup 验证查找开销不随记录条数线性退化（O(1) 哈希索引）。
func BenchmarkReplayLookup(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	for _, n := range []int{10, 1000, 10000} {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			rec, err := New(Config{Mode: ModeRecord, StoragePath: dir, Session: "bench", Match: DefaultMatchConfig()})
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < n; i++ {
				req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/p-%d", srv.URL, i), nil)
				resp, err := rec.Client().Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			if err := rec.Close(); err != nil {
				b.Fatal(err)
			}
			r, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: "bench", Match: DefaultMatchConfig()})
			if err != nil {
				b.Fatal(err)
			}
			defer r.Close()
			target := fmt.Sprintf("%s/p-%d", srv.URL, n/2)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req, _ := http.NewRequest(http.MethodGet, target, nil)
				resp, err := r.Client().Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}
}

// TestStatusCodeRoundTrip 非 200 响应（如 404/500）同样完整录制与回放。
func TestStatusCodeRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	dir := t.TempDir()
	session := "status"

	rec, err := New(newRecorderCfg(t, dir, session))
	if err != nil {
		t.Fatal(err)
	}
	notFound, nfHeader, nfBody := doRequest(t, rec.Client(), http.MethodGet, srv.URL+"/missing", "")
	serverErr, seHeader, seBody := doRequest(t, rec.Client(), http.MethodGet, srv.URL+"/error", "")
	_ = rec.Close()

	r, err := New(Config{Mode: ModeReplay, StoragePath: dir, Session: session, Match: DefaultMatchConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	got404, h404, b404 := doRequest(t, r.Client(), http.MethodGet, srv.URL+"/missing", "")
	if got404 != notFound || got404 != http.StatusNotFound {
		t.Fatalf("404 status: got %d want %d", got404, notFound)
	}
	if !headersEqual(h404, nfHeader) || !bytes.Equal(b404, nfBody) {
		t.Fatalf("404 response mismatch:\n got %v %q\nwant %v %q", h404, b404, nfHeader, nfBody)
	}
	got500, h500, b500 := doRequest(t, r.Client(), http.MethodGet, srv.URL+"/error", "")
	if got500 != serverErr || got500 != http.StatusInternalServerError {
		t.Fatalf("500 status: got %d want %d", got500, serverErr)
	}
	if !headersEqual(h500, seHeader) || !bytes.Equal(b500, seBody) {
		t.Fatalf("500 response mismatch:\n got %v %q\nwant %v %q", h500, b500, seHeader, seBody)
	}
}
