package httptape

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 录制文件相关的错误，调用方可以用 errors.Is 区分失败原因。
var (
	// ErrCorrupted 表示录制文件结构损坏、缺少提交标记或校验和不符。
	ErrCorrupted = errors.New("httptape: corrupted tape file")
	// ErrUnsupportedVersion 表示录制文件版本不受支持。
	ErrUnsupportedVersion = errors.New("httptape: unsupported tape version")
	// ErrNoMatch 表示回放时找不到匹配的记录。
	ErrNoMatch = errors.New("httptape: no matching record")
)

const (
	magicPrefix = "TRTAPE"
	magicV1     = "TRTAPE1"
	commitType  = "commit"
)

// Record 是一条完整的请求/响应记录。
type Record struct {
	Seq        int64        `json:"seq"`
	RecordedAt time.Time    `json:"recorded_at"`
	Request    TapeRequest  `json:"request"`
	Response   TapeResponse `json:"response"`
}

// TapeRequest 描述一次请求。
type TapeRequest struct {
	Method        string      `json:"method"`
	URL           string      `json:"url"`
	Header        http.Header `json:"header,omitempty"`
	Body          []byte      `json:"body,omitempty"`
	BodyTruncated bool        `json:"body_truncated,omitempty"`
}

// TapeResponse 描述一次响应。
type TapeResponse struct {
	StatusCode    int         `json:"status_code"`
	Status        string      `json:"status"`
	Header        http.Header `json:"header,omitempty"`
	Body          []byte      `json:"body,omitempty"`
	BodyTruncated bool        `json:"body_truncated,omitempty"`
}

// commitMarker 是录制文件末尾的提交标记。
type commitMarker struct {
	Type     string `json:"type"`
	Records  int    `json:"records"`
	Checksum string `json:"checksum"`
}

// finalPath 返回录制文件的最终路径。
func finalPath(dir, session string) string {
	return filepath.Join(dir, session+".tape")
}

// safeSession 校验会话名，避免路径穿越。
func safeSession(session string) error {
	if session == "" {
		return errors.New("httptape: session name must not be empty")
	}
	if session == "." || session == ".." || strings.ContainsAny(session, `/\`) {
		return fmt.Errorf("httptape: invalid session name %q", session)
	}
	return nil
}

// ListSessions 列出目录下所有已提交的录制会话名。
func ListSessions(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var sessions []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".tape") {
			sessions = append(sessions, strings.TrimSuffix(name, ".tape"))
		}
	}
	return sessions, nil
}

// tapeWriter 把记录流式写入临时文件，提交时原子 rename。
// 中途失败只会留下临时文件，不会产生半截的最终录制文件。
type tapeWriter struct {
	f         *os.File
	bw        *bufio.Writer
	h         hash.Hash
	n         int
	committed bool
}

// newTapeWriter 在 dir 下创建临时录制文件并写入版本魔数。
func newTapeWriter(dir, session string) (*tapeWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "."+session+".tape.tmp-*")
	if err != nil {
		return nil, err
	}
	tw := &tapeWriter{
		f:  f,
		bw: bufio.NewWriter(f),
		h:  sha256.New(),
	}
	if _, err := tw.bw.WriteString(magicV1 + "\n"); err != nil {
		tw.cleanup()
		return nil, err
	}
	return tw, nil
}

// append 写入一条记录并把字节计入校验和。
func (tw *tapeWriter) append(rec *Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := tw.bw.Write(data); err != nil {
		return err
	}
	tw.h.Write(data)
	tw.n++
	return nil
}

// flush 把缓冲数据刷到操作系统（不 fsync）。
func (tw *tapeWriter) flush() error {
	return tw.bw.Flush()
}

// commit 写入提交标记、fsync 后原子替换为最终文件。
func (tw *tapeWriter) commit(final string) error {
	m := commitMarker{
		Type:     commitType,
		Records:  tw.n,
		Checksum: hex.EncodeToString(tw.h.Sum(nil)),
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := tw.bw.Write(data); err != nil {
		return err
	}
	if err := tw.bw.Flush(); err != nil {
		return err
	}
	if err := tw.f.Sync(); err != nil {
		return err
	}
	if err := tw.f.Close(); err != nil {
		return err
	}
	tw.committed = true
	return os.Rename(tw.f.Name(), final)
}

// abort 放弃录制，关闭并删除临时文件。
func (tw *tapeWriter) abort() error {
	if tw.committed {
		return nil
	}
	name := tw.f.Name()
	_ = tw.bw.Flush()
	_ = tw.f.Close()
	return os.Remove(name)
}

// cleanup 在构造失败时清理临时文件。
func (tw *tapeWriter) cleanup() {
	_ = tw.bw.Flush()
	_ = tw.f.Close()
	_ = os.Remove(tw.f.Name())
}

// loadTape 读取并解析录制文件。
func loadTape(dir, session string) ([]*Record, error) {
	data, err := os.ReadFile(finalPath(dir, session))
	if err != nil {
		return nil, err
	}
	return parseTape(data)
}

// parseTape 解析录制内容，校验版本、提交标记与校验和。
// 结构损坏返回 ErrCorrupted，版本不受支持返回 ErrUnsupportedVersion。
func parseTape(data []byte) ([]*Record, error) {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) == 0 {
		return nil, fmt.Errorf("%w: empty file", ErrCorrupted)
	}
	magic := strings.TrimSpace(string(lines[0]))
	switch magic {
	case magicV1:
		// 支持的版本
	case "":
		return nil, fmt.Errorf("%w: missing magic header", ErrCorrupted)
	default:
		if strings.HasPrefix(magic, magicPrefix) {
			return nil, fmt.Errorf("%w: %s (supported: %s)", ErrUnsupportedVersion, magic, magicV1)
		}
		return nil, fmt.Errorf("%w: bad magic header %q", ErrCorrupted, magic)
	}

	var records []*Record
	h := sha256.New()
	for i, line := range lines[1:] {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return nil, fmt.Errorf("%w: invalid json at line %d: %v", ErrCorrupted, i+2, err)
		}
		if head.Type == commitType {
			var m commitMarker
			if err := json.Unmarshal(line, &m); err != nil {
				return nil, fmt.Errorf("%w: invalid commit marker: %v", ErrCorrupted, err)
			}
			if m.Records != len(records) {
				return nil, fmt.Errorf("%w: record count mismatch: commit says %d, found %d", ErrCorrupted, m.Records, len(records))
			}
			if got := hex.EncodeToString(h.Sum(nil)); m.Checksum != got {
				return nil, fmt.Errorf("%w: checksum mismatch: commit says %s, computed %s", ErrCorrupted, m.Checksum, got)
			}
			return records, nil
		}
		rec := &Record{}
		if err := json.Unmarshal(line, rec); err != nil {
			return nil, fmt.Errorf("%w: invalid record at line %d: %v", ErrCorrupted, i+2, err)
		}
		h.Write(line)
		h.Write([]byte("\n"))
		records = append(records, rec)
	}
	return nil, fmt.Errorf("%w: missing commit marker (incomplete write)", ErrCorrupted)
}

// 确保 io 包被引用（Record 之外的工具函数预留）。
var _ = io.EOF
