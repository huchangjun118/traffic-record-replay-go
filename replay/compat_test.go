package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeCassetteFile 按文档格式手工构造一份记录文件（模拟旧版本工具产出）。
func writeCassetteFile(t *testing.T, path string, version int, c Cassette) {
	t.Helper()
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	env := map[string]any{
		"version":  version,
		"checksum": hex.EncodeToString(sum[:]),
		"payload":  json.RawMessage(payload),
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandcraftedV1CassetteReplays(t *testing.T) {
	// 按声明兼容的 v1 格式手工构造的记录必须能正确回放，
	// 以此锁定格式演进时的向后兼容契约。
	c := Cassette{
		Version: 1,
		Name:    "legacy",
		Interactions: []Interaction{{
			ID: 1,
			Request: RecordedRequest{
				Method: "GET",
				URL:    "http://example.com/api?b=2&a=1",
			},
			Response: RecordedResponse{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       []byte("legacy-body"),
			},
		}},
	}
	path := filepath.Join(t.TempDir(), "legacy.json")
	writeCassetteFile(t, path, 1, c)

	store, err := LoadFrom(path, nil, nil)
	if err != nil {
		t.Fatalf("load v1 cassette: %v", err)
	}
	// 查询参数顺序不同也应命中（归一化）。
	client := &http.Client{Transport: NewTransport(ModeReplay, store, nil)}
	resp, err := client.Get("http://example.com/api?a=1&b=2")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	if string(body[:n]) != "legacy-body" {
		t.Fatalf("body=%q", body[:n])
	}
}

func TestFutureVersionRejected(t *testing.T) {
	c := Cassette{Version: FormatVersion + 1, Name: "future"}
	path := filepath.Join(t.TempDir(), "future.json")
	writeCassetteFile(t, path, FormatVersion+1, c)
	if _, err := LoadFrom(path, nil, nil); !IsUnsupportedVersion(err) {
		t.Fatalf("expected unsupported version, got %v", err)
	}
}

func TestMissingFileError(t *testing.T) {
	_, err := LoadFrom(filepath.Join(t.TempDir(), "nope.json"), nil, nil)
	if err == nil || IsCorrupt(err) || IsUnsupportedVersion(err) {
		t.Fatalf("missing file should be a plain IO error, got %v", err)
	}
}

// 大流量下查找不能随记录条数线性退化：索引查找应接近常数。
func BenchmarkLookupLargeCassette(b *testing.B) {
	s := NewStore("bench", nil, nil, nil)
	const n = 20000
	for i := 0; i < n; i++ {
		s.Append(
			RecordedRequest{Method: "GET", URL: fmt.Sprintf("http://h/p%d?x=%d", i, i)},
			RecordedResponse{StatusCode: 200, Body: []byte("ok")},
		)
	}
	req := RecordedRequest{Method: "GET", URL: fmt.Sprintf("http://h/p%d?x=%d", n-1, n-1)}
	ctx := b.Context()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Lookup(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// 确保基准场景本身功能正确（防止基准测了个寂寞）。
func TestBenchmarkFixtureValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	s := NewStore("t", nil, nil, nil)
	c := &http.Client{Transport: NewTransport(ModeRecord, s, nil)}
	resp, err := c.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if s.Len() != 1 {
		t.Fatalf("len=%d", s.Len())
	}
}
