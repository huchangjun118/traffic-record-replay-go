package replay

import "fmt"

// ErrInvalidMode 表示非法的模式字符串。
type ErrInvalidMode struct{ Value string }

func (e *ErrInvalidMode) Error() string { return fmt.Sprintf("replay: invalid mode %q", e.Value) }

// ErrNoMatch 表示回放时未命中任何记录。
type ErrNoMatch struct {
	Method string
	Path   string
}

func (e *ErrNoMatch) Error() string {
	return fmt.Sprintf("replay: no recorded interaction matches %s %s", e.Method, e.Path)
}

// ErrAmbiguousMatch 表示按消费策略匹配到多条且无法裁决（保留用于可解释性）。
type ErrAmbiguousMatch struct {
	Method string
	Path   string
	Count  int
}

func (e *ErrAmbiguousMatch) Error() string {
	return fmt.Sprintf("replay: %d recorded interactions match %s %s", e.Count, e.Method, e.Path)
}

// ErrUnsupportedVersion 表示记录格式版本不受支持。
type ErrUnsupportedVersion struct{ Version int }

func (e *ErrUnsupportedVersion) Error() string {
	return fmt.Sprintf("replay: unsupported cassette version %d", e.Version)
}

// ErrCorruptCassette 表示记录文件结构损坏或校验失败。
type ErrCorruptCassette struct{ Reason string }

func (e *ErrCorruptCassette) Error() string {
	return fmt.Sprintf("replay: corrupt cassette: %s", e.Reason)
}
