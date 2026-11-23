package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Caller sends one callback to the host and returns the envelope's result.
// abi.Call reaches the real host; tests substitute a fake.
type Caller func(method string, payload any) (json.RawMessage, error)

// ErrHostResponse marks a host callback whose result could not be decoded,
// as opposed to one the host reported as failed.
var ErrHostResponse = errors.New("malformed host response")

// HTTPRequest is an upstream request the host performs with its configured
// proxy and connection policy.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

// HTTPResponse is a buffered upstream response. The JSON field names are the
// host's.
type HTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// HTTPStream is an upstream response whose body is read with ReadStream.
type HTTPStream struct {
	StatusCode int
	Headers    http.Header
	ID         string
}

// StreamChunk is the next part of an upstream stream body.
type StreamChunk struct {
	Payload []byte
	Error   string
	Done    bool
}

// Transport is the part of Host a provider needs for upstream traffic and
// streamed output. Host implements it; tests can fake it.
type Transport interface {
	Do(ctx context.Context, callbackID string, req HTTPRequest) (HTTPResponse, error)
	OpenStream(ctx context.Context, callbackID string, req HTTPRequest) (HTTPStream, error)
	ReadStream(ctx context.Context, streamID string) (StreamChunk, error)
	CloseStream(ctx context.Context, streamID string) error
	Emit(ctx context.Context, streamID string, payload []byte) error
	CloseOutput(ctx context.Context, streamID, message string)
}

// Host makes the host callbacks of pluginabi through Call. The payloads are
// plain maps so that a fake Caller can inspect them without these types.
type Host struct {
	Call Caller
}

var _ Transport = Host{}

func httpPayload(callbackID string, req HTTPRequest) map[string]any {
	return map[string]any{"host_callback_id": callbackID, "method": req.Method, "url": req.URL, "headers": req.Headers, "body": req.Body}
}

// Do performs an upstream request and buffers its response.
func (h Host) Do(_ context.Context, callbackID string, req HTTPRequest) (HTTPResponse, error) {
	raw, err := h.Call(pluginabi.MethodHostHTTPDo, httpPayload(callbackID, req))
	if err != nil {
		return HTTPResponse{}, err
	}
	var resp HTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return HTTPResponse{}, fmt.Errorf("decode host HTTP response: %w", errors.Join(ErrHostResponse, err))
	}
	return resp, nil
}

// OpenStream starts an upstream request whose body is read in chunks. The
// caller closes the stream with CloseStream, also after an error status.
func (h Host) OpenStream(_ context.Context, callbackID string, req HTTPRequest) (HTTPStream, error) {
	raw, err := h.Call(pluginabi.MethodHostHTTPDoStream, httpPayload(callbackID, req))
	if err != nil {
		return HTTPStream{}, err
	}
	var resp struct {
		StatusCode int         `json:"status_code"`
		Headers    http.Header `json:"headers"`
		StreamID   string      `json:"stream_id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return HTTPStream{}, fmt.Errorf("decode host HTTP stream response: %w", errors.Join(ErrHostResponse, err))
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return HTTPStream{}, fmt.Errorf("host HTTP stream response has no stream_id: %w", ErrHostResponse)
	}
	return HTTPStream{StatusCode: resp.StatusCode, Headers: resp.Headers, ID: resp.StreamID}, nil
}

// ReadStream returns the next chunk of an upstream stream.
func (h Host) ReadStream(_ context.Context, streamID string) (StreamChunk, error) {
	raw, err := h.Call(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": streamID})
	if err != nil {
		return StreamChunk{}, err
	}
	var chunk struct {
		Payload []byte `json:"payload"`
		Error   string `json:"error"`
		Done    bool   `json:"done"`
	}
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return StreamChunk{}, fmt.Errorf("decode host HTTP stream chunk: %w", errors.Join(ErrHostResponse, err))
	}
	return StreamChunk{Payload: chunk.Payload, Error: chunk.Error, Done: chunk.Done}, nil
}

// CloseStream releases an upstream stream.
func (h Host) CloseStream(_ context.Context, streamID string) error {
	_, err := h.Call(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": streamID})
	return err
}

// Emit writes payload to the client's output stream.
func (h Host) Emit(_ context.Context, streamID string, payload []byte) error {
	_, err := h.Call(pluginabi.MethodHostStreamEmit, map[string]any{"stream_id": streamID, "payload": payload})
	return err
}

// CloseOutput ends the client's output stream, with an error message when it
// failed. The host cannot report anything useful back, so errors are dropped.
func (h Host) CloseOutput(_ context.Context, streamID, message string) {
	_, _ = h.Call(pluginabi.MethodHostStreamClose, map[string]string{"stream_id": streamID, "error": message})
}

// Log writes a message to the host's log at level ("debug", "info", "warn" or
// "error"). Logging is best effort, so errors are dropped. Fields must not hold
// credentials.
func (h Host) Log(callbackID, level, message string, fields map[string]any) {
	_, _ = h.Call(pluginabi.MethodHostLog, map[string]any{"host_callback_id": callbackID, "level": level, "message": message, "fields": fields})
}

// SaveAuth stores a credential file in the host's auth directory.
func (h Host) SaveAuth(name string, credential json.RawMessage) error {
	_, err := h.Call(pluginabi.MethodHostAuthSave, map[string]any{"name": name, "json": credential})
	return err
}
