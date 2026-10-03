package replay

// Mode 表示录制/回放模式。
type Mode int

const (
	// ModeRecord 录制模式：真实请求透传并落盘。
	ModeRecord Mode = iota
	// ModeReplay 回放模式：只命中本地记录，绝不发起真实网络访问。
	ModeReplay
)

// String 返回模式名称。
func (m Mode) String() string {
	switch m {
	case ModeRecord:
		return "record"
	case ModeReplay:
		return "replay"
	default:
		return "unknown"
	}
}

// ParseMode 解析模式字符串，非法值返回错误。
func ParseMode(s string) (Mode, error) {
	switch s {
	case "record":
		return ModeRecord, nil
	case "replay":
		return ModeReplay, nil
	default:
		return 0, &ErrInvalidMode{Value: s}
	}
}
