package replay

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newRecordClient 起一个进程内 httptest 服务并返回录制模式的 client 与 store。
func newRecordClient(t *testing.T, handler http.HandlerFunc) (*http.Client, *Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	store := NewStore("test", nil, nil, nil)
	tr := NewTransport(ModeRecord, store, nil)
	return &http.Client{Transport: tr}, store, srv
}

func doGet(t *testing.T, c *http.Client, url string) (int, http.Header, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

func TestRecordThenReplayRoundTrip(t *testing.T) {
	var handler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Token", "abc")
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	}
	client, store, srv := newRecordClient(t, handler)

	code, hdr, body := doGet(t, client, srv.URL+"/api/users?page=1")
	if code != http.StatusTeapot || body != "hello /api/users" || hdr.Get("X-Token") != "abc" {
		t.Fatalf("record: got %d %q %v", code, body, hdr)
	}
	if store.Len() != 1 {
		t.Fatalf("expected 1 interaction, got %d", store.Len())
	}

	// 落盘后加载，用回放模式重放；此时真实服务已关闭也无妨。
	path := filepath.Join(t.TempDir(), "cassette.json")
	if err := store.SaveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadFrom(path, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	replayClient := &http.Client{Transport: NewTransport(ModeReplay, loaded, nil)}
	code, hdr, body = doGet(t, replayClient, srv.URL+"/api/users?page=1")
	if code != http.StatusTeapot || body != "hello /api/users" || hdr.Get("X-Token") != "abc" {
		t.Fatalf("replay: got %d %q %v", code, body, hdr)
	}
	if loaded.Metrics().Hits() != 1 {
		t.Fatalf("expected 1 hit, got %d", loaded.Metrics().Hits())
	}
}

func TestReplayNeverTouchesNetwork(t *testing.T) {
	// 指向一个必然不可达的地址；若回放误触网络会报错而非 ErrNoMatch。
	store := NewStore("empty", nil, nil, nil)
	client := &http.Client{Transport: NewTransport(ModeReplay, store, nil)}
	_, err := client.Get("http://127.0.0.1:1/unreachable")
	var noMatch *ErrNoMatch
	if !errors.As(err, &noMatch) {
		t.Fatalf("expected ErrNoMatch, got %v", err)
	}
	if store.Metrics().Misses() != 1 {
		t.Fatalf("expected 1 miss, got %d", store.Metrics().Misses())
	}
}

func TestModeSwitchExplicit(t *testing.T) {
	m, err := ParseMode("record")
	if err != nil || m != ModeRecord {
		t.Fatalf("parse record: %v %v", m, err)
	}
	m, err = ParseMode("replay")
	if err != nil || m != ModeReplay {
		t.Fatalf("parse replay: %v %v", m, err)
	}
	if _, err := ParseMode("bogus"); err == nil {
		t.Fatal("expected error for bogus mode")
	}
	// 非法模式值的 Transport 必须报错而非静默。
	bad := NewTransport(Mode(99), NewStore("x", nil, nil, nil), nil)
	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	if _, err := bad.RoundTrip(req); err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestReplayDeterministicRepeatedRequests(t *testing.T) {
	// 同一请求录制两条不同响应，回放必须按录制顺序轮询，且重复运行结果一致。
	var mu sync.Mutex
	n := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		fmt.Fprintf(w, "resp-%d", n)
		mu.Unlock()
	}
	client, store, srv := newRecordClient(t, handler)
	for i := 0; i < 2; i++ {
		if _, _, body := doGet(t, client, srv.URL+"/same"); body != fmt.Sprintf("resp-%d", i+1) {
			t.Fatalf("record %d: %q", i, body)
		}
	}
	path := filepath.Join(t.TempDir(), "c.json")
	if err := store.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	run := func() []string {
		loaded, err := LoadFrom(path, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		c := &http.Client{Transport: NewTransport(ModeReplay, loaded, nil)}
		var out []string
		for i := 0; i < 4; i++ { // 4 次请求，2 条记录，轮询两轮
			_, _, body := doGet(t, c, srv.URL+"/same")
			out = append(out, body)
		}
		return out
	}
	want := []string{"resp-1", "resp-2", "resp-1", "resp-2"}
	for round := 0; round < 3; round++ {
		got := run()
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("round %d req %d: got %q want %q", round, i, got[i], want[i])
			}
		}
	}
}

func TestReplayConcurrentDeterministic(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	}
	client, store, srv := newRecordClient(t, handler)
	const kinds = 8
	for i := 0; i < kinds; i++ {
		doGet(t, client, fmt.Sprintf("%s/p%d", srv.URL, i))
	}
	path := filepath.Join(t.TempDir(), "c.json")
	if err := store.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFrom(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rc := &http.Client{Transport: NewTransport(ModeReplay, loaded, nil)}
	// 高并发请求不同路径，每个响应必须与路径对应，互不串扰。
	var wg sync.WaitGroup
	errs := make(chan string, kinds*20)
	for i := 0; i < kinds*20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := fmt.Sprintf("/p%d", i%kinds)
			_, _, body := doGet(t, rc, srv.URL+p)
			if body != "path="+p {
				errs <- fmt.Sprintf("got %q want path=%s", body, p)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if loaded.Metrics().Hits() != kinds*20 {
		t.Fatalf("hits=%d want %d", loaded.Metrics().Hits(), kinds*20)
	}
}

func TestReplayByteIdenticalAcrossRuns(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		io.WriteString(w, `{"a":1,"b":[2,3]}`)
	}
	client, store, srv := newRecordClient(t, handler)
	doGet(t, client, srv.URL+"/x")
	path := filepath.Join(t.TempDir(), "c.json")
	if err := store.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	snapshot := func() (int, string, string) {
		loaded, err := LoadFrom(path, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		c := &http.Client{Transport: NewTransport(ModeReplay, loaded, nil)}
		code, hdr, body := doGet(t, c, srv.URL+"/x")
		return code, hdr.Get("Content-Type"), body
	}
	c0, h0, b0 := snapshot()
	for i := 0; i < 5; i++ {
		c, h, b := snapshot()
		if c != c0 || h != h0 || b != b0 {
			t.Fatalf("run %d differs: (%d,%q,%q) vs (%d,%q,%q)", i, c, h, b, c0, h0, b0)
		}
	}
	if !strings.Contains(b0, `"a":1`) {
		t.Fatalf("unexpected body %q", b0)
	}
}
