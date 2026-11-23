package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

var registry = builtin.Registry()

// Make's execution time limit is 40 minutes; one agent step is capped at 10.
const runDeadline = 30 * time.Minute

var pollInterval = 1500 * time.Millisecond

// pingInterval keeps idle streaming clients from timing out while Make runs.
var pingInterval = 15 * time.Second

type execution struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id"`
	Callback string `json:"host_callback_id"`
}

type gatewayResult struct {
	Response  string
	Reasoning string
	Usage     struct {
		PromptTokens     int64 `json:"promptTokens"`
		CompletionTokens int64 `json:"completionTokens"`
		TotalTokens      int64 `json:"totalTokens"`
		CacheReadTokens  int64 `json:"cacheReadTokens"`
	}
}

type prepared struct {
	model      modelSpec
	source     translator.Format
	translated []byte
	system     string
	message    string
	reasoning  string
	tools      map[string]bool
}

func prepare(req execution) (prepared, error) {
	var p prepared
	model, ok := findModel(req.Model)
	if !ok {
		return p, newStatusError(404, "unknown Make model %q", req.Model)
	}
	p.model = model
	p.source = translator.FromString(req.SourceFormat)
	if p.source == "" {
		p.source = translator.FromString(req.Format)
	}
	if p.source == "" {
		p.source = translator.FormatOpenAI
	}
	stream := req.Stream
	body := req.Payload
	if p.source != translator.FormatOpenAI {
		if !registry.HasRequestTransformer(p.source, translator.FormatOpenAI) {
			return p, newStatusError(400, "unsupported client protocol %q", p.source)
		}
		body = registry.TranslateRequest(p.source, translator.FormatOpenAI, req.Model, body, stream)
	}
	p.translated = body
	var chat chatRequest
	if json.Unmarshal(body, &chat) != nil {
		return p, newStatusError(400, "Invalid chat request")
	}
	if len(chat.Messages) == 0 {
		return p, newStatusError(400, "The request has no messages")
	}
	p.system, p.message, p.tools = renderPrompt(chat)
	if strings.TrimSpace(p.message) == "" {
		return p, newStatusError(400, "The request has no conversation content")
	}
	p.reasoning = reasoningLevel(model, chat.ReasoningEffort)
	return p, nil
}

func execute(method string, raw []byte) (any, error) {
	var req execution
	if json.Unmarshal(raw, &req) != nil {
		return nil, fmt.Errorf("Invalid execution request")
	}
	req.Stream = method == pluginabi.MethodExecutorExecuteStream
	p, err := prepare(req)
	if err != nil {
		return nil, err
	}
	s, key, err := liveStorage(req.Callback, req.AuthID, req.StorageJSON, false)
	if err != nil {
		return nil, err
	}
	if err := pausedError(key); err != nil {
		return nil, err
	}
	if !req.Stream {
		result, err := complete(context.Background(), req.Callback, key, s, p)
		if err != nil {
			return nil, err
		}
		response := chatCompletion(req.Model, p, result)
		if p.source != translator.FormatOpenAI {
			response = registry.TranslateNonStream(context.Background(), translator.FormatOpenAI, p.source, req.Model, req.OriginalRequest, p.translated, response, nil)
		}
		if !json.Valid(response) {
			return nil, newStatusError(502, "Invalid Make model response")
		}
		return pluginapi.ExecutorResponse{Payload: response, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
	}
	if req.StreamID == "" {
		return nil, fmt.Errorf("Missing output stream")
	}
	go streamRun(req, key, s, p)
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}}, nil
}

// runGateway starts the gateway scenario and waits for its outputs. A deleted
// or deactivated gateway is re-provisioned once.
func runGateway(ctx context.Context, callback, key string, s storage, p prepared) (gatewayResult, error) {
	if s.ScenarioID == 0 {
		var err error
		if s, err = ensureGateway(callback, key, s); err != nil {
			return gatewayResult{}, err
		}
	}
	result, err := runOnce(ctx, callback, s, p)
	if err != nil && isMissingScenario(err) {
		if s, err = ensureGateway(callback, key, s); err != nil {
			return gatewayResult{}, err
		}
		result, err = runOnce(ctx, callback, s, p)
	}
	notePaused(key, err)
	return result, err
}

// pauseHold is how long requests for a credential whose Make organization is
// paused fail without running the gateway. A streamed request learns about
// the pause only after its response headers went out, so the host cannot see
// the 429 there; the next requests report it before streaming.
const pauseHold = 10 * time.Minute

var (
	pausedMu sync.Mutex
	paused   = map[string]pausedRun{}
)

type pausedRun struct {
	until time.Time
	err   error
}

func notePaused(key string, err error) {
	var failure *mcpError
	if !errors.As(err, &failure) || failure.status != http.StatusTooManyRequests {
		return
	}
	pausedMu.Lock()
	paused[key] = pausedRun{until: time.Now().Add(pauseHold), err: err}
	pausedMu.Unlock()
}

func pausedError(key string) error {
	pausedMu.Lock()
	defer pausedMu.Unlock()
	run, ok := paused[key]
	if !ok {
		return nil
	}
	if time.Now().After(run.until) {
		delete(paused, key)
		return nil
	}
	return run.err
}

// maxToolCallCorrections bounds the extra runs spent on replies whose
// tool_call blocks do not decode.
const maxToolCallCorrections = 2

// complete runs the gateway and, when the reply contains tool_call blocks
// that do not decode, runs it again with the problems appended to the
// transcript so the model rewrites its message. Delivering such a reply would
// end the client's agent turn with a tool call that never runs. The appended
// turns extend the transcript, so the retries reuse the cached prefix.
func complete(ctx context.Context, callback, key string, s storage, p prepared) (gatewayResult, error) {
	result, err := runGateway(ctx, callback, key, s, p)
	if err != nil {
		return result, err
	}
	usage := result.Usage
	for attempt := 0; attempt < maxToolCallCorrections; attempt++ {
		problems := parseReply(result.Response, p.tools).Problems
		if len(problems) == 0 {
			break
		}
		p.message = correctionMessage(p, result.Response, problems)
		next, err := runGateway(ctx, callback, key, s, p)
		if err != nil {
			// Keep the reply already produced; buildResult marks its problems.
			break
		}
		result = next
		usage.PromptTokens += next.Usage.PromptTokens
		usage.CompletionTokens += next.Usage.CompletionTokens
		usage.TotalTokens += next.Usage.TotalTokens
		usage.CacheReadTokens += next.Usage.CacheReadTokens
	}
	result.Usage = usage
	return result, nil
}

// correctionMessage appends the rejected reply and the reasons to the
// transcript, before the fixed trailer.
func correctionMessage(p prepared, reply string, problems []string) string {
	trailer := turnReminder
	if len(p.tools) > 0 {
		trailer = turnReminderTools
	}
	var out strings.Builder
	out.WriteString(strings.TrimSuffix(p.message, trailer))
	fmt.Fprintf(&out, "<assistant>\n%s\n</assistant>\n<system>\n", strings.TrimSpace(reply))
	out.WriteString("Your previous message was not delivered: the client could not decode these tool_call blocks.\n")
	for _, problem := range problems {
		fmt.Fprintf(&out, "- %s\n", problem)
	}
	out.WriteString(toolCallCorrection)
	out.WriteString("\n</system>\n")
	out.WriteString(trailer)
	return out.String()
}

const toolCallCorrection = `Write that message again in full, with every tool_call block corrected. TOOL_NAME must be a tool name exactly as listed under "Client tools". The body must be one valid JSON object: inside JSON strings, escape every backslash as \\ and every double quote as \", and write line breaks as \n. This applies on top of any escaping the language inside the string needs.`

type runStatus struct {
	ExecutionID string `json:"executionId"`
	Status      string `json:"status"`
	Outputs     *struct {
		Response  *string `json:"response"`
		Reasoning *string `json:"reasoning"`
		Usage     *string `json:"usage"`
	} `json:"outputs"`
	Error     json.RawMessage `json:"error"`
	Execution *struct {
		Status string `json:"status"`
	} `json:"execution"`
}

func (r runStatus) state() string {
	if r.Status != "" {
		return r.Status
	}
	if r.Execution != nil {
		return r.Execution.Status
	}
	return ""
}

func runOnce(ctx context.Context, callback string, s storage, p prepared) (gatewayResult, error) {
	inputs := map[string]any{"model": p.model.Native, "reasoning": p.reasoning, "system": p.system, "message": p.message}
	raw, err := callTool(callback, s.AccessToken, "scenario_run", map[string]any{"scenarioId": s.ScenarioID, "inputs": inputs})
	if err != nil {
		return gatewayResult{}, err
	}
	var status runStatus
	if json.Unmarshal(raw, &status) != nil {
		return gatewayResult{}, newStatusError(502, "Invalid Make run response")
	}
	// scenario_run waits briefly; longer runs are polled by execution ID.
	deadline := time.Now().Add(runDeadline)
	for !finished(status.state()) {
		if status.ExecutionID == "" {
			return gatewayResult{}, newStatusError(502, "Make did not return an execution ID")
		}
		if time.Now().After(deadline) {
			return gatewayResult{}, newStatusError(504, "Make run exceeded %s", runDeadline)
		}
		select {
		case <-ctx.Done():
			return gatewayResult{}, ctx.Err()
		case <-time.After(pollInterval):
		}
		executionID := status.ExecutionID
		token := s.AccessToken
		if time.UnixMilli(s.AccessExpires).Before(time.Now().Add(accessSkew)) {
			// Long runs outlive the five-minute access token.
			if fresh, _, err := liveStorage(callback, cacheKey("", s), encode(s), false); err == nil {
				s, token = fresh, fresh.AccessToken
			}
		}
		raw, err = callTool(callback, token, "scenario_execution_get", map[string]any{"scenarioId": s.ScenarioID, "executionId": executionID})
		if err != nil {
			return gatewayResult{}, err
		}
		status = runStatus{}
		if json.Unmarshal(raw, &status) != nil {
			return gatewayResult{}, newStatusError(502, "Invalid Make execution response")
		}
		status.ExecutionID = executionID
	}
	if status.state() != "success" || status.Outputs == nil || status.Outputs.Response == nil {
		return gatewayResult{}, runFailure(status)
	}
	result := gatewayResult{Response: *status.Outputs.Response}
	if status.Outputs.Reasoning != nil {
		result.Reasoning = *status.Outputs.Reasoning
	}
	if status.Outputs.Usage != nil {
		_ = json.Unmarshal([]byte(*status.Outputs.Usage), &result.Usage)
	}
	return result, nil
}

func finished(state string) bool {
	switch state {
	case "success", "error", "warning", "failed", "cancelled", "canceled", "incomplete":
		return true
	}
	return false
}

// runFailure maps Make's error text to an HTTP status. Make reports quota
// and rate problems in prose, so this matches on well-known phrases.
func runFailure(status runStatus) error {
	message := ""
	var text string
	var detail struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(status.Error, &text) == nil {
		message = text
	} else if json.Unmarshal(status.Error, &detail) == nil {
		message = detail.Message
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "run ended with status " + status.state()
	}
	lower := strings.ToLower(message)
	code := 502
	switch {
	case strings.Contains(lower, "steps per agent call"):
		// The model kept calling Make's built-in agent tools instead of
		// answering. That is a property of this request and model, not of the
		// credential, so it must not put the account into cooldown.
		return newStatusError(422, "Make run failed: the model called Make's built-in agent tools instead of answering. Retry, or use a stronger model such as make/gpt-6.2-sol")
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") || strings.Contains(lower, "429"):
		code = 429
	case strings.Contains(lower, "credit") || strings.Contains(lower, "operations limit") || strings.Contains(lower, "quota"):
		code = 429
	case strings.Contains(lower, "context") && strings.Contains(lower, "length"):
		code = 400
	case strings.Contains(lower, "invalidconfiguration") || strings.Contains(lower, "validation error"):
		code = 400
	case strings.HasPrefix(lower, "400 |") || strings.HasPrefix(lower, "422 |"):
		// Make prefixes module errors with their HTTP status; data errors
		// describe this run's input.
		code = 400
	}
	return newStatusError(code, "Make run failed: %s", truncate(message, 500))
}

func callID() string {
	return "call_" + hex.EncodeToString(randomBytes(12))
}

type chatResult struct {
	Content   string
	Reasoning string
	Calls     []parsedCall
	IDs       []string
	Finish    string
	Usage     map[string]any
}

func buildResult(p prepared, result gatewayResult) chatResult {
	reply := parseReply(result.Response, p.tools)
	content, calls := reply.Content, reply.Calls
	if len(reply.Problems) > 0 {
		// Corrections are exhausted. Say why no tool ran rather than ending
		// the turn on what looks like an ordinary message.
		content += "\n\n[Make provider: these tool calls were not run because they could not be decoded: " + strings.Join(reply.Problems, "; ") + "]"
	}
	out := chatResult{Content: content, Calls: calls, Finish: "stop"}
	if p.source != translator.FormatClaude || currentConfig().ClaudeThinking {
		out.Reasoning = strings.TrimSpace(result.Reasoning)
	}
	for range calls {
		out.IDs = append(out.IDs, callID())
	}
	if len(calls) > 0 {
		out.Finish = "tool_calls"
	}
	out.Usage = map[string]any{
		"prompt_tokens":         result.Usage.PromptTokens,
		"completion_tokens":     result.Usage.CompletionTokens,
		"total_tokens":          result.Usage.PromptTokens + result.Usage.CompletionTokens,
		"prompt_tokens_details": map[string]any{"cached_tokens": result.Usage.CacheReadTokens},
	}
	return out
}

func chatCompletion(model string, p prepared, result gatewayResult) []byte {
	r := buildResult(p, result)
	message := map[string]any{"role": "assistant", "content": r.Content}
	if r.Content == "" && len(r.Calls) > 0 {
		message["content"] = nil
	}
	if r.Reasoning != "" {
		message["reasoning_content"] = r.Reasoning
	}
	if len(r.Calls) > 0 {
		var calls []map[string]any
		for i, call := range r.Calls {
			calls = append(calls, map[string]any{"id": r.IDs[i], "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
		}
		message["tool_calls"] = calls
	}
	return encode(map[string]any{
		"id": "chatcmpl-" + hex.EncodeToString(randomBytes(12)), "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": r.Finish}},
		"usage":   r.Usage,
	})
}

// streamChunks renders a finished result as Chat Completions stream chunks.
func streamChunks(id, model string, created int64, r chatResult) [][]byte {
	chunk := func(delta map[string]any, finish any) []byte {
		return encode(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}})
	}
	var out [][]byte
	if r.Reasoning != "" {
		out = append(out, chunk(map[string]any{"reasoning_content": r.Reasoning}, nil))
	}
	if r.Content != "" {
		out = append(out, chunk(map[string]any{"content": r.Content}, nil))
	}
	for i, call := range r.Calls {
		out = append(out, chunk(map[string]any{"tool_calls": []map[string]any{{"index": i, "id": r.IDs[i], "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Arguments}}}}, nil))
	}
	out = append(out, encode(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": r.Finish}}, "usage": r.Usage}))
	return out
}

func streamRun(req execution, key string, s storage, p prepared) {
	failure := ""
	defer func() {
		host().CloseOutput(context.Background(), req.StreamID, failure)
	}()
	id := "chatcmpl-" + hex.EncodeToString(randomBytes(12))
	created := time.Now().Unix()
	var state any
	emit := func(payload []byte) bool {
		frame := append([]byte("data: "), payload...)
		outputs := [][]byte{payload}
		if p.source != translator.FormatOpenAI {
			outputs = registry.TranslateStream(context.Background(), translator.FormatOpenAI, p.source, req.Model, req.OriginalRequest, p.translated, frame, &state)
		}
		for _, out := range outputs {
			if len(out) == 0 {
				continue
			}
			if err := host().Emit(context.Background(), req.StreamID, out); err != nil {
				return false
			}
		}
		return true
	}
	// Open the response (message_start / response.created) before waiting.
	if !emit(encode(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}})) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	pinger := make(chan struct{})
	if p.source != translator.FormatClaude {
		close(pinger)
	} else {
		go func() {
			defer close(pinger)
			ticker := time.NewTicker(pingInterval)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					if err := host().Emit(context.Background(), req.StreamID, []byte("event: ping\ndata: {\"type\": \"ping\"}\n\n")); err != nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	result, err := complete(ctx, req.Callback, key, s, p)
	close(done)
	<-pinger
	if err != nil {
		failure = err.Error()
		return
	}
	for _, chunk := range streamChunks(id, req.Model, created, buildResult(p, result)) {
		if !emit(chunk) {
			return
		}
	}
	if p.source != translator.FormatOpenAI {
		_ = emit([]byte("[DONE]"))
	}
}
