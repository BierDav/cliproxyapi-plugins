package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/BierDav/cliproxyapi-plugins/framework/sse"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

// Profile-to-native IDs are protocol metadata from JetBrains' profiles API.
//
//go:embed model-map.json
var modelMapJSON []byte

type modelRoute struct {
	Family  string `json:"family"`
	Native  string `json:"native"`
	Profile string `json:"-"`
}

var modelMap = func() map[string]modelRoute {
	var m map[string]modelRoute
	_ = json.Unmarshal(modelMapJSON, &m)
	return m
}()
var registry = builtin.Registry()
var catalogMu sync.Mutex

type catalog struct {
	Expires time.Time
	Models  []pluginapi.ModelInfo
	Routes  map[string]modelRoute
}

var catalogs = map[string]catalog{}

func models(r rpcRequest) (any, error) {
	s, err := liveStorage(r.HostCallbackID, r.AuthID, r.StorageJSON)
	if err != nil {
		return nil, err
	}
	c, err := modelCatalog(r.HostCallbackID, s)
	if err != nil {
		return nil, err
	}
	return pluginapi.ModelResponse{Provider: "jetbrains", Models: c.Models}, nil
}
func modelCatalog(callback string, s storage) (catalog, error) {
	catalogMu.Lock()
	cached, ok := catalogs[s.Email]
	catalogMu.Unlock()
	if ok && time.Now().Before(cached.Expires) {
		return cached, nil
	}
	body, err := checked(callback, "model discovery", "GET", apiBase+"/user/v5/llm/profiles/v8", apiHeaders(s.JWT), nil)
	if err != nil {
		return catalog{}, err
	}
	var response struct {
		Profiles []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"profiles"`
	}
	if json.Unmarshal(body, &response) != nil {
		return catalog{}, fmt.Errorf("Invalid JetBrains catalog")
	}
	c := catalog{Expires: time.Now().Add(5 * time.Minute), Routes: map[string]modelRoute{}}
	for _, profile := range response.Profiles {
		route, ok := modelMap[profile.ID]
		if !ok {
			// Gemini uses JetBrains' aggregate endpoint; embedding and completion
			// profiles are excluded because this executor serves conversational APIs.
			if !strings.HasPrefix(profile.ID, "google-") || strings.Contains(profile.ID, "embedding") {
				continue
			}
			route = modelRoute{Family: "aggregate", Native: profile.ID}
		}
		route.Profile = profile.ID
		id := "jetbrains/" + route.Native
		if _, exists := c.Routes[id]; exists {
			continue
		}
		c.Routes[id] = route
		c.Models = append(c.Models, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: "jetbrains", Type: "jetbrains", Name: id, DisplayName: route.Native})
	}
	if len(c.Models) == 0 {
		return c, fmt.Errorf("JetBrains returned no conversational models")
	}
	catalogMu.Lock()
	catalogs[s.Email] = c
	catalogMu.Unlock()
	return c, nil
}

type execution struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id"`
	Callback string `json:"host_callback_id"`
}

func formatRoute(route modelRoute, source translator.Format) (translator.Format, string) {
	responses := translator.FormatOpenAIResponse
	if source != translator.FormatOpenAIResponse {
		// The SDK calls its Responses upstream transformer "codex"; the
		// "openai-response" format names the incoming client protocol.
		responses = translator.FormatCodex
	}
	switch route.Family {
	case "anthropic":
		return translator.FormatClaude, "/user/v5/llm/anthropic/v1/messages"
	case "xai":
		return responses, "/user/v5/llm/xai/v1/responses"
	case "aggregate":
		return translator.FormatOpenAI, "/user/v5/llm/chat/stream/v8"
	default:
		if source != translator.FormatOpenAIResponse && !strings.Contains(route.Native, "codex") {
			return translator.FormatOpenAI, "/user/v5/llm/openai/v1/chat/completions"
		}
		return responses, "/user/v5/llm/openai/v1/responses"
	}
}
func translateRequest(from, to translator.Format, model string, body []byte, stream bool) ([]byte, error) {
	var out []byte
	if from == to {
		out = body
	} else if registry.HasRequestTransformer(from, to) {
		out = registry.TranslateRequest(from, to, model, body, stream)
	} else {
		middle := translator.FormatOpenAI
		if from == middle || to == middle || !registry.HasRequestTransformer(from, middle) || !registry.HasRequestTransformer(middle, to) {
			return nil, fmt.Errorf("Unsupported JetBrains request protocol %q to %q", from, to)
		}
		out = registry.TranslateRequest(middle, to, model, registry.TranslateRequest(from, middle, model, body, stream), stream)
	}
	var doc map[string]any
	if json.Unmarshal(out, &doc) != nil {
		return nil, fmt.Errorf("Invalid translated JetBrains request")
	}
	doc["model"] = model
	doc["stream"] = stream
	if stream && to == translator.FormatOpenAI {
		options, _ := doc["stream_options"].(map[string]any)
		if options == nil {
			options = map[string]any{}
		}
		options["include_usage"] = true
		doc["stream_options"] = options
	}
	return json.Marshal(doc)
}
func execute(method string, raw []byte) (any, error) {
	var req execution
	if json.Unmarshal(raw, &req) != nil {
		return nil, fmt.Errorf("Invalid execution request")
	}
	s, err := liveStorage(req.Callback, req.AuthID, req.StorageJSON)
	if err != nil {
		return nil, err
	}
	catalog, err := modelCatalog(req.Callback, s)
	if err != nil {
		return nil, err
	}
	route, ok := catalog.Routes[req.Model]
	if !ok {
		return nil, &statusError{404, "unknown model"}
	}
	stream := method == pluginabi.MethodExecutorExecuteStream
	source := translator.FromString(req.SourceFormat)
	if source == "" {
		source = translator.FromString(req.Format)
	}
	target, path := formatRoute(route, source)
	body, err := translateRequest(source, target, route.Native, req.Payload, stream)
	if err != nil {
		return nil, err
	}
	translated := body
	if route.Family == "aggregate" {
		body, err = aggregateRequest(body, route.Profile)
		if err != nil {
			return nil, err
		}
	}
	if !stream {
		response, err := checked(req.Callback, "model request", "POST", apiBase+path, apiHeaders(s.JWT), body)
		if err != nil {
			return nil, err
		}
		if route.Family == "aggregate" {
			response, err = aggregateResponse(response, req.Model)
			if err != nil {
				return nil, err
			}
		}
		if source != target {
			response = registry.TranslateNonStream(context.Background(), target, source, req.Model, req.OriginalRequest, translated, response, nil)
		}
		if !json.Valid(response) {
			return nil, fmt.Errorf("Invalid JetBrains model response")
		}
		return pluginapi.ExecutorResponse{Payload: response, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
	}
	if req.StreamID == "" {
		return nil, fmt.Errorf("Missing output stream")
	}
	upstream, err := host().OpenStream(context.Background(), req.Callback, framework.HTTPRequest{Method: "POST", URL: apiBase + path, Headers: apiHeaders(s.JWT), Body: body})
	switch {
	case errors.Is(err, framework.ErrHostResponse):
		return nil, fmt.Errorf("Invalid JetBrains stream")
	case err != nil:
		return nil, fmt.Errorf("JetBrains streaming transport failed")
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		_ = host().CloseStream(context.Background(), upstream.ID)
		status := upstream.StatusCode
		if status == 477 {
			status = 429
		}
		return nil, &statusError{status, "model stream"}
	}
	go pump(req, upstream.ID, source, target, translated, route.Family == "aggregate")
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}}, nil
}
func pump(req execution, input string, source, target translator.Format, translated []byte, aggregate bool) {
	failure := ""
	ctx := context.Background()
	h := host()
	defer func() {
		_ = h.CloseStream(ctx, input)
		h.CloseOutput(ctx, req.StreamID, failure)
	}()
	decoder := sse.Decoder{}
	var state any
	transform := newAggregateStream(req.Model)
	emit := func(frame []byte) bool {
		frames := [][]byte{frame}
		if aggregate {
			frames = transform.feed(frame)
			if transform.Error != "" {
				failure = transform.Error
				return false
			}
		}
		for _, frame := range frames {
			outputs := [][]byte{frame}
			if source != target {
				outputs = registry.TranslateStream(context.Background(), target, source, req.Model, req.OriginalRequest, translated, sse.DataFrame(frame), &state)
			}
			for _, out := range outputs {
				out = clientStreamPayload(source, out)
				if len(out) == 0 {
					continue
				}
				if err := h.Emit(ctx, req.StreamID, out); err != nil {
					return false
				}
			}
		}
		return true
	}
	for {
		chunk, err := h.ReadStream(ctx, input)
		if errors.Is(err, framework.ErrHostResponse) || (err == nil && chunk.Error != "") {
			failure = "JetBrains stream failed"
			return
		}
		if err != nil {
			failure = "JetBrains stream read failed"
			return
		}
		for _, frame := range decoder.Feed(chunk.Payload) {
			if !emit(frame) {
				return
			}
		}
		if chunk.Done {
			if tail := decoder.Flush(); len(tail) > 0 {
				if !emit(tail) {
					return
				}
			}
			if aggregate {
				_ = emit([]byte("data: end\n\n"))
			}
			return
		}
	}
}

// The Chat Completions handler adds its own SSE envelope and terminal marker.
// Messages and Responses handlers consume complete SSE frames instead.
func clientStreamPayload(format translator.Format, frame []byte) []byte {
	if format != translator.FormatOpenAI {
		return frame
	}
	return sse.DataPayload(frame)
}

// parseQuota reads the included allowance. Its "until" field is the
// subscription end, not the monthly reset, so reset dates come from
// parseRefill through /quota/metadata/refill instead.
func parseQuota(raw []byte) (float64, float64, error) {
	var doc struct {
		Current struct {
			Current struct {
				Amount json.RawMessage `json:"amount"`
			} `json:"current"`
			Maximum struct {
				Amount json.RawMessage `json:"amount"`
			} `json:"maximum"`
			Tariff json.RawMessage `json:"tariffQuota"`
		} `json:"current"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return 0, 0, fmt.Errorf("Invalid JetBrains quota")
	}
	current := doc.Current.Current.Amount
	maximum := doc.Current.Maximum.Amount
	if len(doc.Current.Tariff) > 0 && string(doc.Current.Tariff) != "null" {
		var tariff struct {
			Current struct {
				Amount json.RawMessage `json:"amount"`
			} `json:"current"`
			Maximum struct {
				Amount json.RawMessage `json:"amount"`
			} `json:"maximum"`
		}
		if json.Unmarshal(doc.Current.Tariff, &tariff) != nil {
			return 0, 0, fmt.Errorf("Invalid JetBrains tariff")
		}
		current = tariff.Current.Amount
		maximum = tariff.Maximum.Amount
	}
	used, e1 := quotaNumber(current)
	limit, e2 := quotaNumber(maximum)
	if e1 != nil || e2 != nil || math.IsNaN(used) || math.IsNaN(limit) || math.IsInf(used, 0) || math.IsInf(limit, 0) || used < 0 || limit < 0 {
		return 0, 0, fmt.Errorf("JetBrains did not return a measurable quota")
	}
	return used, limit, nil
}

// parseRefill returns the next monthly refill time from the quota metadata
// endpoint. Falling back to the refill period keeps the reset date available
// when JetBrains omits "next" but still reports the last refill and tariff.
func parseRefill(raw []byte) string {
	var doc struct {
		Current struct {
			Next   json.RawMessage `json:"next"`
			Last   json.RawMessage `json:"last"`
			Tariff struct {
				Period struct {
					Millis json.RawMessage `json:"millis"`
				} `json:"period"`
			} `json:"tariff"`
		} `json:"current"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	if next, ok := quotaTimestamp(doc.Current.Next); ok {
		return next.Format(time.RFC3339)
	}
	last, ok := quotaTimestamp(doc.Current.Last)
	if !ok {
		return ""
	}
	period, err := quotaNumber(doc.Current.Tariff.Period.Millis)
	if err != nil || period <= 0 || math.IsNaN(period) || math.IsInf(period, 0) {
		return ""
	}
	return time.UnixMilli(last.UnixMilli() + int64(period)).UTC().Format(time.RFC3339)
}

func quotaNumber(value json.RawMessage) (float64, error) {
	return strconv.ParseFloat(strings.Trim(string(value), "\""), 64)
}

// quotaTimestamp accepts both the epoch milliseconds the API model serializes
// and the RFC 3339 strings the IDE persists locally.
func quotaTimestamp(value json.RawMessage) (time.Time, bool) {
	if len(value) == 0 || string(value) == "null" {
		return time.Time{}, false
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return time.Time{}, false
		}
		return parsed.UTC(), true
	}
	millis, err := quotaNumber(value)
	if err != nil || millis <= 0 || math.IsNaN(millis) || math.IsInf(millis, 0) {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(millis)).UTC(), true
}
