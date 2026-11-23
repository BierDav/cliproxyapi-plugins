package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type recordedCall struct {
	method  string
	payload any
}

func fakeHost(results map[string]string) (Host, *[]recordedCall) {
	var calls []recordedCall
	return Host{Call: func(method string, payload any) (json.RawMessage, error) {
		calls = append(calls, recordedCall{method, payload})
		result, ok := results[method]
		if !ok {
			return nil, errors.New("host refused " + method)
		}
		return json.RawMessage(result), nil
	}}, &calls
}

func TestDoUsesTheHostsResponseShape(t *testing.T) {
	// The host answers with pluginapi.HTTPResponse, which has no JSON tags.
	reply, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 201, Headers: http.Header{"X-Test": {"1"}}, Body: []byte("hello")})
	host, calls := fakeHost(map[string]string{pluginabi.MethodHostHTTPDo: string(reply)})
	resp, err := host.Do(context.Background(), "cb", HTTPRequest{Method: "POST", URL: "https://example.com", Headers: http.Header{"A": {"b"}}, Body: []byte("{}")})
	if err != nil || resp.StatusCode != 201 || string(resp.Body) != "hello" || resp.Headers.Get("X-Test") != "1" {
		t.Fatalf("unexpected response %+v, %v", resp, err)
	}
	payload := (*calls)[0].payload.(map[string]any)
	if payload["host_callback_id"] != "cb" || payload["method"] != "POST" || payload["url"] != "https://example.com" ||
		string(payload["body"].([]byte)) != "{}" || payload["headers"].(http.Header).Get("A") != "b" {
		t.Fatalf("unexpected payload %#v", payload)
	}
}

func TestStreamsAndOutput(t *testing.T) {
	host, calls := fakeHost(map[string]string{
		pluginabi.MethodHostHTTPDoStream:    `{"status_code":200,"headers":{"Content-Type":["text/event-stream"]},"stream_id":"up-1"}`,
		pluginabi.MethodHostHTTPStreamRead:  `{"payload":"ZGF0YTogeAoK","done":true}`,
		pluginabi.MethodHostHTTPStreamClose: `{}`,
		pluginabi.MethodHostStreamEmit:      `{}`,
		pluginabi.MethodHostStreamClose:     `{}`,
		pluginabi.MethodHostAuthSave:        `{}`,
	})
	ctx := context.Background()
	stream, err := host.OpenStream(ctx, "cb", HTTPRequest{Method: "POST", URL: "https://example.com"})
	if err != nil || stream.ID != "up-1" || stream.StatusCode != 200 || stream.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("open: %+v, %v", stream, err)
	}
	chunk, err := host.ReadStream(ctx, stream.ID)
	if err != nil || string(chunk.Payload) != "data: x\n\n" || !chunk.Done {
		t.Fatalf("read: %+v, %v", chunk, err)
	}
	if err := host.CloseStream(ctx, stream.ID); err != nil {
		t.Fatal(err)
	}
	if err := host.Emit(ctx, "out-1", []byte("data: y\n\n")); err != nil {
		t.Fatal(err)
	}
	host.CloseOutput(ctx, "out-1", "failed")
	if err := host.SaveAuth("cred.json", json.RawMessage(`{"type":"test"}`)); err != nil {
		t.Fatal(err)
	}
	want := []string{pluginabi.MethodHostHTTPDoStream, pluginabi.MethodHostHTTPStreamRead, pluginabi.MethodHostHTTPStreamClose,
		pluginabi.MethodHostStreamEmit, pluginabi.MethodHostStreamClose, pluginabi.MethodHostAuthSave}
	for i, method := range want {
		if (*calls)[i].method != method {
			t.Fatalf("call %d is %s, want %s", i, (*calls)[i].method, method)
		}
	}
	if got := (*calls)[4].payload.(map[string]string); got["stream_id"] != "out-1" || got["error"] != "failed" {
		t.Fatalf("close output payload %#v", got)
	}
	if got := (*calls)[5].payload.(map[string]any); got["name"] != "cred.json" || string(got["json"].(json.RawMessage)) != `{"type":"test"}` {
		t.Fatalf("save payload %#v", got)
	}
}

func TestMalformedHostResponsesAreMarked(t *testing.T) {
	host, _ := fakeHost(map[string]string{
		pluginabi.MethodHostHTTPDo:         `[]`,
		pluginabi.MethodHostHTTPDoStream:   `{"status_code":200}`,
		pluginabi.MethodHostHTTPStreamRead: `"x"`,
	})
	ctx := context.Background()
	if _, err := host.Do(ctx, "", HTTPRequest{}); !errors.Is(err, ErrHostResponse) {
		t.Fatalf("Do: %v", err)
	}
	if _, err := host.OpenStream(ctx, "", HTTPRequest{}); !errors.Is(err, ErrHostResponse) {
		t.Fatalf("OpenStream without stream_id: %v", err)
	}
	if _, err := host.ReadStream(ctx, "s"); !errors.Is(err, ErrHostResponse) {
		t.Fatalf("ReadStream: %v", err)
	}
	if err := host.Emit(ctx, "s", nil); err == nil || errors.Is(err, ErrHostResponse) {
		t.Fatalf("a refused callback is not a malformed response: %v", err)
	}
}

func TestResolveAuthDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := map[string]string{
		"":                          "",
		"  ":                        "",
		"~":                         home,
		DefaultAuthDir:              filepath.Join(home, ".cli-proxy-api"),
		`~\nested\dir`:              filepath.Join(home, "nested", "dir"),
		"/var/lib/cliproxy/../auth": "/var/lib/auth",
	}
	for in, want := range cases {
		if got := ResolveAuthDir(in); got != want {
			t.Errorf("ResolveAuthDir(%q) = %q, want %q", in, got, want)
		}
	}
}
