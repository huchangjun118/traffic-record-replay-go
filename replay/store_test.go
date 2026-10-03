package replay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func sampleStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore("s", nil, nil, nil)
	s.Append(
		RecordedRequest{Method: "GET", URL: "http://h/a", Header: http.Header{"Authorization": []string{"secret"}}},
		RecordedResponse{StatusCode: 200, Body: []byte("A")},
	)
	return s
}

func TestLoadCorruptChecksum(t *testing.T) {
	s := sampleStore(t)
	path := filepath.Join(t.TempDir(), "c.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	// 篡改 payload 中一个字节（响应体 base64 为 "QQ=="，保持 JSON 合法地改动它）。
	data2 := []byte(strings.Replace(string(data), `"QQ=="`, `"QR=="`, 1))
	if err := os.WriteFile(path, data2, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFrom(path, nil, nil)
	var ce *ErrCorruptCassette
	if !errors.As(err, &ce) {
		t.Fatalf("expected ErrCorruptCassette, got %v", err)
	}
	if !IsCorrupt(err) {
		t.Fatal("IsCorrupt should be true")
	}
}

func TestLoadTruncatedFile(t *testing.T) {
	s := sampleStore(t)
	path := filepath.Join(t.TempDir(), "c.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	// 模拟进程被杀留下的半截文件。
	if err := os.WriteFile(path, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFrom(path, nil, nil)
	if !IsCorrupt(err) {
		t.Fatalf("expected corrupt error, got %v", err)
	}
}

func TestLoadUnsupportedVersion(t *testing.T) {
	s := sampleStore(t)
	path := filepath.Join(t.TempDir(), "c.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	env["version"] = 999
	out, _ := json.Marshal(env)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFrom(path, nil, nil)
	var ve *ErrUnsupportedVersion
	if !errors.As(err, &ve) {
		t.Fatalf("expected ErrUnsupportedVersion, got %v", err)
	}
	if !IsUnsupportedVersion(err) {
		t.Fatal("IsUnsupportedVersion should be true")
	}
	if IsCorrupt(err) {
		t.Fatal("version error must be distinguishable from corruption")
	}
}

func TestSaveAtomicNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	s := sampleStore(t)
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	// 再次保存（覆盖）后目录中不应残留临时文件。
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cassette-") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
	// 旧内容仍可整体加载。
	if _, err := LoadFrom(path, nil, nil); err != nil {
		t.Fatalf("load after overwrite: %v", err)
	}
}

func TestMaskingAppliedAtRecord(t *testing.T) {
	mk := NewMasker(MaskConfig{Headers: []string{"Authorization"}})
	s := NewStore("s", nil, mk, nil)
	s.Append(
		RecordedRequest{Method: "GET", URL: "http://h/a", Header: http.Header{"Authorization": []string{"secret"}}},
		RecordedResponse{StatusCode: 200, Header: http.Header{"Authorization": []string{"resp-secret"}}},
	)
	path := filepath.Join(t.TempDir(), "c.json")
	if err := s.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("sensitive value leaked to disk")
	}
	if !strings.Contains(string(raw), "***") {
		t.Fatal("expected masked replacement on disk")
	}
}

func TestConcurrentStoresIsolated(t *testing.T) {
	// 多个用例各自持有 Store，并发录制互不串扰。
	const stores = 4
	const per = 50
	var wg sync.WaitGroup
	results := make([]*Store, stores)
	for i := 0; i < stores; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := NewStore("case", nil, nil, nil)
			for j := 0; j < per; j++ {
				url := "http://h/" + strings.Repeat("x", i+1)
				s.Append(RecordedRequest{Method: "GET", URL: url},
					RecordedResponse{StatusCode: 200, Body: []byte{byte('a' + i)}})
			}
			results[i] = s
		}(i)
	}
	wg.Wait()
	for i, s := range results {
		if s.Len() != per {
			t.Fatalf("store %d has %d interactions, want %d", i, s.Len(), per)
		}
		// 各自记录的主体应为本用例专属字节。
		it, err := s.Lookup(context.Background(), RecordedRequest{Method: "GET", URL: "http://h/" + strings.Repeat("x", i+1)})
		if err != nil {
			t.Fatal(err)
		}
		if it.Response.Body[0] != byte('a'+i) {
			t.Fatalf("store %d contaminated: body=%q", i, it.Response.Body)
		}
	}
}

func TestConcurrentAppendAndLookupConsistent(t *testing.T) {
	s := NewStore("s", nil, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := "http://h/p" + string(rune('0'+i))
			for j := 0; j < 100; j++ {
				s.Append(RecordedRequest{Method: "GET", URL: url}, RecordedResponse{StatusCode: 200})
				if _, err := s.Lookup(context.Background(), RecordedRequest{Method: "GET", URL: url}); err != nil {
					t.Errorf("lookup after append failed: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if s.Len() != 800 {
		t.Fatalf("len=%d want 800", s.Len())
	}
	if s.Metrics().Hits() != 800 {
		t.Fatalf("hits=%d want 800", s.Metrics().Hits())
	}
}

func TestLookupCancellation(t *testing.T) {
	s := sampleStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Lookup(ctx, RecordedRequest{Method: "GET", URL: "http://h/a"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// 取消不应消费记录：用未取消的 ctx 仍能命中。
	if _, err := s.Lookup(context.Background(), RecordedRequest{Method: "GET", URL: "http://h/a"}); err != nil {
		t.Fatalf("lookup after cancel: %v", err)
	}
}

func TestReplayTransportCancellation(t *testing.T) {
	s := sampleStore(t)
	tr := NewTransport(ModeReplay, s, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://h/a", nil)
	if _, err := tr.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestLookupMissErrorIdentifiesRequest(t *testing.T) {
	s := NewStore("s", nil, nil, nil)
	_, err := s.Lookup(context.Background(), RecordedRequest{Method: "GET", URL: "http://h/none"})
	var nm *ErrNoMatch
	if !errors.As(err, &nm) {
		t.Fatalf("expected ErrNoMatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "GET") || !strings.Contains(err.Error(), "http://h/none") {
		t.Fatalf("error should identify the request: %v", err)
	}
}
