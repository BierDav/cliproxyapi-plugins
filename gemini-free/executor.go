package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/BierDav/cliproxyapi-plugins/framework/sse"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var registry = builtin.Registry()

func geminiRequest(key, model, action string, body []byte) framework.HTTPRequest {
	return framework.HTTPRequest{
		Method: http.MethodPost, URL: geminiAPI + "models/" + model + ":" + action,
		Headers: http.Header{"X-Goog-Api-Key": {key}, "Content-Type": {"application/json"}}, Body: body,
	}
}

func (p *providerPlugin) execute(method string, r rpc) (any, error) {
	var c credential
	if json.Unmarshal(r.StorageJSON, &c) != nil || !c.valid() {
		return nil, failure(http.StatusUnauthorized, "Invalid Gemini Free credential; enroll again")
	}
	alias := strings.TrimPrefix(r.Model, "models/")
	model, vertex, ok := p.models().upstream(alias)
	if !ok || (vertex && c.Vertex == "") {
		return nil, failure(http.StatusNotFound, "Gemini Free does not serve this model")
	}
	source := translator.FromString(r.SourceFormat)
	if source == "" {
		source = translator.FromString(r.Format)
	}
	if source != translator.FormatGemini && source != translator.FormatOpenAI && source != translator.FormatOpenAIResponse {
		return nil, failure(http.StatusBadRequest, "Unsupported Gemini Free request format")
	}
	body := r.Payload
	stream := method == pluginabi.MethodExecutorExecuteStream
	if source != translator.FormatGemini {
		body = registry.TranslateRequest(source, translator.FormatGemini, model, body, stream)
	}
	if !json.Valid(body) || !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		return nil, failure(http.StatusBadRequest, "Invalid Gemini request body")
	}
	body, _ = sjson.DeleteBytes(body, "model")
	request := func(action string, payload []byte) framework.HTTPRequest {
		return geminiRequest(c.Key, model, action, payload)
	}
	if vertex {
		token, err := p.vertexAccessToken(r.HostCallbackID, c)
		if err != nil {
			return nil, err
		}
		request = func(action string, payload []byte) framework.HTTPRequest {
			return vertexRequest(token, c.Vertex, model, action, payload)
		}
		if method != pluginabi.MethodExecutorCountTokens {
			body = vertexBody(body)
		}
	}
	if stream {
		return p.startStream(r, source, request("streamGenerateContent?alt=sse", body), alias, vertex)
	}
	var result json.RawMessage
	act := "generateContent"
	if method == pluginabi.MethodExecutorCountTokens {
		act = "countTokens"
	}
	requestBody := body
	if act == "countTokens" && vertex {
		requestBody = vertexCountBody(body)
	} else if act == "countTokens" && source != translator.FormatGemini {
		// The Gemini countTokens method takes either contents or a complete
		// generateContentRequest, not generation fields at the top level.
		payload, _ := sjson.SetBytes(body, "model", "models/"+model)
		requestBody, _ = json.Marshal(map[string]json.RawMessage{"generateContentRequest": payload})
	} else if act == "countTokens" && gjson.GetBytes(body, "generateContentRequest.model").Exists() {
		requestBody, _ = sjson.SetBytes(body, "generateContentRequest.model", "models/"+model)
	}
	req := request(act, requestBody)
	var err error
	if vertex && act == "generateContent" {
		body, err = p.generateVertex(r.HostCallbackID, req, &result)
	} else {
		err = p.call(r.HostCallbackID, act+" with Gemini Free", req, &result)
	}
	if err != nil {
		return nil, err
	}
	if source != translator.FormatGemini {
		if act == "countTokens" {
			var count struct{ TotalTokens int64 }
			if err := json.Unmarshal(result, &count); err != nil {
				return nil, failure(http.StatusBadGateway, "Invalid Gemini token count")
			}
			result = registry.TranslateTokenCount(context.Background(), translator.FormatGemini, source, count.TotalTokens, result)
		} else {
			result = registry.TranslateNonStream(context.Background(), translator.FormatGemini, source, alias, r.OriginalRequest, body, result, nil)
		}
	}
	return pluginapi.ExecutorResponse{Payload: result, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
}

func (p *providerPlugin) generateVertex(callbackID string, req framework.HTTPRequest, result *json.RawMessage) ([]byte, error) {
	const step = "generateContent with Gemini Free"
	ctx := context.Background()
	resp, err := p.host.Do(ctx, callbackID, req)
	if err == nil && resp.StatusCode == http.StatusBadRequest {
		if repaired := repairVertexSignatures(req.Body, resp.Body); repaired != nil {
			req.Body = repaired
			resp, err = p.host.Do(ctx, callbackID, req)
		}
	}
	return req.Body, decodeGoogleResponse(step, resp, err, result)
}

func (p *providerPlugin) startStream(r rpc, source translator.Format, req framework.HTTPRequest, alias string, vertex bool) (any, error) {
	if r.StreamID == "" {
		return nil, failure(http.StatusInternalServerError, "Missing Gemini Free output stream")
	}
	ctx := context.Background()
	req.Headers.Set("Accept", "text/event-stream")
	upstream, err := p.host.OpenStream(ctx, r.HostCallbackID, req)
	// Retry only a rejection received before any response was sent to the client.
	if vertex && err == nil && upstream.StatusCode == http.StatusBadRequest {
		if repaired := repairVertexSignatures(req.Body, p.readVertexRejection(ctx, upstream.ID)); repaired != nil {
			_ = p.host.CloseStream(ctx, upstream.ID)
			req.Body = repaired
			upstream, err = p.host.OpenStream(ctx, r.HostCallbackID, req)
		}
	}
	if err != nil {
		return nil, failure(http.StatusBadGateway, "Could not open the Gemini Free stream")
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		_ = p.host.CloseStream(ctx, upstream.ID)
		return nil, &googleError{status: upstream.StatusCode, step: "stream Gemini Free output"}
	}
	go p.forwardStream(r, source, alias, upstream.ID, req.Body)
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}}}, nil
}

func (p *providerPlugin) forwardStream(r rpc, source translator.Format, alias, upstream string, body []byte) {
	ctx := context.Background()
	defer p.host.CloseStream(ctx, upstream)
	var failureMessage string
	defer func() { p.host.CloseOutput(ctx, r.StreamID, failureMessage) }()
	var decoder sse.Decoder
	var state any
	emit := func(data []byte) bool {
		outputs := [][]byte{data}
		if source != translator.FormatGemini {
			outputs = registry.TranslateStream(ctx, translator.FormatGemini, source, alias, r.OriginalRequest, body, data, &state)
		}
		for _, output := range outputs {
			if source == translator.FormatOpenAI {
				output = sse.DataPayload(output)
			}
			if len(output) > 0 && p.host.Emit(ctx, r.StreamID, output) != nil {
				return false
			}
		}
		return true
	}
	forward := func(frame []byte) bool {
		data := sse.DataPayload(frame)
		return len(data) == 0 || !json.Valid(data) || emit(data)
	}
	for {
		chunk, err := p.host.ReadStream(ctx, upstream)
		if err != nil || chunk.Error != "" {
			failureMessage = "Google interrupted the Gemini Free stream"
			return
		}
		for _, frame := range decoder.Feed(chunk.Payload) {
			if !forward(frame) {
				return
			}
		}
		if chunk.Done {
			if tail := decoder.Flush(); len(tail) > 0 && !forward(tail) {
				return
			}
			if source != translator.FormatGemini && state != nil {
				emit([]byte("[DONE]")) // flush translators that buffer the terminal event
			}
			return
		}
	}
}
