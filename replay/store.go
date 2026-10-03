package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// 版本兼容声明：当前实现能回放 [MinSupportedVersion, FormatVersion] 的记录。
const MinSupportedVersion = 1

// envelope 是落盘文件的外层结构，带版本与校验和。
type envelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"` // 对 Payload 字节的 SHA-256
	Payload  json.RawMessage `json:"payload"`
}

// Store 是录制内容的并发安全存储。
// 每个 Store 实例相互独立，多个用例各自持有实例即可天然隔离；
// 实例内部用互斥锁保证读写一致，慢用例不会阻塞其他实例。
type Store struct {
	mu       sync.RWMutex
	cassette *Cassette
	masker   *Masker
	matcher  *Matcher
	metrics  *Metrics
	nextID   int64
	// consumed 记录每条 interaction 被回放消费的次数（用于确定性选取）。
	consumed map[int64]int
	// index 以匹配键索引 interaction ID（按追加顺序，即 ID 升序），避免线性查找。
	index map[string][]int64
	// byID 以 ID 定位 interaction。
	byID map[int64]*Interaction
}

// NewStore 创建空存储。masker 可为 nil（不脱敏）。
func NewStore(name string, matcher *Matcher, masker *Masker, metrics *Metrics) *Store {
	if matcher == nil {
		matcher = NewMatcher(DefaultMatchConfig())
	}
	if metrics == nil {
		metrics = &Metrics{}
	}
	return &Store{
		cassette: &Cassette{Version: FormatVersion, Name: name},
		masker:   masker,
		matcher:  matcher,
		metrics:  metrics,
		consumed: map[int64]int{},
		index:    map[string][]int64{},
		byID:     map[int64]*Interaction{},
	}
}

// Metrics 返回该存储的统计。
func (s *Store) Metrics() *Metrics { return s.metrics }

// Append 录制一条交互（脱敏后写入内存，并更新索引）。
func (s *Store) Append(req RecordedRequest, resp RecordedResponse) {
	if s.masker != nil {
		req = s.masker.MaskRequest(req)
		resp = s.masker.MaskResponse(resp)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLocked(req, resp)
}

func (s *Store) appendLocked(req RecordedRequest, resp RecordedResponse) {
	s.nextID++
	it := Interaction{ID: s.nextID, Request: req, Response: resp}
	s.cassette.Interactions = append(s.cassette.Interactions, it)
	key := s.matcher.Key(req)
	s.index[key] = append(s.index[key], it.ID)
	s.byID[it.ID] = &s.cassette.Interactions[len(s.cassette.Interactions)-1]
}

// Lookup 按确定性规则选取匹配的记录。
//
// 选取规则（可复现）：在匹配键相同的候选中，选择被消费次数最少者；
// 并列时选择 ID 最小（即录制最早）者。选中后其消费计数加一。
// 因此同一请求重复出现时按录制顺序依次轮询各条记录；
// 请求到达顺序变化时，每个键的选取序列仍只取决于该键下的请求次数，
// 与其他键的请求交错无关。
func (s *Store) Lookup(ctx context.Context, req RecordedRequest) (*Interaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := s.matcher.Key(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.index[key]
	if len(ids) == 0 {
		s.metrics.incMiss()
		return nil, &ErrNoMatch{Method: req.Method, Path: req.URL}
	}
	best := ids[0]
	bestConsumed := s.consumed[best]
	for _, id := range ids[1:] {
		if c := s.consumed[id]; c < bestConsumed {
			best, bestConsumed = id, c
		}
	}
	s.consumed[best]++
	s.metrics.incHit()
	return s.byID[best], nil
}

// Len 返回记录条数。
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.cassette.Interactions)
}

// SaveTo 原子写入文件：先写临时文件并 fsync，再 rename。
// 写入中途失败或进程被杀只会留下临时文件，目标文件要么整体可用，
// 要么保持旧内容，绝不出现半截记录。
func (s *Store) SaveTo(path string) error {
	s.mu.RLock()
	payload, err := json.Marshal(s.cassette)
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("replay: marshal cassette: %w", err)
	}
	sum := sha256.Sum256(payload)
	env := envelope{
		Version:  FormatVersion,
		Checksum: hex.EncodeToString(sum[:]),
		Payload:  payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("replay: marshal envelope: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cassette-*.tmp")
	if err != nil {
		return fmt.Errorf("replay: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 已 rename 后为 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("replay: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("replay: fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("replay: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replay: rename temp file: %w", err)
	}
	return nil
}

// LoadFrom 加载并校验记录文件。
// 校验失败返回可区分的错误：*ErrCorruptCassette（结构损坏/校验和不符）
// 或 *ErrUnsupportedVersion（版本不受支持），绝不返回部分可用的数据。
func LoadFrom(path string, matcher *Matcher, metrics *Metrics) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay: read cassette: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, &ErrCorruptCassette{Reason: "not a valid envelope: " + err.Error()}
	}
	if env.Version < MinSupportedVersion || env.Version > FormatVersion {
		return nil, &ErrUnsupportedVersion{Version: env.Version}
	}
	if len(env.Payload) == 0 {
		return nil, &ErrCorruptCassette{Reason: "empty payload"}
	}
	sum := sha256.Sum256(env.Payload)
	if hex.EncodeToString(sum[:]) != env.Checksum {
		return nil, &ErrCorruptCassette{Reason: "checksum mismatch"}
	}
	var c Cassette
	if err := json.Unmarshal(env.Payload, &c); err != nil {
		return nil, &ErrCorruptCassette{Reason: "invalid payload: " + err.Error()}
	}
	if c.Version != env.Version {
		return nil, &ErrCorruptCassette{Reason: "version mismatch between envelope and payload"}
	}
	for i := range c.Interactions {
		if c.Interactions[i].Request.Method == "" || c.Interactions[i].Request.URL == "" {
			return nil, &ErrCorruptCassette{Reason: fmt.Sprintf("interaction %d missing method or URL", i)}
		}
	}
	s := NewStore(c.Name, matcher, nil, metrics)
	s.cassette = &c
	for i := range c.Interactions {
		it := &c.Interactions[i]
		if it.ID > s.nextID {
			s.nextID = it.ID
		}
		key := s.matcher.Key(it.Request)
		s.index[key] = append(s.index[key], it.ID)
		s.byID[it.ID] = it
	}
	return s, nil
}

// IsCorrupt 判断错误是否为记录损坏。
func IsCorrupt(err error) bool {
	var ce *ErrCorruptCassette
	return errors.As(err, &ce)
}

// IsUnsupportedVersion 判断错误是否为版本不受支持。
func IsUnsupportedVersion(err error) bool {
	var ve *ErrUnsupportedVersion
	return errors.As(err, &ve)
}
