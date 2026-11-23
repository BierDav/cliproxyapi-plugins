package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework/sse"
)

// Gemini is available through JetBrains' aggregate protocol. Other families
// retain their native provider wire format, including thinking and tool calls.
func aggregateRequest(raw []byte, profile string) ([]byte, error) {
	var request struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			Name       string          `json:"name"`
			Calls      []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		MaxTokens   *int     `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		Reasoning   string   `json:"reasoning_effort"`
	}
	if json.Unmarshal(raw, &request) != nil {
		return nil, fmt.Errorf("Invalid aggregate request")
	}
	messages := []map[string]any{}
	toolNames := map[string]string{}
	for _, m := range request.Messages {
		for _, call := range m.Calls {
			toolNames[call.ID] = call.Function.Name
		}
	}
	text := func(raw json.RawMessage) string {
		var plain string
		if json.Unmarshal(raw, &plain) == nil {
			return plain
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return ""
		}
		texts := []string{}
		for _, part := range parts {
			if part.Type == "text" {
				texts = append(texts, part.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	for _, message := range request.Messages {
		content := text(message.Content)
		switch message.Role {
		case "system", "developer":
			if strings.TrimSpace(content) != "" {
				messages = append(messages, map[string]any{"type": "system_message", "content": content})
			}
		case "user":
			var parts []struct {
				Type  string `json:"type"`
				Image struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			if json.Unmarshal(message.Content, &parts) == nil {
				for _, part := range parts {
					if part.Type != "image_url" {
						continue
					}
					address := part.Image.URL
					media := "image/png"
					data := address
					if strings.HasPrefix(address, "data:") {
						pieces := strings.SplitN(address, ",", 2)
						if len(pieces) != 2 {
							return nil, fmt.Errorf("Invalid image data URL")
						}
						media = strings.TrimSuffix(strings.TrimPrefix(pieces[0], "data:"), ";base64")
						data = pieces[1]
					}
					messages = append(messages, map[string]any{"type": "media_message", "mediaType": media, "data": data})
				}
			}
			if strings.TrimSpace(content) != "" {
				messages = append(messages, map[string]any{"type": "user_message", "content": content})
			}
		case "assistant":
			if content != "" {
				messages = append(messages, map[string]any{"type": "assistant_message_text", "content": content})
			}
			for _, call := range message.Calls {
				arguments := call.Function.Arguments
				if arguments == "" {
					arguments = "{}"
				}
				messages = append(messages, map[string]any{"type": "assistant_message_tool", "id": call.ID, "toolName": call.Function.Name, "content": arguments})
			}
		case "tool":
			name := message.Name
			if name == "" {
				name = toolNames[message.ToolCallID]
			}
			if content == "" {
				content = string(message.Content)
			}
			messages = append(messages, map[string]any{"type": "tool_message", "id": message.ToolCallID, "toolName": name, "result": content})
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("JetBrains needs a conversation")
	}
	if messages[len(messages)-1]["type"] != "user_message" {
		messages = append(messages, map[string]any{"type": "user_message", "content": "continue"})
	}
	parameters := []map[string]any{}
	add := func(kind, name string, value any) {
		parameters = append(parameters, map[string]any{"type": kind, "fqdn": "llm.parameters." + name}, map[string]any{"type": kind, "value": value})
	}
	if request.MaxTokens != nil {
		add("int", "length", *request.MaxTokens)
	}
	if request.Temperature != nil {
		v := *request.Temperature
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		add("double", "temperature", v)
	}
	if request.TopP != nil {
		add("double", "top-p", *request.TopP)
	}
	if request.Reasoning != "" {
		add("text", "reasoning-effort", request.Reasoning)
	}
	tools := []map[string]any{}
	for _, tool := range request.Tools {
		if tool.Type != "function" {
			continue
		}
		schema := tool.Function.Parameters
		if !json.Valid(schema) {
			schema = json.RawMessage("{}")
		}
		tools = append(tools, map[string]any{"name": tool.Function.Name, "description": tool.Function.Description, "parameters": map[string]any{"schema": schema}})
	}
	if len(tools) > 0 {
		add("json", "tools", string(encode(tools)))
	}
	result := map[string]any{"prompt": "ij.chat.request.new-chat-on-start", "profile": profile, "chat": map[string]any{"messages": messages}}
	if len(parameters) > 0 {
		result["parameters"] = map[string]any{"data": parameters}
	}
	return json.Marshal(result)
}

type aggregateTool struct{ ID, Name, Arguments string }
type aggregateStream struct {
	ID, Model, Content, Finish, Error string
	Created                           int64
	Done                              bool
	Tools                             map[int]*aggregateTool
}

func newAggregateStream(model string) *aggregateStream {
	return &aggregateStream{ID: "chatcmpl-" + randomString()[:24], Model: model, Created: time.Now().Unix(), Tools: map[int]*aggregateTool{}}
}
func (s *aggregateStream) chunk(delta any, finish any) []byte {
	return []byte("data: " + string(encode(map[string]any{"id": s.ID, "object": "chat.completion.chunk", "created": s.Created, "model": s.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})) + "\n\n")
}
func (s *aggregateStream) feed(frame []byte) [][]byte {
	out := [][]byte{}
	for _, line := range strings.Split(string(frame), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "end" {
			if !s.Done {
				s.Done = true
				out = append(out, []byte("data: [DONE]\n\n"))
			}
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Content string `json:"content"`
			ID      string `json:"id"`
			Name    string `json:"name"`
			Index   int    `json:"parallelToolIndex"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		switch event.Type {
		case "Content":
			s.Content += event.Content
			out = append(out, s.chunk(map[string]any{"content": event.Content}, nil))
		case "ToolCall":
			if event.ID != "" && event.Name != "" {
				s.Tools[event.Index] = &aggregateTool{ID: event.ID, Name: event.Name}
				out = append(out, s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": event.Index, "id": event.ID, "type": "function", "function": map[string]any{"name": event.Name, "arguments": ""}}}}, nil))
			} else if event.Content != "" {
				tool := s.Tools[event.Index]
				if tool == nil {
					s.Error = "JetBrains returned a tool fragment without its identity"
					return out
				}
				tool.Arguments += event.Content
				out = append(out, s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": event.Index, "function": map[string]any{"arguments": event.Content}}}}, nil))
			}
		case "FinishMetadata":
			s.Finish = "stop"
			if event.Reason == "tool_call" {
				s.Finish = "tool_calls"
			}
			out = append(out, s.chunk(map[string]any{}, s.Finish))
		case "Error":
			s.Error = "JetBrains aggregate stream returned an error"
		}
	}
	return out
}
func aggregateResponse(raw []byte, model string) ([]byte, error) {
	stream := newAggregateStream(model)
	decoder := sse.Decoder{}
	for _, frame := range decoder.Feed(raw) {
		stream.feed(frame)
	}
	if tail := decoder.Flush(); len(tail) > 0 {
		stream.feed(tail)
	}
	if stream.Error != "" || stream.Finish == "" {
		return nil, fmt.Errorf("JetBrains aggregate response is incomplete or failed")
	}
	message := map[string]any{"role": "assistant", "content": stream.Content}
	keys := []int{}
	for index := range stream.Tools {
		keys = append(keys, index)
	}
	sort.Ints(keys)
	calls := []any{}
	for _, index := range keys {
		tool := stream.Tools[index]
		calls = append(calls, map[string]any{"id": tool.ID, "type": "function", "function": map[string]any{"name": tool.Name, "arguments": tool.Arguments}})
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	return json.Marshal(map[string]any{"id": stream.ID, "object": "chat.completion", "created": stream.Created, "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": stream.Finish}}})
}
