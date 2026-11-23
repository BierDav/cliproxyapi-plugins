package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type fusionTestHost struct {
	t             *testing.T
	requests      []framework.HTTPRequest
	selected      string
	autoCount     int
	mu            sync.Mutex
	streams       map[string][]byte
	closed        map[string]bool
	output        []byte
	done          chan string
	badBody       []byte
	expireSession bool
	sessionBody   []byte
}

func (h *fusionTestHost) Do(_ context.Context, _ string, req framework.HTTPRequest) (framework.HTTPResponse, error) {
	h.requests = append(h.requests, req)
	parsed, _ := url.Parse(req.URL)
	resp := framework.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{}}
	switch parsed.Path {
	case "/copilot_internal/v2/token":
		resp.Body = []byte(`{"token":"new-account-token","refresh_in":3600,"endpoints":{"api":"https://copilot.test"}}`)
	case "/auto":
		h.autoCount++
		if req.Headers.Get("Copilot-Integration-Id") != "copilot-developer-cli" || req.Headers.Get("X-GitHub-Api-Version") != "2026-08-01" {
			h.t.Fatal("Fusion request does not use the current Copilot contract")
		}
		if req.Headers.Get("Copilot-Session-Token") != "" {
			h.t.Fatal("a previous request's session token leaked into the router")
		}
		if gjson.GetBytes(req.Body, "prompt").String() != "Reply OK" {
			h.t.Fatalf("wrong routing prompt: %s", req.Body)
		}
		resp.Body = []byte(fmt.Sprintf(`{"session_token":"session-%d","selected_model":{"id":%q,"supported_endpoints":["/responses"]}}`, h.autoCount, h.selected))
		if h.sessionBody != nil {
			resp.Body = h.sessionBody
		}
	case "/responses":
		model := gjson.GetBytes(req.Body, "model").String()
		if h.expireSession && req.Headers.Get("Copilot-Session-Token") != "" {
			h.expireSession = false
			resp.StatusCode = http.StatusUnauthorized
			resp.Body = []byte(`{"error":{"message":"expired"}}`)
			return resp, nil
		}
		if model != "gpt-5.4-nano" && req.Headers.Get("Copilot-Session-Token") == "" {
			resp.StatusCode = http.StatusBadRequest
			resp.Body = []byte(`{"error":{"code":"model_not_supported","message":"The requested model is not supported."}}`)
			if h.badBody != nil {
				resp.Body = h.badBody
			}
			return resp, nil
		}
		if model != "gpt-5.4-nano" && (model != h.selected || req.Headers.Get("Copilot-Session-Token") != fmt.Sprintf("session-%d", h.autoCount)) {
			h.t.Fatalf("inference did not use its authorized model and fresh token: model=%s", model)
		}
		if (model == "gpt-5.3-codex" || model == "mai-code-2-flash") && gjson.GetBytes(req.Body, "reasoning.context").String() == "all_turns" {
			resp.StatusCode = http.StatusBadRequest
			resp.Body = []byte(`{"error":{"code":"invalid_request_body","message":"Unsupported value: 'all_turns' is not supported with the 'gpt-5.3-codex' model. Supported values are: 'auto' and 'current_turn'."}}`)
			return resp, nil
		}
		resp.Body = []byte(fmt.Sprintf(`{"id":"resp_test","object":"response","status":"completed","model":%q,"output":[{"id":"msg_test","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`, model))
	default:
		h.t.Fatalf("unexpected upstream path: %s", parsed.Path)
	}
	return resp, nil
}

func (h *fusionTestHost) OpenStream(ctx context.Context, callback string, req framework.HTTPRequest) (framework.HTTPStream, error) {
	resp, err := h.Do(ctx, callback, req)
	if err != nil {
		return framework.HTTPStream{}, err
	}
	id := fmt.Sprintf("stream-%d", len(h.requests))
	body := resp.Body
	if resp.StatusCode == http.StatusOK {
		// Copilot names every Responses event on an event line.
		body = []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\",\"output_index\":0,\"content_index\":0}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + string(body) + "}\n\n")
	}
	h.mu.Lock()
	h.streams[id] = body
	h.mu.Unlock()
	return framework.HTTPStream{StatusCode: resp.StatusCode, Headers: http.Header{}, ID: id}, nil
}

func (h *fusionTestHost) ReadStream(_ context.Context, id string) (framework.StreamChunk, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return framework.StreamChunk{Payload: h.streams[id], Done: true}, nil
}

func (h *fusionTestHost) CloseStream(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed[id] = true
	return nil
}

func (h *fusionTestHost) Emit(_ context.Context, _ string, payload []byte) error {
	h.output = append(h.output, payload...)
	return nil
}

func (h *fusionTestHost) CloseOutput(_ context.Context, _ string, message string) {
	h.done <- message
}

func newFusionTestService(t *testing.T) (*Service, *fusionTestHost, []byte) {
	t.Helper()
	host := &fusionTestHost{t: t, selected: "gpt-5.6-luna", streams: map[string][]byte{}, closed: map[string]bool{}, done: make(chan string, 1)}
	s := New(host)
	storage := []byte(`{"type":"copilot","github_access_token":"github-token","model_aliases":[{"name":"gpt-5.6-luna","alias":"gh/gpt-5.6-luna"},{"name":"gpt-6.1-sol","alias":"gh/gpt-6.1-sol"},{"name":"auto","alias":"gh/auto"}]}`)
	fingerprint := tokenFingerprint("github-token")
	s.tokenEntries["account"] = copilotTokenEntry{Token: "account-token", APIBaseURL: "https://copilot.test", Fingerprint: fingerprint, ExpiresAt: time.Now().Add(time.Hour)}
	models := []upstreamModel{}
	for _, id := range []string{"gpt-5.6-luna", "gpt-6.1-sol", "gpt-5.4-nano"} {
		models = append(models, upstreamModel{ID: id, SupportedEndpoints: []string{"/responses"}})
	}
	s.modelEntries["account"] = modelCacheEntry{Fingerprint: fingerprint, APIBaseURL: "https://copilot.test", ExpiresAt: time.Now().Add(time.Hour), Models: models}
	return s, host, storage
}

func TestFusionExecution(t *testing.T) {
	for _, format := range []string{"openai-response", "claude", "openai"} {
		for _, stream := range []bool{false, true} {
			for _, model := range []string{"gh/gpt-5.6-luna", "gh/auto", "gpt-5.4-nano", "gh/gpt-6.1-sol"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", format, stream, model), func(t *testing.T) {
					s, host, storage := newFusionTestService(t)
					if model == "gh/gpt-6.1-sol" {
						host.selected = "gpt-6.1-sol"
					} else if model == "gh/auto" {
						host.selected = "gpt-5.3-codex"
					}
					payload := []byte(`{"input":[{"role":"user","content":"Reply OK"}],"reasoning":{"context":"all_turns","effort":"high"}}`)
					switch format {
					case "claude":
						payload = []byte(`{"messages":[{"role":"user","content":"Reply OK"}],"max_tokens":64}`)
					case "openai":
						payload = []byte(`{"messages":[{"role":"user","content":"Reply OK"}],"max_tokens":64}`)
					}
					req := ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "account", Model: model, SourceFormat: format, Payload: payload, OriginalRequest: payload, StorageJSON: storage}, StreamID: "output"}
					for attempt := 0; attempt < 2; attempt++ {
						var result []byte
						if stream {
							_, err := s.ExecuteStream(context.Background(), req)
							if err != nil {
								t.Fatal(err)
							}
							select {
							case message := <-host.done:
								if message != "" {
									t.Fatal(message)
								}
							case <-time.After(time.Second):
								t.Fatal("stream did not complete")
							}
							result = host.output
						} else {
							resp, err := s.Execute(context.Background(), req)
							if err != nil {
								t.Fatal(err)
							}
							result = resp.Payload
						}
						if !strings.Contains(string(result), "OK") {
							t.Fatalf("no model output: %s", result)
						}
						if format == "openai-response" {
							reasoning := gjson.GetBytes(host.requests[len(host.requests)-1].Body, "reasoning")
							wantContext := "all_turns"
							if model == "gh/auto" {
								wantContext = "auto"
							}
							if reasoning.Get("context").String() != wantContext || reasoning.Get("effort").String() != "high" {
								t.Fatalf("unexpected upstream reasoning settings: %s", reasoning)
							}
						}
						if format == "openai" {
							// The host adds the SSE envelope of Chat Completions streams.
							if strings.Contains(string(result), "data:") || !strings.Contains(string(result), `"content":"OK"`) {
								t.Fatalf("unexpected Chat Completions output: %s", result)
							}
							host.output = nil
						}
						if model == "gpt-5.4-nano" && host.autoCount != 0 {
							t.Fatal("working direct model was routed through Fusion")
						}
						if model != "gpt-5.4-nano" && host.autoCount != attempt+1 {
							t.Fatal("Fusion session was incorrectly shared between requests")
						}
						if s.tokenEntries["account"].SessionToken != "" {
							t.Fatal("Fusion session leaked into account token cache")
						}
					}
					if stream {
						for id := range host.streams {
							if !host.closed[id] {
								t.Fatalf("upstream stream %s was leaked", id)
							}
						}
					}
				})
			}
		}
	}
}

func TestFusionRetriesRejectedReasoningContext(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			s, host, storage := newFusionTestService(t)
			host.selected = "mai-code-2-flash"
			req := ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				AuthID: "account", Model: "gh/auto", SourceFormat: "openai-response", StorageJSON: storage,
				Payload: []byte(`{"input":"Reply OK","reasoning":{"context":"all_turns","effort":"high"}}`),
			}, StreamID: "output"}
			if stream {
				if _, err := s.ExecuteStream(context.Background(), req); err != nil {
					t.Fatal(err)
				}
				select {
				case message := <-host.done:
					if message != "" || !strings.Contains(string(host.output), "OK") {
						t.Fatalf("stream failed: %s, output: %s", message, host.output)
					}
				case <-time.After(time.Second):
					t.Fatal("stream did not complete")
				}
				if !host.closed["stream-2"] {
					t.Fatal("rejected upstream stream was not closed")
				}
			} else if response, err := s.Execute(context.Background(), req); err != nil || !strings.Contains(string(response.Payload), "OK") {
				t.Fatalf("request failed: %v, output: %s", err, response.Payload)
			}
			if len(host.requests) != 3 || host.autoCount != 1 {
				t.Fatalf("want one routing call and two inference attempts, got %d routing calls and %d total requests", host.autoCount, len(host.requests))
			}
			for i, wantContext := range []string{"all_turns", "auto"} {
				req := host.requests[i+1]
				body := gjson.ParseBytes(req.Body)
				if body.Get("model").String() != host.selected || req.Headers.Get("Copilot-Session-Token") != "session-1" ||
					body.Get("reasoning.context").String() != wantContext || body.Get("reasoning.effort").String() != "high" {
					t.Fatalf("attempt %d changed the model, session, or unrelated reasoning settings: %s", i+1, req.Body)
				}
			}
		})
	}
}

func TestFusionDoesNotSubstituteForSol(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			s, host, storage := newFusionTestService(t)
			req := ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "account", Model: "gh/gpt-6.1-sol", SourceFormat: "openai-response", Payload: []byte(`{"input":"Reply OK"}`), StorageJSON: storage}, StreamID: "output"}
			var err error
			if stream {
				_, err = s.ExecuteStream(context.Background(), req)
			} else {
				_, err = s.Execute(context.Background(), req)
			}
			var status *StatusError
			if !errors.As(err, &status) || status.HTTPStatus != http.StatusForbidden || status.Code != "model_not_available_for_plan" {
				t.Fatalf("expected explicit plan error, got %v", err)
			}
			if len(host.requests) != 2 || !strings.Contains(err.Error(), "gpt-5.6-luna") || !strings.Contains(err.Error(), "gpt-6.1-sol") {
				t.Fatalf("unexpected retry or unhelpful error: %v", err)
			}
		})
	}
}

func TestFusionDoesNotRetryOtherBadRequests(t *testing.T) {
	s, host, storage := newFusionTestService(t)
	host.badBody = []byte(`{"error":{"code":"invalid_request","message":"bad input"}}`)
	_, err := s.Execute(context.Background(), ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "account", Model: "gh/gpt-5.6-luna", SourceFormat: "openai-response", Payload: []byte(`{"input":"Reply OK"}`), StorageJSON: storage}})
	if err == nil || host.autoCount != 0 {
		t.Fatalf("unrelated error was retried: %v", err)
	}
}

func TestFusionPromptIgnoresInstructionsAndToolOutputs(t *testing.T) {
	body := []byte(`{"instructions":"secret system text","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="},{"type":"input_text","text":"earlier"}]},{"type":"function_call_output","output":"tool result"},{"role":"user","content":[{"type":"input_text","text":"latest"}]}]}`)
	prompt, image := fusionPrompt(body)
	if prompt != "latest" || !image {
		t.Fatalf("prompt=%q image=%t", prompt, image)
	}
}

func TestFusionModelAliasIsDiscoverable(t *testing.T) {
	s, _, storage := newFusionTestService(t)
	response, err := s.ModelsForAuth(context.Background(), "", pluginapi.AuthModelRequest{AuthID: "account", StorageJSON: storage})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(response.Models)
	if !strings.Contains(string(data), "gh/auto") {
		t.Fatalf("Fusion alias was not published: %s", data)
	}
}

func TestFusionRefreshesAccountAndSessionTokensTogether(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			s, host, storage := newFusionTestService(t)
			host.expireSession = true
			req := ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "account", Model: "gh/auto", SourceFormat: "openai-response", Payload: []byte(`{"input":"Reply OK"}`), StorageJSON: storage}, StreamID: "output"}
			var err error
			if stream {
				_, err = s.ExecuteStream(context.Background(), req)
				if err == nil {
					select {
					case message := <-host.done:
						if message != "" {
							t.Fatal(message)
						}
					case <-time.After(time.Second):
						t.Fatal("stream did not complete")
					}
				}
			} else {
				_, err = s.Execute(context.Background(), req)
			}
			if err != nil || host.autoCount != 2 || s.tokenEntries["account"].Token != "new-account-token" {
				t.Fatalf("tokens were not refreshed together: %v, sessions=%d", err, host.autoCount)
			}
		})
	}
}

func TestFusionRejectsIncompleteSessionWithoutLeakingTokens(t *testing.T) {
	for _, body := range []string{`{"session_token":"secret-session"}`, `{"selected_model":{"id":"gpt-5.6-luna"}}`, `{`} {
		s, host, storage := newFusionTestService(t)
		host.sessionBody = []byte(body)
		_, err := s.Execute(context.Background(), ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "account", Model: "gh/auto", SourceFormat: "openai-response", Payload: []byte(`{"input":"Reply OK"}`), StorageJSON: storage}})
		if err == nil || strings.Contains(err.Error(), "secret-session") || len(host.requests) != 1 {
			t.Fatalf("invalid session was accepted or leaked: %v", err)
		}
	}
}
