package replay

import "net/http"

// FormatVersion 当前记录格式版本。
const FormatVersion = 1

// Interaction 一条请求/响应记录。
type Interaction struct {
	ID       int64            `json:"id"`
	Request  RecordedRequest  `json:"request"`
	Response RecordedResponse `json:"response"`
}

// RecordedRequest 录制的请求。
type RecordedRequest struct {
	Method string      `json:"method"`
	URL    string      `json:"url"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body,omitempty"`
}

// RecordedResponse 录制的响应。
type RecordedResponse struct {
	StatusCode int         `json:"status_code"`
	Header     http.Header `json:"header"`
	Body       []byte      `json:"body,omitempty"`
}

// Cassette 一份录制文件的内容。
type Cassette struct {
	Version      int           `json:"version"`
	Name         string        `json:"name"`
	Interactions []Interaction `json:"interactions"`
}
