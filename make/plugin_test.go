package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeMake simulates the host transport plus Make's OAuth and MCP endpoints.
type fakeMake struct {
	mu        sync.Mutex
	tools     map[string]func(args map[string]any) (any, error)
	toolCalls []string
	runInputs []map[string]any
	saved     map[string]storage
	savedRaw  map[string]json.RawMessage
	emitted   [][]byte
	closed    chan string
	refreshes int
	refresh   string
	tokenErr  string
	// tokenReply, when set, is Make's raw answer to token requests.
	tokenReply   *upstreamResponse
	tokenHeaders []map[string][]string
	// email is the address users/me reports; empty answers 403.
	email   string
	meCalls int
	logs    []string
	// userinfoEmail is the address OpenID userinfo reports; empty answers 401.
	userinfoEmail string
	// idEmail, when set, is the address in the sign-in's ID token.
	idEmail string
}

func newFakeMake(t *testing.T) *fakeMake {
	f := &fakeMake{tools: map[string]func(map[string]any) (any, error){}, saved: map[string]storage{}, savedRaw: map[string]json.RawMessage{}, closed: make(chan string, 4), refresh: "refresh-0", email: "ada@example.com"}
	previous := hostCall
	hostCall = f.call
	t.Cleanup(func() {
		hostCall = previous
		shutdown()
	})
	shutdown()
	return f
}

func testToken(expires time.Time) string {
	claims := encode(map[string]any{"sub": "4242", "organization_id": 77, "zone_domain": "https://eu1.make.com", "exp": expires.Unix()})
	return "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
}

func (f *fakeMake) call(method string, payload any) (json.RawMessage, error) {
	raw := encode(payload)
	switch method {
	case pluginabi.MethodHostAuthSave:
		var req struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		_ = json.Unmarshal(raw, &req)
		var s storage
		_ = json.Unmarshal(req.JSON, &s)
		f.mu.Lock()
		f.saved[req.Name] = s
		f.savedRaw[req.Name] = req.JSON
		f.mu.Unlock()
		return encode(map[string]any{}), nil
	case pluginabi.MethodHostLog:
		f.mu.Lock()
		f.logs = append(f.logs, string(raw))
		f.mu.Unlock()
		return encode(map[string]any{}), nil
	case pluginabi.MethodHostStreamEmit:
		var req struct {
			Payload []byte `json:"payload"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.emitted = append(f.emitted, req.Payload)
		f.mu.Unlock()
		return encode(map[string]any{}), nil
	case pluginabi.MethodHostStreamClose:
		var req struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &req)
		f.closed <- req.Error
		return encode(map[string]any{}), nil
	case pluginabi.MethodHostHTTPDo:
		var req struct {
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		_ = json.Unmarshal(raw, &req)
		status, body := f.http(req.URL, req.Headers, req.Body)
		return encode(upstreamResponse{StatusCode: status, Body: body}), nil
	}
	return nil, fmt.Errorf("unexpected host method %s", method)
}

func (f *fakeMake) http(address string, headers map[string][]string, body []byte) (int, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ua := headers["User-Agent"]; len(ua) == 0 || !strings.HasPrefix(ua[0], "cliproxyapi-make/") {
		return 403, []byte(`{"message":"blocked"}`)
	}
	switch address {
	case "https://eu1.make.com/api/v2/users/me":
		f.meCalls++
		auth := headers["Authorization"]
		if len(auth) == 0 || !strings.HasPrefix(auth[0], "Bearer h.") || f.email == "" {
			return 403, []byte(`{"message":"Forbidden"}`)
		}
		return 200, encode(map[string]any{"authUser": map[string]any{"id": 4242, "name": "Ada", "email": f.email}})
	case userinfoURL:
		if f.userinfoEmail == "" {
			return 401, []byte(`{"error":"invalid_token"}`)
		}
		return 200, encode(map[string]any{"sub": "4242", "email": f.userinfoEmail})
	case registerURL:
		return 201, []byte(`{"client_id":"client-1","client_secret":"secret-1"}`)
	case tokenURL:
		form, _ := url.ParseQuery(string(body))
		f.tokenHeaders = append(f.tokenHeaders, headers)
		if f.tokenReply != nil {
			return f.tokenReply.StatusCode, f.tokenReply.Body
		}
		if f.tokenErr != "" {
			return 400, []byte(`{"error":"` + f.tokenErr + `"}`)
		}
		switch form.Get("grant_type") {
		case "authorization_code":
			if form.Get("code") != "code-1" || form.Get("code_verifier") == "" || form.Get("client_secret") != "secret-1" {
				return 400, []byte(`{"error":"invalid_grant"}`)
			}
		case "refresh_token":
			if form.Get("refresh_token") != f.refresh {
				return 400, []byte(`{"error":"invalid_grant"}`)
			}
		}
		f.refreshes++
		f.refresh = fmt.Sprintf("refresh-%d", f.refreshes)
		reply := map[string]any{"access_token": testToken(time.Now().Add(5 * time.Minute)), "refresh_token": f.refresh, "expires_in": 300}
		if f.idEmail != "" && form.Get("grant_type") == "authorization_code" {
			reply["id_token"] = "h." + base64.RawURLEncoding.EncodeToString(encode(map[string]any{"sub": "4242", "email": f.idEmail})) + ".s"
		}
		return 200, encode(reply)
	case mcpURL:
		var rpc struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &rpc)
		if rpc.Method == "tools/list" {
			var tools []map[string]any
			for name := range f.tools {
				tools = append(tools, map[string]any{"name": name})
			}
			return 200, []byte("event: message\ndata: " + string(encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": tools}})) + "\n\n")
		}
		f.toolCalls = append(f.toolCalls, rpc.Params.Name)
		if rpc.Params.Name == "scenario_run" {
			inputs, _ := rpc.Params.Arguments["inputs"].(map[string]any)
			f.runInputs = append(f.runInputs, inputs)
		}
		handler := f.tools[rpc.Params.Name]
		if handler == nil {
			return 200, []byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"unexpected tool"}]}}`)
		}
		f.mu.Unlock()
		result, err := handler(rpc.Params.Arguments)
		f.mu.Lock()
		if err != nil {
			return 200, encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}}})
		}
		// Make answers as a single SSE event.
		return 200, []byte("event: message\ndata: " + string(encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"structuredContent": result, "content": []any{}}})) + "\n\n")
	}
	return 404, []byte(`{}`)
}

func (f *fakeMake) provisioning() {
	f.tools["environment_get"] = func(map[string]any) (any, error) {
		return map[string]any{"zone": "eu1.make.com", "organizations": []any{map[string]any{"id": 77, "name": "Acme", "teams": []any{map[string]any{"id": 5, "name": "Space"}}}}}, nil
	}
	f.tools["module_spec"] = func(map[string]any) (any, error) {
		return map[string]any{"modules": []any{map[string]any{"connection": map[string]any{"existing": []any{
			map[string]any{"id": 9, "name": "My OpenAI"}, map[string]any{"id": 13, "name": "Make's AI Provider (default)"}}}}}}, nil
	}
	f.tools["scenario_create"] = func(args map[string]any) (any, error) {
		if args["teamId"].(float64) != 5 {
			return nil, fmt.Errorf("wrong team")
		}
		return map[string]any{"scenarioId": 1001, "isActive": true, "errors": []any{}}, nil
	}
	f.tools["scenario_delete"] = func(map[string]any) (any, error) { return map[string]any{"text": "deleted"}, nil }
}

func (f *fakeMake) reply(text string) {
	f.tools["scenario_run"] = func(map[string]any) (any, error) {
		return map[string]any{"executionId": "e1", "status": "success", "outputs": map[string]any{"response": text, "reasoning": "Thought briefly.",
			"usage": `{"promptTokens":9000,"completionTokens":40,"totalTokens":9040,"cacheReadTokens":8800}`}}, nil
	}
}

func credential() storage {
	return storage{Type: providerKey, ClientID: "client-1", ClientSecret: "secret-1", RefreshToken: "refresh-0", OrganizationID: 77,
		OrganizationName: "Acme", UserID: "4242", TeamID: 5, ConnectionID: 13, ScenarioID: 1001, GatewayVersion: gatewayVersion, RotatedAt: 1}
}

const claudeToolRequest = `{"model":"make/gpt-6.2-sol","max_tokens":4096,"stream":%s,"thinking":{"type":"enabled","budget_tokens":16000},
"system":[{"type":"text","text":"You are a coding agent."}],
"tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],
"messages":[{"role":"user","content":[{"type":"text","text":"Show me a.txt"}]}]}`

func executorPayload(stream bool, source string, payload string) []byte {
	return encode(map[string]any{"AuthID": "make-77-4242.json", "Model": "make/gpt-6.2-sol", "SourceFormat": source, "Format": source,
		"Payload": []byte(payload), "OriginalRequest": []byte(payload), "StorageJSON": encode(credential()), "stream_id": "s1", "host_callback_id": "cb"})
}

func TestRenderPromptKeepsCacheablePrefix(t *testing.T) {
	first := chatRequest{Messages: []chatMessage{
		{Role: "system", Content: json.RawMessage(`"Be helpful."`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"Read a.txt"}]`)},
	}, Tools: []chatTool{{Type: "function"}}}
	first.Tools[0].Function.Name = "Read"
	first.Tools[0].Function.Parameters = json.RawMessage(`{"type":"object","required":["file_path"],"properties":{"file_path":{"type":"string"}}}`)
	second := first
	second.Messages = append(append([]chatMessage(nil), first.Messages...),
		chatMessage{Role: "assistant", Content: json.RawMessage(`"Reading."`), ToolCalls: []chatToolCall{{ID: "call_1"}}},
		chatMessage{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"hello"`)})
	second.Messages[2].ToolCalls[0].Function.Name = "Read"
	second.Messages[2].ToolCalls[0].Function.Arguments = `{ "file_path": "a.txt" }`

	system1, message1, tools := renderPrompt(first)
	system2, message2, _ := renderPrompt(second)
	if system1 != system2 {
		t.Fatal("system prompt changed between turns")
	}
	if !strings.HasSuffix(message1, turnReminderTools) || !strings.HasPrefix(message2, strings.TrimSuffix(message1, turnReminderTools)) {
		t.Fatalf("transcript is not append-only:\n%s\n---\n%s", message1, message2)
	}
	if !tools["Read"] || !strings.Contains(system1, "Be helpful.") || !strings.Contains(system1, `"required":["file_path"]`) {
		t.Fatalf("system prompt misses tools or instructions:\n%s", system1)
	}
	for _, want := range []string{`<tool_call id="call_1" name="Read">{"file_path":"a.txt"}</tool_call>`, `<tool_result id="call_1" name="Read">` + "\nhello\n</tool_result>"} {
		if !strings.Contains(message2, want) {
			t.Fatalf("transcript misses %q:\n%s", want, message2)
		}
	}
	again, _, _ := renderPrompt(second)
	if again != system2 {
		t.Fatal("rendering is not deterministic")
	}
}

func TestParseReply(t *testing.T) {
	tools := map[string]bool{"Read": true, "Bash": true}
	reply := parseReply("Let me look.\n<tool_call name=\"Read\">\n{\"file_path\": \"a.txt\"}\n</tool_call>\n<tool_call name=\"Bash\">```json\n{\"command\":\"ls\"}\n```</tool_call>", tools)
	if reply.Content != "Let me look." || len(reply.Calls) != 2 || reply.Calls[0].Arguments != `{"file_path":"a.txt"}` || reply.Calls[1].Name != "Bash" || len(reply.Problems) != 0 {
		t.Fatalf("%+v", reply)
	}
	reply = parseReply(`<tool_call name="Unknown">{}</tool_call> done`, tools)
	if len(reply.Calls) != 0 || !strings.Contains(reply.Content, "Unknown") || len(reply.Problems) != 1 || !strings.Contains(reply.Problems[0], "not a client tool") {
		t.Fatalf("unknown tool must stay text and be reported: %+v", reply)
	}
	reply = parseReply("<tool_call name=\"Read\">{\"file_path\":\"b\"}", tools)
	if len(reply.Calls) != 1 {
		t.Fatalf("unterminated final block must parse: %+v", reply)
	}
	reply = parseReply(`<tool_call name="Read">not json</tool_call>`, tools)
	if len(reply.Calls) != 0 || reply.Content == "" || len(reply.Problems) != 1 || !strings.Contains(reply.Problems[0], "not valid JSON") {
		t.Fatalf("invalid arguments must stay text and be reported: %+v", reply)
	}
	// OpenAI models address tools through their "functions" namespace.
	codex := map[string]bool{"exec": true, "mcp__t3__read": true}
	for _, name := range []string{"functions__exec", "functions.exec", " exec "} {
		reply = parseReply("Running it.\n<tool_call name=\""+name+"\">\n{\"input\":\"text(await tools.exec_command({cmd:\\\"ls\\\"}))\"}\n</tool_call>", codex)
		if reply.Content != "Running it." || len(reply.Calls) != 1 || reply.Calls[0].Name != "exec" {
			t.Fatalf("%s: %+v", name, reply)
		}
	}
	reply = parseReply(`<tool_call name="functions.mcp__t3__read">{}</tool_call>`, codex)
	if len(reply.Calls) != 1 || reply.Calls[0].Name != "mcp__t3__read" {
		t.Fatalf("namespaced tool: %+v", reply)
	}
	// Prose about the format, quoted as code, is neither a call nor a problem.
	reply = parseReply("Blocks look like `<tool_call name=\"…\">`, for example:\n```\n<tool_call name=\"X\">{}</tool_call>\n```", codex)
	if len(reply.Calls) != 0 || len(reply.Problems) != 0 {
		t.Fatalf("quoted format must stay prose: %+v", reply)
	}
}

// A model writing a JavaScript template literal inside a JSON string escaped
// "${" for JavaScript (\$) and not for JSON. The block reached Codex as text
// and the agent stopped.
func TestParseReplyRepairsHandEscapedArguments(t *testing.T) {
	codex := map[string]bool{"exec": true}
	text := strings.ReplaceAll(`<tool_call name="functions__exec">
{"input":"text(await tools.apply_patch(‵*** Begin Patch\n*** Add File: ci.yaml\n+        run: |\n+          docker build -t \"\${IMAGE}:\${{ github.sha }}\" .\n+          test \"$(docker image inspect --format '{{.Architecture}}' \"$image\")\" = amd64\n*** End Patch‵)); text(await tools.exec_command({cmd:\"actionlint ci.yaml\",max_output_tokens:2500}));"}
</tool_call>`, "‵", "`")
	reply := parseReply(text, codex)
	if len(reply.Calls) != 1 || len(reply.Problems) != 0 || reply.Content != "" {
		t.Fatalf("%+v", reply)
	}
	var arguments struct{ Input string }
	if err := json.Unmarshal([]byte(reply.Calls[0].Arguments), &arguments); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(arguments.Input, `-t "\${IMAGE}:\${{ github.sha }}" .`) || !strings.Contains(arguments.Input, `'{{.Architecture}}' "$image"`) {
		t.Fatalf("backslashes meant for JavaScript must survive: %s", arguments.Input)
	}

	tools := map[string]bool{"Write": true, "Bash": true}
	cases := []struct{ body, want string }{
		{"{\"content\": \"line 1\nline 2\"}", `{"content":"line 1\nline 2"}`},
		{`{"command": "ls", "timeout": 5,}`, `{"command":"ls","timeout":5}`},
		{`{"command": "echo \'hi\'"}`, `{"command":"echo \\'hi\\'"}`},
		{`{"id": 12345678901234567890, "u": "é"}`, `{"id":12345678901234567890,"u":"é"}`},
	}
	for _, c := range cases {
		reply = parseReply(`<tool_call name="Bash">`+c.body+`</tool_call>`, tools)
		if len(reply.Calls) != 1 || reply.Calls[0].Arguments != c.want {
			t.Errorf("%s: %+v", c.body, reply)
		}
	}
	// Arguments may contain the closing tag, for example when editing this protocol.
	reply = parseReply(`<tool_call name="Write">{"content": "<tool_call name=\"x\">{}</tool_call>"}</tool_call> after`, tools)
	if len(reply.Calls) != 1 || reply.Content != "after" || !strings.Contains(reply.Calls[0].Arguments, `</tool_call>`) {
		t.Fatalf("closing tag inside arguments: %+v", reply)
	}
	// The name-in-body format many models are trained on.
	for _, body := range []string{`{"name": "functions.Bash", "arguments": {"command": "ls"}}`, `{"name": "Bash", "arguments": "{\"command\": \"ls\"}"}`} {
		reply = parseReply("<tool_call>\n"+body+"\n</tool_call>", tools)
		if len(reply.Calls) != 1 || reply.Calls[0].Name != "Bash" || reply.Calls[0].Arguments != `{"command":"ls"}` {
			t.Errorf("%s: %+v", body, reply)
		}
	}
	// Where an unescaped quote ends the string is ambiguous: report it.
	reply = parseReply(`<tool_call name="Bash">{"command": "echo "hi""}</tool_call>`, tools)
	if len(reply.Calls) != 0 || len(reply.Problems) != 1 || !strings.Contains(reply.Problems[0], `near`) {
		t.Fatalf("ambiguous JSON must be reported: %+v", reply)
	}
}

func TestToolCallCorrection(t *testing.T) {
	f := newFakeMake(t)
	replies := []string{
		"Checking.\n<tool_call name=\"Read\">{\"file_path\": \"a.txt\"</tool_call>",
		"Checking.\n<tool_call name=\"Read\">{\"file_path\": \"a.txt\"}</tool_call>",
	}
	f.tools["scenario_run"] = func(map[string]any) (any, error) {
		text := replies[min(len(f.runInputs)-1, len(replies)-1)]
		return map[string]any{"executionId": "e1", "status": "success", "outputs": map[string]any{"response": text,
			"usage": `{"promptTokens":100,"completionTokens":10,"totalTokens":110,"cacheReadTokens":90}`}}, nil
	}
	raw, failed := handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "claude", fmt.Sprintf(claudeToolRequest, "false")))
	payload := payloadOf(t, raw)
	if failed || !strings.Contains(payload, `"type":"tool_use"`) || strings.Contains(payload, "Make provider") || len(f.runInputs) != 2 {
		t.Fatalf("runs=%d: %s", len(f.runInputs), payload)
	}
	if !strings.Contains(payload, `"usage":{"input_tokens":20,"output_tokens":20,"cache_read_input_tokens":180}`) {
		t.Fatalf("usage of both runs must be reported: %s", payload)
	}
	first, second := f.runInputs[0]["message"].(string), f.runInputs[1]["message"].(string)
	prefix := strings.TrimSuffix(first, turnReminderTools)
	if f.runInputs[0]["system"] != f.runInputs[1]["system"] || !strings.HasPrefix(second, prefix) || !strings.HasSuffix(second, turnReminderTools) ||
		!strings.Contains(second[len(prefix):], "<assistant>\nChecking.\n<tool_call name=\"Read\">") || !strings.Contains(second, "tool_call block 1 (name \"Read\")") {
		t.Fatalf("correction must extend the cached transcript:\n%s", second)
	}

	// When corrections run out, the reply says why no tool ran.
	replies = replies[:1]
	f.runInputs = nil
	raw, failed = handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "claude", fmt.Sprintf(claudeToolRequest, "false")))
	payload = payloadOf(t, raw)
	if failed || len(f.runInputs) != 1+maxToolCallCorrections || !strings.Contains(payload, "these tool calls were not run") || strings.Contains(payload, `"type":"tool_use"`) {
		t.Fatalf("runs=%d: %s", len(f.runInputs), payload)
	}
}

func TestReasoningLevel(t *testing.T) {
	sol, _ := findModel("make/gpt-6.2-sol")
	luna, _ := findModel("gpt-6-luna")
	cases := []struct {
		model     modelSpec
		requested string
		want      string
	}{{sol, "", "medium"}, {sol, "high", "high"}, {sol, "none", "low"}, {sol, "minimal", "low"}, {sol, "max", "max"},
		{luna, "max", "high"}, {luna, "none", "none"}, {luna, "bogus", "low"}}
	for _, c := range cases {
		if got := reasoningLevel(c.model, c.requested); got != c.want {
			t.Errorf("%s %q = %q, want %q", c.model.Native, c.requested, got, c.want)
		}
	}
}

func TestExecuteClaudeToolCall(t *testing.T) {
	f := newFakeMake(t)
	f.reply("I'll read it.\n<tool_call name=\"Read\">{\"file_path\":\"a.txt\"}</tool_call>")
	raw, failed := handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "claude", fmt.Sprintf(claudeToolRequest, "false")))
	if failed {
		t.Fatalf("execute failed: %s", raw)
	}
	var envelope pluginabi.Envelope
	_ = json.Unmarshal(raw, &envelope)
	var response pluginapi.ExecutorResponse
	_ = json.Unmarshal(envelope.Result, &response)
	var message struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(response.Payload, &message); err != nil {
		t.Fatalf("invalid Claude response %s", response.Payload)
	}
	var sawText, sawTool bool
	for _, block := range message.Content {
		sawText = sawText || (block.Type == "text" && block.Text == "I'll read it.")
		sawTool = sawTool || (block.Type == "tool_use" && block.Name == "Read" && string(block.Input) == `{"file_path":"a.txt"}`)
	}
	if !sawText || !sawTool || message.StopReason != "tool_use" {
		t.Fatalf("unexpected Claude response %s", response.Payload)
	}
	if message.Usage["cache_read_input_tokens"] == nil {
		t.Fatalf("cache usage missing: %s", response.Payload)
	}
	inputs := f.runInputs[0]
	if inputs["model"] != "gpt-6.2-sol" || inputs["reasoning"] != "high" || !strings.Contains(inputs["system"].(string), "## Read") ||
		!strings.Contains(inputs["message"].(string), "<user>\nShow me a.txt\n</user>") {
		t.Fatalf("unexpected gateway inputs %+v", inputs)
	}
	// The refresh rotated the refresh token, which must be persisted.
	if saved := f.saved["make-77-4242.json"]; saved.RefreshToken != "refresh-1" || saved.AccessToken != "" {
		t.Fatalf("rotated credential not persisted safely: %+v", saved)
	}
}

func TestExecuteStreamsClaude(t *testing.T) {
	f := newFakeMake(t)
	f.reply("Hello there.")
	raw, failed := handleMethod(pluginabi.MethodExecutorExecuteStream, executorPayload(true, "claude", fmt.Sprintf(claudeToolRequest, "true")))
	if failed {
		t.Fatalf("stream failed: %s", raw)
	}
	if failure := <-f.closed; failure != "" {
		t.Fatalf("stream closed with %q", failure)
	}
	f.mu.Lock()
	joined := string(joinFrames(f.emitted))
	f.mu.Unlock()
	for _, want := range []string{"message_start", "Hello there.", "message_delta", "message_stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stream misses %q:\n%s", want, joined)
		}
	}
	// Unsigned thinking would make the thread unusable on a real Claude model.
	if strings.Contains(joined, "thinking") {
		t.Fatalf("Claude stream must not carry unsigned thinking by default:\n%s", joined)
	}
}

func TestClaudeThinkingOptIn(t *testing.T) {
	f := newFakeMake(t)
	f.reply("Hello there.")
	configMu.Lock()
	config = pluginConfig{ClaudeThinking: true}
	configMu.Unlock()
	t.Cleanup(func() { configMu.Lock(); config = pluginConfig{}; configMu.Unlock() })
	raw, failed := handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "claude", fmt.Sprintf(claudeToolRequest, "false")))
	if failed || !strings.Contains(payloadOf(t, raw), `"thinking":"Thought briefly."`) {
		t.Fatalf("opt-in thinking missing: %s", payloadOf(t, raw))
	}
	// OpenAI clients always receive the reasoning summary.
	raw, failed = handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "openai", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if failed || !strings.Contains(payloadOf(t, raw), `"reasoning_content":"Thought briefly."`) {
		t.Fatalf("chat reasoning missing: %s", payloadOf(t, raw))
	}
}

func TestExecuteResponsesAndChat(t *testing.T) {
	f := newFakeMake(t)
	f.reply(`<tool_call name="shell">{"cmd":"ls"}</tool_call>`)
	responses := `{"model":"make/gpt-6.2-sol","input":[{"role":"user","content":[{"type":"input_text","text":"list files"}]}],"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	raw, failed := handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "openai-response", responses))
	if failed || !strings.Contains(payloadOf(t, raw), `"type":"function_call"`) {
		t.Fatalf("responses execution: %s", raw)
	}
	chat := `{"model":"make/gpt-6.2-sol","messages":[{"role":"user","content":"hi"}]}`
	f.reply("Hi!")
	raw, failed = handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "openai", chat))
	if failed || !strings.Contains(payloadOf(t, raw), "Hi!") {
		t.Fatalf("chat execution: %s", raw)
	}
}

func payloadOf(t *testing.T, raw []byte) string {
	t.Helper()
	var envelope pluginabi.Envelope
	var response pluginapi.ExecutorResponse
	if json.Unmarshal(raw, &envelope) != nil || json.Unmarshal(envelope.Result, &response) != nil {
		t.Fatalf("invalid envelope %s", raw)
	}
	return string(response.Payload)
}

func joinFrames(frames [][]byte) []byte {
	var out []byte
	for _, frame := range frames {
		out = append(out, frame...)
		out = append(out, '\n')
	}
	return out
}

func TestLongRunIsPolled(t *testing.T) {
	f := newFakeMake(t)
	pollInterval = time.Millisecond
	polls := 0
	f.tools["scenario_run"] = func(map[string]any) (any, error) {
		return map[string]any{"executionId": "0123456789abcdef0123456789abcdef", "status": "running"}, nil
	}
	f.tools["scenario_execution_get"] = func(args map[string]any) (any, error) {
		polls++
		if args["executionId"] != "0123456789abcdef0123456789abcdef" {
			return nil, fmt.Errorf("wrong execution")
		}
		if polls < 3 {
			return map[string]any{"execution": map[string]any{"status": "running"}}, nil
		}
		return map[string]any{"execution": map[string]any{"status": "success"}, "outputs": map[string]any{"response": "done", "usage": "{}"}}, nil
	}
	raw, failed := handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "openai", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if failed || !strings.Contains(payloadOf(t, raw), `"content":"done"`) || polls != 3 {
		t.Fatalf("polling failed after %d polls: %s", polls, raw)
	}
}

func TestFailedRunMapsStatus(t *testing.T) {
	f := newFakeMake(t)
	f.tools["scenario_run"] = func(map[string]any) (any, error) {
		return map[string]any{"executionId": "e", "status": "error", "error": map[string]any{"message": "Not enough credits to run the AI agent"}}, nil
	}
	raw, failed := handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "openai", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	var envelope pluginabi.Envelope
	_ = json.Unmarshal(raw, &envelope)
	if !failed || envelope.Error.HTTPStatus != 429 || !envelope.Error.Retryable {
		t.Fatalf("credit exhaustion must be a retryable 429: %s", raw)
	}

	// Request-level agent failures must stay below 500 so the host does not
	// put the credential into cooldown.
	for message, want := range map[string]int{
		`400 | DataError: Steps per agent call limit of 3 was reached without completing the requested task.`: 422,
		`400 | DataError: Invalid arguments for tool 'x'`:                                                     400,
		`Internal error`: 502,
	} {
		err := runFailure(runStatus{Error: encode(map[string]any{"message": message})})
		var se *statusError
		if !errors.As(err, &se) || se.status != want {
			t.Fatalf("%q mapped to %v, want %d", message, err, want)
		}
	}
}

func TestMissingGatewayIsReprovisioned(t *testing.T) {
	f := newFakeMake(t)
	f.provisioning()
	runs := 0
	f.tools["scenario_run"] = func(args map[string]any) (any, error) {
		runs++
		if args["scenarioId"].(float64) == 1001 && runs == 1 {
			return nil, fmt.Errorf("Scenario not found")
		}
		return map[string]any{"executionId": "e", "status": "success", "outputs": map[string]any{"response": "ok", "usage": "{}"}}, nil
	}
	raw, failed := handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "openai", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if failed || runs != 2 || !strings.Contains(strings.Join(f.toolCalls, ","), "scenario_create") {
		t.Fatalf("gateway not re-provisioned (runs=%d, tools=%v): %s", runs, f.toolCalls, raw)
	}
}

func TestLoginProvisionsGateway(t *testing.T) {
	f := newFakeMake(t)
	f.provisioning()
	// The ID token carries the address; Make's REST API refuses MCP tokens.
	f.email = ""
	f.idEmail = "id@example.com"
	dir := t.TempDir()
	configMu.Lock()
	config = pluginConfig{PublicBaseURL: "https://ai.example.com"}
	configMu.Unlock()
	t.Cleanup(func() { configMu.Lock(); config = pluginConfig{}; configMu.Unlock() })

	result, err := dispatch(pluginabi.MethodAuthLoginStart, encode(map[string]any{"BaseURL": "http://127.0.0.1:8317/v0/management/oauth-callback", "Host": map[string]any{"AuthDir": dir}, "host_callback_id": "cb"}))
	if err != nil {
		t.Fatal(err)
	}
	start := result.(pluginapi.AuthLoginStartResponse)
	authURL, _ := url.Parse(start.URL)
	query := authURL.Query()
	if query.Get("redirect_uri") != "https://ai.example.com/v0/management/oauth-callback" || query.Get("client_id") != "client-1" ||
		query.Get("code_challenge_method") != "S256" || !strings.Contains(query.Get("scope"), "mcp:use") || !strings.HasSuffix(query.Get("scope"), " openid email") {
		t.Fatalf("unexpected authorize URL %s", start.URL)
	}
	poll := encode(map[string]any{"State": start.State, "Host": map[string]any{"AuthDir": dir}, "host_callback_id": "cb"})
	result, err = dispatch(pluginabi.MethodAuthLoginPoll, poll)
	if err != nil || result.(pluginapi.AuthLoginPollResponse).Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("expected pending, got %+v %v", result, err)
	}
	callbackFile := filepath.Join(dir, ".oauth-make-"+start.State+".oauth")
	if err := os.WriteFile(callbackFile, encode(map[string]string{"code": "code-1", "state": start.State}), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = dispatch(pluginabi.MethodAuthLoginPoll, poll)
	if err != nil {
		t.Fatal(err)
	}
	done := result.(pluginapi.AuthLoginPollResponse)
	var s storage
	_ = json.Unmarshal(done.Auth.StorageJSON, &s)
	if done.Status != pluginapi.AuthLoginStatusSuccess || s.ScenarioID != 1001 || s.ConnectionID != 13 || s.TeamID != 5 ||
		s.OrganizationID != 77 || s.UserID != "4242" || s.AccessToken != "" || done.Auth.Label != "id@example.com" ||
		s.Email != "id@example.com" || done.Auth.Metadata["email"] != "id@example.com" || f.meCalls != 0 || len(f.logs) != 0 {
		t.Fatalf("unexpected login result %+v %+v", done, s)
	}
	if saved := f.saved["make-77-4242.json"]; saved.ScenarioID != 1001 || saved.RefreshToken == "" {
		t.Fatalf("credential not persisted: %+v", saved)
	}
	if _, err := os.Stat(callbackFile); !os.IsNotExist(err) {
		t.Fatal("callback file must be removed")
	}
}

func TestRefreshRecordsEmail(t *testing.T) {
	f := newFakeMake(t)
	f.provisioning()
	refresh := func(s storage) pluginapi.AuthData {
		t.Helper()
		result, err := dispatch(pluginabi.MethodAuthRefresh, encode(map[string]any{"AuthID": "make-77-4242.json", "StorageJSON": encode(s), "host_callback_id": "cb"}))
		if err != nil {
			t.Fatal(err)
		}
		return result.(pluginapi.AuthRefreshResponse).Auth
	}
	// A credential from before the address was recorded gets it on refresh.
	auth := refresh(credential())
	var s storage
	_ = json.Unmarshal(auth.StorageJSON, &s)
	if s.Email != "ada@example.com" || auth.Metadata["email"] != "ada@example.com" || auth.Label != "ada@example.com" {
		t.Fatalf("email not recorded: %q %+v %+v", auth.Label, auth.Metadata, s)
	}
	// A known address is not looked up again.
	refresh(s)
	if f.meCalls != 1 {
		t.Fatalf("users/me called %d times", f.meCalls)
	}

	// A failed lookup leaves the address out but keeps the refresh.
	f = newFakeMake(t)
	f.provisioning()
	f.email = ""
	auth = refresh(credential())
	s = storage{}
	_ = json.Unmarshal(auth.StorageJSON, &s)
	if _, ok := auth.Metadata["email"]; ok || s.Email != "" || s.RefreshToken != "refresh-1" || auth.Label != "Make Acme" {
		t.Fatalf("failed lookup must not break the refresh: %q %+v %+v", auth.Label, auth.Metadata, s)
	}
	if len(f.logs) != 1 || !strings.Contains(f.logs[0], "users/me: HTTP 403 Forbidden") || !strings.Contains(f.logs[0], "userinfo: HTTP 401 invalid_token") ||
		!strings.Contains(f.logs[0], "environment_get") || strings.Contains(f.logs[0], "Bearer") {
		t.Fatalf("failed lookup must be logged without the token: %v", f.logs)
	}

	// Without users/me, OpenID userinfo supplies the address.
	f = newFakeMake(t)
	f.email = ""
	f.userinfoEmail = "grace@example.com"
	if auth = refresh(credential()); auth.Label != "grace@example.com" {
		t.Fatalf("userinfo fallback not used: %q", auth.Label)
	}

	// Without either, a read-only MCP user tool supplies it; tools that
	// change users are never called.
	f = newFakeMake(t)
	f.provisioning()
	f.email = ""
	f.tools["users_rename"] = func(map[string]any) (any, error) { t.Error("users_rename called"); return nil, nil }
	f.tools["users_me"] = func(map[string]any) (any, error) {
		return map[string]any{"authUser": map[string]any{"id": 4242, "email": "linus@example.com"}}, nil
	}
	if auth = refresh(credential()); auth.Label != "linus@example.com" {
		t.Fatalf("MCP user tool not used: %q %v", auth.Label, f.logs)
	}
}

func TestEmailRecordsAndTools(t *testing.T) {
	for raw, want := range map[string]string{
		`{"email":"a@example.com"}`:                                   "a@example.com",
		`{"authUser":{"email":"b@example.com"}}`:                      "b@example.com",
		`{"organizations":[{"members":[{"email":"c@example.com"}]}]}`: "",
		`{"email":"not-an-address"}`:                                  "",
		`[{"email":"d@example.com"}]`:                                 "",
	} {
		if got := findEmail(json.RawMessage(raw)); got != want {
			t.Errorf("findEmail(%s) = %q, want %q", raw, got, want)
		}
	}
	for name, want := range map[string]bool{"users_me": true, "current_user_get": true, "whoami": true, "users_rename": false,
		"users_list": false, "team_users_list": false, "me_delete": false} {
		if userTool(name) != want {
			t.Errorf("userTool(%q) = %v, want %v", name, !want, want)
		}
	}
}

func TestEmailLookupStaysOnMakeHosts(t *testing.T) {
	for zone, want := range map[string]bool{"eu1.make.com": true, "us2.make.celonis.com": true, "": false, "evil.example": false,
		"eu1.make.com@evil.example": false, "evil.example/x.make.com": false, "make.com.evil.example": false} {
		if zoneHost(zone) != want {
			t.Errorf("zoneHost(%q) = %v, want %v", zone, !want, want)
		}
	}
}

func TestRotatedTokenSurvivesStaleHostCopy(t *testing.T) {
	f := newFakeMake(t)
	stale := encode(credential())
	first, _, err := liveStorage("cb", "make-77-4242.json", stale, true)
	if err != nil {
		t.Fatal(err)
	}
	// The host still holds the pre-rotation copy; the cached newer copy wins.
	second, _, err := liveStorage("cb", "make-77-4242.json", stale, true)
	if err != nil || second.RefreshToken == first.RefreshToken || f.refreshes != 2 {
		t.Fatalf("rotation lost: first=%s second=%s err=%v", first.RefreshToken, second.RefreshToken, err)
	}
	f.tokenErr = "invalid_grant"
	_, _, err = liveStorage("cb", "make-77-4242.json", stale, true)
	var envelope pluginabi.Envelope
	raw, _ := handleMethod(pluginabi.MethodAuthRefresh, encode(map[string]any{"AuthID": "make-77-4242.json", "StorageJSON": stale}))
	_ = json.Unmarshal(raw, &envelope)
	if err == nil || envelope.Error == nil || envelope.Error.HTTPStatus != 401 {
		t.Fatalf("revoked sign-in must be a 401: %v %s", err, raw)
	}
}

func TestTokenRequestsAvoidPooledConnections(t *testing.T) {
	f := newFakeMake(t)
	if _, _, err := liveStorage("cb", "make-77-4242.json", encode(credential()), true); err != nil {
		t.Fatal(err)
	}
	// A draining gateway replica must fail before Make rotates the token.
	if len(f.tokenHeaders) != 1 || strings.Join(f.tokenHeaders[0]["Connection"], ",") != "close" {
		t.Fatalf("token request headers %v", f.tokenHeaders)
	}
}

func TestLostRefreshTokenAsksForSignIn(t *testing.T) {
	for _, message := range []string{"Refresh token not found", "Client not found"} {
		t.Run(message, func(t *testing.T) {
			f := newFakeMake(t)
			f.tokenReply = &upstreamResponse{StatusCode: 404, Body: encode(map[string]string{"code": "SC404", "message": message})}
			raw, failed := handleMethod(pluginabi.MethodAuthRefresh, encode(map[string]any{"AuthID": "make-77-4242.json", "StorageJSON": encode(credential())}))
			var envelope pluginabi.Envelope
			_ = json.Unmarshal(raw, &envelope)
			if !failed || envelope.Error == nil || envelope.Error.HTTPStatus != 401 || envelope.Error.Retryable || !strings.Contains(envelope.Error.Message, "sign in to Make again") {
				t.Fatalf("a lost Make credential must ask for a new sign-in: %s", raw)
			}
		})
	}
	f := newFakeMake(t)
	f.tokenReply = &upstreamResponse{StatusCode: 404, Body: []byte(`{"message":"Not Found"}`)}
	raw, _ := handleMethod(pluginabi.MethodAuthRefresh, encode(map[string]any{"AuthID": "make-77-4242.json", "StorageJSON": encode(credential())}))
	var envelope pluginabi.Envelope
	_ = json.Unmarshal(raw, &envelope)
	if envelope.Error == nil || envelope.Error.HTTPStatus != 404 {
		t.Fatalf("an unrelated 404 must keep its status: %s", raw)
	}
}

func TestPausedOrganizationIsAQuotaLimit(t *testing.T) {
	f := newFakeMake(t)
	runs := 0
	f.tools["scenario_run"] = func(map[string]any) (any, error) {
		runs++
		return nil, fmt.Errorf("Nothing ran: the organization or team is paused until its operations or data-transfer limit is resolved.")
	}
	chat := `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
	raw, failed := handleMethod(pluginabi.MethodExecutorExecute, executorPayload(false, "openai", chat))
	var envelope pluginabi.Envelope
	_ = json.Unmarshal(raw, &envelope)
	if !failed || envelope.Error == nil || envelope.Error.HTTPStatus != 429 || !envelope.Error.Retryable || !strings.Contains(envelope.Error.Message, "paused") {
		t.Fatalf("a paused organization must be a retryable 429: %s", raw)
	}
	// The pause covers the whole organization, so the host must cool the
	// credential down for every model, long enough to move on to another one.
	if envelope.Error.Code != framework.CredentialQuotaCode || envelope.Error.RetryAfterSeconds != int(pauseCooldown/time.Second) {
		t.Fatalf("a paused organization must cool the whole credential down: %s", raw)
	}
	// A stream would report the pause after its headers; later requests
	// report it up front without running the gateway again.
	raw, failed = handleMethod(pluginabi.MethodExecutorExecuteStream, executorPayload(true, "openai", chat))
	envelope = pluginabi.Envelope{}
	_ = json.Unmarshal(raw, &envelope)
	if !failed || envelope.Error == nil || envelope.Error.HTTPStatus != 429 || runs != 1 || envelope.Error.Code != framework.CredentialQuotaCode {
		t.Fatalf("paused organization not reported before streaming (runs=%d): %s", runs, raw)
	}
}

func TestRateLimitIsNotAPause(t *testing.T) {
	if after, scoped := (&mcpError{tool: "x", status: 429, message: "Too many requests"}).QuotaLimit(); after != 0 || scoped {
		t.Fatalf("a plain rate limit must not cool the credential down for the pause: %d %v", after, scoped)
	}
}

func TestQuota(t *testing.T) {
	f := newFakeMake(t)
	f.tools["organization_get"] = func(map[string]any) (any, error) {
		return map[string]any{"productName": "Pro", "centicreditsConsumed": "1500000", "centicreditsExtra": "0", "nextReset": "2026-12-17T16:05:10.363Z",
			"license": map[string]any{"operations": 150000}}, nil
	}
	result, err := dispatch(pluginabi.MethodQuotaFetch, encode(map[string]any{"auth_id": "make-77-4242.json", "provider": "make", "storage_json": encode(credential())}))
	if err != nil {
		t.Fatal(err)
	}
	q := result.(pluginapi.QuotaFetchResponse)
	bucket := q.Groups[0].Buckets[0]
	if q.Summary[0].Value != 15000 || q.Summary[1].Value != 150000 || bucket.RemainingFraction != 0.9 || bucket.ResetTime != "2026-12-17T16:05:10.363Z" {
		t.Fatalf("unexpected quota %+v", q)
	}
}

func TestConfigAndRegistration(t *testing.T) {
	yaml := []byte("public_base_url: https://ai.example.com/\nteam_id: \"3665373\"\nclaude_thinking: true\n")
	cfg, err := parseConfig(yaml)
	if err != nil || cfg.PublicBaseURL != "https://ai.example.com" || cfg.TeamID != 3665373 || !cfg.ClaudeThinking {
		t.Fatalf("config %+v %v", cfg, err)
	}
	if _, err := parseConfig([]byte("public_base_url: ftp://x\n")); err == nil {
		t.Fatal("non-HTTP base URL must be rejected")
	}
	result, err := dispatch(pluginabi.MethodModelForAuth, nil)
	if err != nil || result.(pluginapi.ModelResponse).Models[0].ID != "make/gpt-6.2-sol" {
		t.Fatalf("models %+v %v", result, err)
	}
}

func TestRotationKeepsCooldownSwitch(t *testing.T) {
	f := newFakeMake(t)
	if _, _, err := liveStorage("cb", "make-77-4242.json", encode(credential()), true); err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	_ = json.Unmarshal(f.savedRaw["make-77-4242.json"], &saved)
	if value, ok := saved["disable_cooling"]; !ok || value != false {
		t.Fatalf("a rotated credential must keep cooldown enabled: %s", f.savedRaw["make-77-4242.json"])
	}
	optedOut := credential()
	optedOut.DisableCooling = true
	optedOut.RefreshToken = f.refresh
	if _, _, err := liveStorage("cb", "make-78-4242.json", encode(optedOut), true); err != nil {
		t.Fatal(err)
	}
	if !f.saved["make-78-4242.json"].DisableCooling {
		t.Fatalf("rotation dropped an explicit disable_cooling: %s", f.savedRaw["make-78-4242.json"])
	}
}
