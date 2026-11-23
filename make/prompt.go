package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const modelPrefix = "make/"

type modelSpec struct {
	Native       string
	Display      string
	Levels       []string
	DefaultLevel string
	Context      int64
	Output       int64
}

// Models offered by "Make's AI Provider" to the agent module on paid plans.
// Reasoning levels are the ones Make accepts per model.
var models = []modelSpec{
	{"gpt-6.2-sol", "GPT-6.2 Sol (Make)", []string{"low", "medium", "high", "xhigh", "max"}, "medium", 1050000, 128000},
	{"gpt-6.1-sol", "GPT-6.1 Sol (Make)", []string{"low", "medium", "high", "xhigh", "max"}, "medium", 1050000, 128000},
	{"gpt-6-astra", "GPT-6 Astra (Make)", []string{"none", "low", "medium", "high"}, "low", 1050000, 128000},
	{"gpt-6-sol", "GPT-6 Sol (Make)", []string{"none", "low", "medium", "high"}, "low", 1050000, 128000},
	{"gpt-6-luna", "GPT-6 Luna (Make)", []string{"none", "low", "medium", "high"}, "low", 1050000, 128000},
	{"gpt-5.6-sol", "GPT-5.6 Sol (Make)", []string{"none", "low", "medium", "high"}, "low", 400000, 128000},
	{"gpt-5.6-terra", "GPT-5.6 Terra (Make)", []string{"none", "low", "medium", "high"}, "low", 400000, 128000},
	{"gpt-5.6-luna", "GPT-5.6 Luna (Make)", []string{"none", "low", "medium", "high"}, "low", 400000, 128000},
}

func modelInfos() []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, pluginapi.ModelInfo{ID: modelPrefix + m.Native, Object: "model", OwnedBy: providerKey, Type: providerKey,
			Name: modelPrefix + m.Native, DisplayName: m.Display, ContextLength: m.Context, InputTokenLimit: m.Context,
			OutputTokenLimit: m.Output, MaxCompletionTokens: m.Output,
			Thinking: &pluginapi.ThinkingSupport{Levels: append([]string(nil), m.Levels...), ZeroAllowed: contains(m.Levels, "none")}})
	}
	return out
}

func findModel(id string) (modelSpec, bool) {
	native := strings.TrimPrefix(strings.TrimSpace(id), modelPrefix)
	for _, m := range models {
		if m.Native == native {
			return m, true
		}
	}
	return modelSpec{}, false
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// reasoningLevel maps a requested effort onto the closest level the model
// supports.
func reasoningLevel(m modelSpec, requested string) string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" || requested == "auto" {
		return m.DefaultLevel
	}
	if contains(m.Levels, requested) {
		return requested
	}
	order := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	rank := func(level string) int {
		for i, item := range order {
			if item == level {
				return i
			}
		}
		return -1
	}
	want := rank(requested)
	if want < 0 {
		return m.DefaultLevel
	}
	best, distance := m.DefaultLevel, len(order)
	for _, level := range m.Levels {
		d := rank(level) - want
		if d < 0 {
			d = -d
		}
		if d < distance {
			best, distance = level, d
		}
	}
	return best
}

// chatRequest is the OpenAI Chat Completions shape every client protocol is
// translated to before rendering.
type chatRequest struct {
	Messages        []chatMessage   `json:"messages"`
	Tools           []chatTool      `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	ReasoningEffort string          `json:"reasoning_effort"`
	ResponseFormat  json.RawMessage `json:"response_format"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

const protocolPrompt = `You are the language model behind a chat completion API. The user message you receive is not typed by a person: it is a transcript of the whole conversation so far, and your task is to write only the next assistant turn.

Transcript format:
- <user>...</user> is a user message and <assistant>...</assistant> is one of your earlier replies.
- <system>...</system> carries additional instructions that apply from that point on.
- <tool_call id="..." name="...">ARGUMENTS</tool_call> inside an assistant turn is a tool call you made earlier, and <tool_result id="..." name="...">...</tool_result> is what the client returned for it.

How to answer:
- Write the next assistant message directly, as plain text (Markdown is fine). Do not wrap it in <assistant> tags and do not repeat the transcript.
- Never call a function or native tool. The runtime may offer built-in functions such as generate_file_output; they are not part of this conversation and calling them fails. The client tools listed below are not functions either: they exist only as text you write.
- To use one of the client tools, end your message with one block per call, written as plain text exactly in this form:
<tool_call name="TOOL_NAME">
{"argument": "value"}
</tool_call>
- TOOL_NAME is the tool's name exactly as it is listed under "Client tools", with no prefix such as "functions." or "functions__".
- The body of a block is a single JSON object matching that tool's parameter schema. Use several blocks to call tools in parallel.
- The body must be valid JSON. Inside JSON strings, escape every backslash as \\ and every double quote as \", and write line breaks as \n, on top of any escaping the code or command inside the string needs.
- Stop right after the last block. The client runs the tools and the next transcript will contain their results. Never write a tool result yourself.`

const turnReminder = "\n(End of transcript. Write the next assistant message now. Do not call any function.)"

const turnReminderTools = "\n(End of transcript. Write the next assistant message now. Do not call any function: request client tools only with <tool_call name=\"...\"> text blocks.)"

// renderPrompt turns a chat request into the agent's system prompt and input
// message. Both are deterministic and the transcript only grows at its end,
// so consecutive turns share a long cacheable prefix.
func renderPrompt(req chatRequest) (string, string, map[string]bool) {
	var system strings.Builder
	system.WriteString(protocolPrompt)
	toolNames := map[string]bool{}
	if len(req.Tools) > 0 {
		system.WriteString("\n\n# Client tools\n")
		for _, tool := range req.Tools {
			if tool.Type != "" && tool.Type != "function" {
				continue
			}
			name := strings.TrimSpace(tool.Function.Name)
			if name == "" {
				continue
			}
			toolNames[name] = true
			fmt.Fprintf(&system, "\n## %s\n", name)
			if description := strings.TrimSpace(tool.Function.Description); description != "" {
				system.WriteString(description)
				system.WriteString("\n")
			}
			parameters := compactJSON(tool.Function.Parameters)
			if parameters == "" {
				parameters = `{"type":"object","properties":{}}`
			}
			fmt.Fprintf(&system, "Parameters (JSON Schema): %s\n", parameters)
		}
	} else {
		system.WriteString("\n\nNo client tools are available in this conversation; answer with text only.")
	}
	if rule := toolChoiceRule(req.ToolChoice); rule != "" && len(toolNames) > 0 {
		system.WriteString("\n\n# Tool choice\n")
		system.WriteString(rule)
	}
	if format := responseFormatRule(req.ResponseFormat); format != "" {
		system.WriteString("\n\n# Response format\n")
		system.WriteString(format)
	}

	messages := req.Messages
	var instructions []string
	for len(messages) > 0 && (messages[0].Role == "system" || messages[0].Role == "developer") {
		if text := strings.TrimSpace(contentText(messages[0].Content)); text != "" {
			instructions = append(instructions, text)
		}
		messages = messages[1:]
	}
	if len(instructions) > 0 {
		system.WriteString("\n\n# Instructions from the client application\n\n")
		system.WriteString(strings.Join(instructions, "\n\n"))
	}

	toolNamesByID := map[string]string{}
	var transcript strings.Builder
	for _, message := range messages {
		switch message.Role {
		case "system", "developer":
			fmt.Fprintf(&transcript, "<system>\n%s\n</system>\n", strings.TrimSpace(contentText(message.Content)))
		case "user":
			fmt.Fprintf(&transcript, "<user>\n%s\n</user>\n", strings.TrimSpace(contentText(message.Content)))
		case "assistant":
			transcript.WriteString("<assistant>\n")
			if text := strings.TrimSpace(contentText(message.Content)); text != "" {
				transcript.WriteString(text)
				transcript.WriteString("\n")
			}
			for _, call := range message.ToolCalls {
				toolNamesByID[call.ID] = call.Function.Name
				arguments := compactJSON(json.RawMessage(call.Function.Arguments))
				if arguments == "" {
					arguments = strings.TrimSpace(call.Function.Arguments)
				}
				fmt.Fprintf(&transcript, "<tool_call id=%q name=%q>%s</tool_call>\n", call.ID, call.Function.Name, arguments)
			}
			transcript.WriteString("</assistant>\n")
		case "tool", "function":
			name := toolNamesByID[message.ToolCallID]
			if name == "" {
				name = message.Name
			}
			fmt.Fprintf(&transcript, "<tool_result id=%q name=%q>\n%s\n</tool_result>\n", message.ToolCallID, name, strings.TrimRight(contentText(message.Content), "\n"))
		}
	}
	// A fixed trailer after the transcript keeps the cacheable prefix intact
	// and repeats the one rule models most often break.
	if len(toolNames) > 0 {
		transcript.WriteString(turnReminderTools)
	} else {
		transcript.WriteString(turnReminder)
	}
	return system.String(), transcript.String(), toolNames
}

func toolChoiceRule(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "none":
			return "Do not call any tool in this turn; answer with text only."
		case "required":
			return "You must call at least one tool in this turn."
		}
		return ""
	}
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &named) == nil && named.Function.Name != "" {
		return fmt.Sprintf("You must call the tool %q in this turn.", named.Function.Name)
	}
	return ""
}

func responseFormatRule(raw json.RawMessage) string {
	var format struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &format) != nil {
		return ""
	}
	switch format.Type {
	case "json_object":
		return "Answer with a single valid JSON object and nothing else."
	case "json_schema":
		return "Answer with a single valid JSON value matching this JSON Schema and nothing else: " + compactJSON(format.JSONSchema.Schema)
	}
	return ""
}

func compactJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var out bytes.Buffer
	if json.Compact(&out, raw) != nil {
		return ""
	}
	// Re-encode objects with sorted keys so equal schemas render identically.
	var value any
	if json.Unmarshal(out.Bytes(), &value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			return string(canonical)
		}
	}
	return out.String()
}

// contentText flattens string or multi-part content. Non-text parts are
// represented by a placeholder because the gateway accepts text only.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var out []string
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text", "":
			out = append(out, part.Text)
		case "image_url", "input_image", "image":
			out = append(out, "[image omitted: this provider accepts text only]")
		default:
			out = append(out, "["+part.Type+" omitted]")
		}
	}
	return strings.Join(out, "\n")
}
