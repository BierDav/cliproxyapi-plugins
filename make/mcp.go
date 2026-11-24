package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
)

const mcpURL = "https://mcp.make.com/v2"

// Make's edge rejects some default library user agents.
const userAgent = "cliproxyapi-make/" + pluginVersion

type upstreamResponse = framework.HTTPResponse

// httpCall performs an upstream request through the host transport, which
// applies the configured proxy and connection policy.
func httpCall(callback, method, address string, headers http.Header, body []byte) (upstreamResponse, error) {
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("User-Agent", userAgent)
	// Make's MCP edge mislabels compressed SSE bodies; plain bodies avoid
	// "gzip: invalid header" in the host transport.
	headers.Set("Accept-Encoding", "identity")
	response, err := host().Do(context.Background(), callback, framework.HTTPRequest{Method: method, URL: address, Headers: headers, Body: body})
	switch {
	case errors.Is(err, framework.ErrHostResponse):
		return response, newStatusError(502, "Invalid Make transport response")
	case err != nil:
		// Host transport errors describe the connection, never request headers.
		return response, newStatusError(502, "Make transport failed: %s", truncate(err.Error(), 300))
	}
	return response, nil
}

var rpcID atomic.Int64

// mcpError is a tool-level failure reported by the Make MCP server. Its text
// is Make's own explanation (no credentials), so it is safe to surface.
type mcpError struct {
	tool    string
	status  int
	message string
}

// pauseCooldown is how long the host skips a credential whose organization is
// paused. The pause lasts until Make's monthly credits reset or more are
// bought, so the host checks back now and then rather than on every request.
const pauseCooldown = 30 * time.Minute

// QuotaLimit tells the host that a paused organization is a limit on the
// whole credential, so it cools the credential down for every model and moves
// on to another Make organization.
func (e *mcpError) QuotaLimit() (int, bool) {
	if e.status != http.StatusTooManyRequests || !pausedMessage(e.message) {
		return 0, false
	}
	return int(pauseCooldown / time.Second), true
}

// StatusCode gives the framework the HTTP status Make reported, if any.
func (e *mcpError) StatusCode() int {
	if e.status >= 400 {
		return e.status
	}
	return 0
}

func (e *mcpError) Error() string {
	return fmt.Sprintf("Make %s failed: %s", e.tool, e.message)
}

// callTool invokes one Make MCP tool and returns its structured result.
func callTool(callback, accessToken, tool string, args any) (json.RawMessage, error) {
	body, err := mcpCall(callback, accessToken, tool, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return nil, err
	}
	return decodeToolResult(tool, body)
}

// listTools returns the names of the MCP tools the access token may use.
func listTools(callback, accessToken string) ([]string, error) {
	body, err := mcpCall(callback, accessToken, "tools/list", "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if json.Unmarshal(rpcPayload(body), &envelope) != nil || envelope.Result == nil {
		return nil, newStatusError(502, "Invalid Make MCP tool list")
	}
	names := make([]string, 0, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

// mcpCall sends one JSON-RPC request to Make's MCP server and returns the
// response body. label names the request in errors.
func mcpCall(callback, accessToken, label, method string, params any) ([]byte, error) {
	request := map[string]any{"jsonrpc": "2.0", "id": rpcID.Add(1), "method": method, "params": params}
	headers := http.Header{
		"Authorization": {"Bearer " + accessToken},
		"Content-Type":  {"application/json"},
		"Accept":        {"application/json, text/event-stream"},
	}
	response, err := httpCall(callback, "POST", mcpURL, headers, encode(request))
	if err != nil {
		return nil, err
	}
	if response.StatusCode == 401 {
		return nil, newStatusError(401, "Make rejected the access token")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(response.Body, &failure)
		status := response.StatusCode
		message := strings.TrimSpace(failure.Message)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", status)
		}
		return nil, &mcpError{tool: label, status: status, message: message}
	}
	return response.Body, nil
}

// rpcPayload is the JSON-RPC message of a plain JSON or single-event SSE body.
func rpcPayload(body []byte) []byte {
	payload := bytes.TrimSpace(body)
	if !bytes.HasPrefix(payload, []byte("{")) {
		var last []byte
		for _, line := range bytes.Split(payload, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				last = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			}
		}
		payload = last
	}
	return payload
}

// decodeToolResult accepts both plain JSON-RPC and single-event SSE bodies.
func decodeToolResult(tool string, body []byte) (json.RawMessage, error) {
	payload := rpcPayload(body)
	var envelope struct {
		Result *struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
			Content           []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &envelope) != nil {
		return nil, newStatusError(502, "Invalid Make MCP response")
	}
	if envelope.Error != nil {
		message := strings.TrimSpace(envelope.Error.Message)
		return nil, &mcpError{tool: tool, status: toolErrorStatus(message), message: message}
	}
	if envelope.Result == nil {
		return nil, newStatusError(502, "Empty Make MCP response")
	}
	var text strings.Builder
	for _, item := range envelope.Result.Content {
		if item.Type == "text" {
			text.WriteString(item.Text)
		}
	}
	if envelope.Result.IsError {
		message := strings.TrimSpace(text.String())
		if message == "" {
			message = "tool error"
		}
		return nil, &mcpError{tool: tool, status: toolErrorStatus(message), message: truncate(message, 400)}
	}
	if len(envelope.Result.StructuredContent) > 0 && string(envelope.Result.StructuredContent) != "null" {
		return envelope.Result.StructuredContent, nil
	}
	trimmed := strings.TrimSpace(text.String())
	if json.Valid([]byte(trimmed)) && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) {
		return json.RawMessage(trimmed), nil
	}
	return encode(map[string]string{"text": trimmed}), nil
}

// toolErrorStatus maps a Make tool error to an HTTP status. A paused
// organization or team ("Nothing ran: the organization or team is paused
// until its operations or data-transfer limit is resolved.") is a quota
// limit, so the host cools the credential down instead of treating Make as
// broken.
func toolErrorStatus(message string) int {
	if pausedMessage(message) {
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
}

func pausedMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "paused until") || strings.Contains(message, "operations or data-transfer limit")
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
