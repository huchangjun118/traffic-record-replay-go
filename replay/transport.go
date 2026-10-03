package replay

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

// Transport 实现 http.RoundTripper，按模式录制或回放。
type Transport struct {
	mode  Mode
	store *Store
	real  http.RoundTripper // 仅录制模式使用
}

// NewTransport 创建传输层。mode 必须显式给出。
// 录制模式下 real 为底层真实传输（nil 则使用 http.DefaultTransport）；
// 回放模式下 real 被忽略，绝不发起真实网络访问。
func NewTransport(mode Mode, store *Store, real http.RoundTripper) *Transport {
	if real == nil {
		real = http.DefaultTransport
	}
	return &Transport{mode: mode, store: store, real: real}
}

// RoundTrip 实现 http.RoundTripper。
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch t.mode {
	case ModeRecord:
		return t.record(req)
	case ModeReplay:
		return t.replay(req)
	default:
		return nil, &ErrInvalidMode{Value: fmt.Sprintf("%d", int(t.mode))}
	}
}

// record 透传真实请求并把请求/响应落进存储。
func (t *Transport) record(req *http.Request) (*http.Response, error) {
	reqBody, err := drainBody(req)
	if err != nil {
		return nil, fmt.Errorf("replay: read request body: %w", err)
	}
	resp, err := t.real.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("replay: read response body: %w", err)
	}
	// 复原响应体，调用方照常消费。
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	resp.ContentLength = int64(len(respBody))

	t.store.Append(
		RecordedRequest{
			Method: req.Method,
			URL:    req.URL.String(),
			Header: req.Header.Clone(),
			Body:   reqBody,
		},
		RecordedResponse{
			StatusCode: resp.StatusCode,
			Header:     resp.Header.Clone(),
			Body:       respBody,
		},
	)
	return resp, nil
}

// replay 只查本地记录，绝不发起真实网络访问。
// 未命中时返回明确的 *ErrNoMatch，不透传、不编造。
func (t *Transport) replay(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	reqBody, err := drainBody(req)
	if err != nil {
		return nil, fmt.Errorf("replay: read request body: %w", err)
	}
	it, err := t.store.Lookup(req.Context(), RecordedRequest{
		Method: req.Method,
		URL:    req.URL.String(),
		Header: req.Header.Clone(),
		Body:   reqBody,
	})
	if err != nil {
		return nil, err
	}
	body := it.Response.Body
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", it.Response.StatusCode, http.StatusText(it.Response.StatusCode)),
		StatusCode:    it.Response.StatusCode,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        it.Response.Header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// drainBody 读取并复原请求体，使请求可继续被底层传输使用。
func drainBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	b, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return b, nil
}
