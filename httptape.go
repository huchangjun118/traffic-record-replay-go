package httptape

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Mode 表示工作模式。
type Mode int

const (
	// ModeRecord 录制模式：真实发起请求并把请求/响应落盘。
	ModeRecord Mode = iota
	// ModeReplay 回放模式：完全不发起真实网络访问，从录制内容构造响应。
	ModeReplay
)

// SelectionPolicy 控制同一条匹配键对应多条记录时的选取规则。
type SelectionPolicy int

const (
	// PolicyFirst 总是选取序号最小（最先录制）的记录，结果与请求到达顺序无关。
	PolicyFirst SelectionPolicy = iota
	// PolicyRoundRobin 按录制顺序依次选取、循环复用，确定且可复现。
	PolicyRoundRobin
)

// MissPolicy 控制回放命中不到记录时的策略。
type MissPolicy int

const (
	// MissError 返回明确错误，不透传、不编造响应（默认策略）。
	MissError MissPolicy = iota
	// MissStatus 返回配置的确定性响应。
	MissStatus
)

// MissResponse 描述 MissStatus 策略下返回的确定性响应。
type MissResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Config 是录制器/回放器的配置。
type Config struct {
	// Mode 工作模式，必填。
	Mode Mode
	// StoragePath 录制文件所在目录，必填。
	StoragePath string
	// Session 会话名，决定文件名，多个用例使用不同会话名隔离。
	Session string
	// Match 匹配规则，零值使用 DefaultMatchConfig。
	Match MatchConfig
	// Sanitize 脱敏配置。
	Sanitize SanitizeConfig
	// Selection 多条匹配记录时的选取策略，默认 PolicyFirst。
	Selection SelectionPolicy
	// Miss 命中不到记录时的策略，默认 MissError。
	Miss MissPolicy
	// MissResponse MissStatus 策略下使用的响应。
	MissResponse MissResponse
	// Transport 录制模式下使用的真实传输，为空时使用 http.DefaultTransport。
	Transport http.RoundTripper
	// MaxBodyBytes 单条请求体/响应体落盘的最大字节数，超出后只记录摘要，
	// 为空时使用默认值。
	MaxBodyBytes int
}

// Stats 是可观测的匹配统计。
type Stats struct {
	Hits    uint64
	Misses  uint64
	Records int
}

// ErrBodyTruncated 表示命中的记录在录制时因体积过大只保存了前缀，无法回放。
var ErrBodyTruncated = errors.New("httptape: record body was truncated during recording")

const defaultMaxBodyBytes = 10 << 20 // 10 MiB

// Recorder 同时承担录制与回放职责，实现 http.RoundTripper。
type Recorder struct {
	cfg Config

	// 录制状态
	tw       *tapeWriter
	recordMu sync.Mutex
	next     http.RoundTripper
	seq      atomic.Int64

	// 回放状态
	index    map[string][]*Record
	indexMu  sync.RWMutex
	cursorMu sync.Mutex
	cursors  map[string]int

	hits   atomic.Uint64
	misses atomic.Uint64
}

// New 根据配置创建录制器或回放器。
func New(cfg Config) (*Recorder, error) {
	if cfg.StoragePath == "" {
		return nil, errors.New("httptape: StoragePath must not be empty")
	}
	if err := safeSession(cfg.Session); err != nil {
		return nil, err
	}
	if !cfg.Match.Method && !cfg.Match.Path && !cfg.Match.Query && !cfg.Match.Headers && !cfg.Match.Body {
		cfg.Match = DefaultMatchConfig()
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	switch cfg.Mode {
	case ModeRecord:
		return newRecorder(cfg)
	case ModeReplay:
		return newReplayer(cfg)
	default:
		return nil, fmt.Errorf("httptape: unknown mode %d", cfg.Mode)
	}
}

func newRecorder(cfg Config) (*Recorder, error) {
	tw, err := newTapeWriter(cfg.StoragePath, cfg.Session)
	if err != nil {
		return nil, err
	}
	next := cfg.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	return &Recorder{
		cfg:  cfg,
		tw:   tw,
		next: next,
	}, nil
}

func newReplayer(cfg Config) (*Recorder, error) {
	records, err := loadTape(cfg.StoragePath, cfg.Session)
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Seq < records[j].Seq })
	index := make(map[string][]*Record, len(records))
	for _, rec := range records {
		key := cfg.Match.matchKey(rec.Request.Method, rec.Request.URL, rec.Request.Header, rec.Request.Body)
		index[key] = append(index[key], rec)
	}
	return &Recorder{
		cfg:     cfg,
		index:   index,
		cursors: make(map[string]int),
	}, nil
}

// RoundTrip 实现 http.RoundTripper。
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.cfg.Mode == ModeRecord {
		return r.recordRoundTrip(req)
	}
	return r.replayRoundTrip(req)
}

// Client 返回一个使用本录制器作为 Transport 的 *http.Client。
func (r *Recorder) Client() *http.Client {
	return &http.Client{Transport: r}
}

// Close 提交录制内容（原子落盘）或释放回放资源。
func (r *Recorder) Close() error {
	if r.cfg.Mode != ModeRecord {
		return nil
	}
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	if r.tw == nil {
		return nil
	}
	final := finalPath(r.cfg.StoragePath, r.cfg.Session)
	tmpName := r.tw.f.Name()
	if err := r.tw.commit(final); err != nil {
		// 提交失败（如 rename 失败）时清理临时文件，避免残留。
		_ = os.Remove(tmpName)
		return err
	}
	r.tw = nil
	return nil
}

// Abort 放弃本次录制，删除临时文件，不产生最终录制文件。
func (r *Recorder) Abort() error {
	if r.cfg.Mode != ModeRecord {
		return nil
	}
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	if r.tw == nil {
		return nil
	}
	err := r.tw.abort()
	r.tw = nil
	return err
}

// Stats 返回当前匹配统计快照。
func (r *Recorder) Stats() Stats {
	r.indexMu.RLock()
	n := 0
	for _, cands := range r.index {
		n += len(cands)
	}
	r.indexMu.RUnlock()
	return Stats{Hits: r.hits.Load(), Misses: r.misses.Load(), Records: n}
}

// ---- 录制 ----

func (r *Recorder) recordRoundTrip(req *http.Request) (*http.Response, error) {
	reqBody, err := readRequestBody(req)
	if err != nil {
		return nil, err
	}
	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	respBody, truncated, err := readResponseBody(resp, r.cfg.MaxBodyBytes)
	if err != nil {
		return nil, err
	}
	s := r.cfg.Sanitize
	rec := &Record{
		Seq:        r.seq.Add(1),
		RecordedAt: time.Now(),
		Request: TapeRequest{
			Method: req.Method,
			URL:    req.URL.String(),
			Header: s.sanitizeHeaders(req.Header),
			Body:   s.sanitizeBody(reqBody),
		},
		Response: TapeResponse{
			StatusCode:    resp.StatusCode,
			Status:        resp.Status,
			Header:        s.sanitizeHeaders(resp.Header),
			Body:          s.sanitizeBody(respBody),
			BodyTruncated: truncated,
		},
	}
	r.recordMu.Lock()
	defer r.recordMu.Unlock()
	if r.tw == nil {
		return nil, errors.New("httptape: recorder already closed")
	}
	if err := r.tw.append(rec); err != nil {
		return nil, err
	}
	if err := r.tw.flush(); err != nil {
		return nil, err
	}
	return resp, nil
}

// ---- 回放 ----

func (r *Recorder) replayRoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	reqBody, err := readRequestBody(req)
	if err != nil {
		return nil, err
	}
	// 对入站请求做与落盘时一致的脱敏后再计算匹配键，
	// 保证脱敏配置下录制与回放的键口径一致。
	s := r.cfg.Sanitize
	key := r.cfg.Match.matchKey(req.Method, req.URL.String(), s.sanitizeHeaders(req.Header), s.sanitizeBody(reqBody))
	r.indexMu.RLock()
	cands := r.index[key]
	r.indexMu.RUnlock()
	if len(cands) == 0 {
		r.misses.Add(1)
		return r.missResponse(req)
	}
	rec := r.selectRecord(key, cands)
	if rec.Response.BodyTruncated {
		return nil, fmt.Errorf("%w: %s %s", ErrBodyTruncated, req.Method, req.URL.String())
	}
	r.hits.Add(1)
	return buildResponse(rec, req), nil
}

// selectRecord 按配置的策略从候选记录中选取一条。
func (r *Recorder) selectRecord(key string, cands []*Record) *Record {
	if r.cfg.Selection == PolicyRoundRobin {
		r.cursorMu.Lock()
		idx := r.cursors[key]
		r.cursors[key] = (idx + 1) % len(cands)
		r.cursorMu.Unlock()
		return cands[idx]
	}
	return cands[0]
}

// missResponse 构造未命中时的确定性响应或错误。
func (r *Recorder) missResponse(req *http.Request) (*http.Response, error) {
	if r.cfg.Miss == MissStatus {
		mr := r.cfg.MissResponse
		if mr.StatusCode == 0 {
			mr.StatusCode = http.StatusOK
		}
		body := mr.Body
		if body == nil {
			body = []byte{}
		}
		header := mr.Header
		if header == nil {
			header = http.Header{}
		}
		return &http.Response{
			Status:        fmt.Sprintf("%d %s", mr.StatusCode, http.StatusText(mr.StatusCode)),
			StatusCode:    mr.StatusCode,
			Header:        header.Clone(),
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       req,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
		}, nil
	}
	return nil, fmt.Errorf("%w: %s %s", ErrNoMatch, req.Method, req.URL.String())
}

// buildResponse 用录制内容构造响应。
func buildResponse(rec *Record, req *http.Request) *http.Response {
	body := rec.Response.Body
	if body == nil {
		body = []byte{}
	}
	status := rec.Response.Status
	if status == "" {
		status = fmt.Sprintf("%d %s", rec.Response.StatusCode, http.StatusText(rec.Response.StatusCode))
	}
	header := rec.Response.Header
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		Status:        status,
		StatusCode:    rec.Response.StatusCode,
		Header:        header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
	}
}

// ---- 辅助 ----

// readRequestBody 读取并恢复请求体，使后续流程仍可读取。
func readRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

// readResponseBody 读取完整响应体并替换为可重复读取的字节流；
// 超出 maxBytes 时只保留前缀用于落盘，调用方仍拿到完整响应。
func readResponseBody(resp *http.Response, maxBytes int) ([]byte, bool, error) {
	if resp.Body == nil {
		return nil, false, nil
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, false, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	truncated := false
	stored := data
	if maxBytes > 0 && len(data) > maxBytes {
		stored = append([]byte(nil), data[:maxBytes]...)
		truncated = true
	}
	return stored, truncated, nil
}
