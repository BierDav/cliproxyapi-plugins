package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/BierDav/cliproxyapi-plugins/framework/sse"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	EndpointResponses       = "/responses"
	EndpointChatCompletions = "/chat/completions"
	EndpointMessages        = "/v1/messages"
)

var registry = builtin.Registry()

func ClaudeToResponses(model string, body []byte, stream bool) ([]byte, error) {
	return request(sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, model, body, stream)
}

func ResponsesToClaude(ctx context.Context, model string, original, translated, body []byte) ([]byte, error) {
	return response(ctx, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, model, original, translated, body)
}

func RequestForEndpoint(model string, body []byte, stream bool, endpoint string) ([]byte, error) {
	return RequestForEndpointFrom(sdktranslator.FormatOpenAIResponse.String(), model, body, stream, endpoint)
}

func RequestForEndpointFrom(source, model string, body []byte, stream bool, endpoint string) ([]byte, error) {
	from := sdktranslator.FromString(source)
	to, err := upstreamFormat(endpoint, from)
	if err != nil {
		return nil, err
	}
	var out []byte
	if from == to {
		out = append([]byte(nil), body...)
	} else {
		out, err = request(from, to, model, body, stream)
	}
	if err != nil {
		return nil, err
	}
	if to == sdktranslator.FormatCodex {
		out, err = restoreChatSampling(body, out)
		if err != nil {
			return nil, err
		}
	}
	// Fusion can resolve "auto" to a model that rejects the client's reasoning
	// context. Use the model's default while preserving its other settings.
	if endpoint == EndpointResponses && model == "gpt-5.3-codex" && gjson.GetBytes(out, "reasoning.context").String() == "all_turns" {
		out, err = sjson.SetBytes(out, "reasoning.context", "auto")
		if err != nil {
			return nil, err
		}
	}
	return setModelAndStream(out, model, stream)
}

func ResponseToResponses(ctx context.Context, endpoint, model string, original, translated, body []byte) ([]byte, error) {
	return ResponseFromEndpoint(ctx, endpoint, sdktranslator.FormatOpenAIResponse.String(), model, original, translated, body)
}

func ResponseFromEndpoint(ctx context.Context, endpoint, destination, model string, original, translated, body []byte) ([]byte, error) {
	to := sdktranslator.FromString(destination)
	from, err := upstreamFormat(endpoint, to)
	if err != nil {
		return nil, err
	}
	if from == to {
		if !json.Valid(body) {
			return nil, fmt.Errorf("Copilot API returned invalid JSON")
		}
		return append([]byte(nil), body...), nil
	}
	return response(ctx, from, to, model, original, translated, body)
}

func StreamToResponses(ctx context.Context, endpoint, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	return StreamFromEndpoint(ctx, endpoint, sdktranslator.FormatOpenAIResponse.String(), model, original, translated, frame, state)
}

func StreamFromEndpoint(ctx context.Context, endpoint, destination, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	to := sdktranslator.FromString(destination)
	from, err := upstreamFormat(endpoint, to)
	if err != nil {
		return nil, err
	}
	if from == to {
		return [][]byte{append([]byte(nil), frame...)}, nil
	}
	return stream(ctx, from, to, model, original, translated, frame, state)
}

func ResponsesSSEToClaude(ctx context.Context, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	return stream(ctx, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, model, original, translated, frame, state)
}

func request(from, to sdktranslator.Format, model string, body []byte, stream bool) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("request body is not valid JSON")
	}
	if from == sdktranslator.FormatClaude && to == sdktranslator.FormatOpenAIResponse {
		return claudeRequestToResponses(model, body, stream)
	}
	if from != to && !registry.HasRequestTransformer(from, to) {
		intermediate, ok := intermediateFormat(from, to)
		if !ok {
			return nil, fmt.Errorf("official translator has no request route from %s to %s", from, to)
		}
		first := registry.TranslateRequest(from, intermediate, model, body, stream)
		out := registry.TranslateRequest(intermediate, to, model, first, stream)
		if len(out) == 0 || !json.Valid(out) {
			return nil, fmt.Errorf("official two-hop request translation from %s to %s failed", from, to)
		}
		return out, nil
	}
	out := registry.TranslateRequest(from, to, model, body, stream)
	if len(out) == 0 || !json.Valid(out) {
		return nil, fmt.Errorf("official request translation from %s to %s failed", from, to)
	}
	return out, nil
}

func response(ctx context.Context, from, to sdktranslator.Format, model string, original, translated, body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("response body is not valid JSON")
	}
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatClaude {
		return responsesResponseToClaude(model, body)
	}
	if from == sdktranslator.FormatCodex {
		// The Codex translators convert the terminal stream event, not the
		// bare response object a non-streaming /responses call returns.
		wrapped, err := json.Marshal(map[string]json.RawMessage{"type": json.RawMessage(`"response.completed"`), "response": body})
		if err != nil {
			return nil, err
		}
		body = wrapped
	}
	if from != to && !registry.HasNonStreamResponseTransformer(to, from) {
		intermediate, ok := intermediateFormat(to, from)
		if !ok {
			return nil, fmt.Errorf("official translator has no response route from %s to %s", from, to)
		}
		intermediateRequest := registry.TranslateRequest(to, intermediate, model, original, false)
		first := registry.TranslateNonStream(ctx, from, intermediate, model, intermediateRequest, translated, body, nil)
		out := registry.TranslateNonStream(ctx, intermediate, to, model, original, intermediateRequest, first, nil)
		if len(out) == 0 || !json.Valid(out) {
			return nil, fmt.Errorf("official two-hop response translation from %s to %s failed", from, to)
		}
		return out, nil
	}
	out := registry.TranslateNonStream(ctx, from, to, model, original, translated, body, nil)
	if len(out) == 0 || !json.Valid(out) {
		return nil, fmt.Errorf("official response translation from %s to %s failed", from, to)
	}
	return out, nil
}

func stream(ctx context.Context, from, to sdktranslator.Format, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatClaude {
		return responsesStreamToClaude(model, frame, state)
	}
	// The SDK translators read data lines only; Copilot's Responses stream
	// starts every frame with an event line.
	frame = sse.DataFrame(frame)
	if from != to && !registry.HasStreamResponseTransformer(to, from) {
		intermediate, ok := intermediateFormat(to, from)
		if !ok {
			return nil, fmt.Errorf("official translator has no stream route from %s to %s", from, to)
		}
		if state == nil {
			return nil, fmt.Errorf("two-hop stream translation requires state")
		}
		hopState, okState := (*state).(*twoHopStreamState)
		if !okState {
			hopState = &twoHopStreamState{}
			*state = hopState
		}
		intermediateRequest := registry.TranslateRequest(to, intermediate, model, original, true)
		first := registry.TranslateStream(ctx, from, intermediate, model, intermediateRequest, translated, frame, &hopState.First)
		var out [][]byte
		for _, firstFrame := range first {
			out = append(out, registry.TranslateStream(ctx, intermediate, to, model, original, intermediateRequest, firstFrame, &hopState.Second)...)
		}
		return out, nil
	}
	out := registry.TranslateStream(ctx, from, to, model, original, translated, frame, state)
	if len(out) == 1 && bytes.Equal(out[0], frame) && from != to {
		return nil, fmt.Errorf("official stream translation from %s to %s fell back unchanged", from, to)
	}
	return out, nil
}

type twoHopStreamState struct {
	First  any
	Second any
}

func intermediateFormat(from, to sdktranslator.Format) (sdktranslator.Format, bool) {
	for _, candidate := range []sdktranslator.Format{
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude,
	} {
		if candidate != from && candidate != to &&
			registry.HasRequestTransformer(from, candidate) &&
			registry.HasRequestTransformer(candidate, to) {
			return candidate, true
		}
	}
	return "", false
}

// upstreamFormat is the translator format of a Copilot endpoint as seen from a
// client format. Copilot's /responses speaks the OpenAI Responses wire format,
// which the SDK registers for Chat Completions clients only as Codex: there is
// no Chat Completions <-> openai-response route, so those clients go through
// the Codex translators.
func upstreamFormat(endpoint string, client sdktranslator.Format) (sdktranslator.Format, error) {
	format, err := endpointFormat(endpoint)
	if err == nil && format == sdktranslator.FormatOpenAIResponse && client == sdktranslator.FormatOpenAI {
		return sdktranslator.FormatCodex, nil
	}
	return format, err
}

// restoreChatSampling undoes the Codex request translator's choices that only
// suit the ChatGPT Codex backend: it drops the token limit and sampling
// parameters and always asks for medium reasoning, which Copilot's Responses
// API accepts as given.
func restoreChatSampling(chat, out []byte) ([]byte, error) {
	var err error
	limit := gjson.GetBytes(chat, "max_completion_tokens")
	if !limit.Exists() {
		limit = gjson.GetBytes(chat, "max_tokens")
	}
	if limit.Type == gjson.Number {
		out, err = sjson.SetBytes(out, "max_output_tokens", limit.Int())
	}
	for _, field := range []string{"temperature", "top_p"} {
		if v := gjson.GetBytes(chat, field); err == nil && v.Type == gjson.Number {
			out, err = sjson.SetRawBytes(out, field, []byte(v.Raw))
		}
	}
	if err == nil && !gjson.GetBytes(chat, "reasoning_effort").Exists() {
		out, err = sjson.DeleteBytes(out, "reasoning")
	}
	if err != nil {
		return nil, fmt.Errorf("restore Chat Completions parameters: %w", err)
	}
	return out, nil
}

func endpointFormat(endpoint string) (sdktranslator.Format, error) {
	switch endpoint {
	case EndpointResponses:
		return sdktranslator.FormatOpenAIResponse, nil
	case EndpointChatCompletions:
		return sdktranslator.FormatOpenAI, nil
	case EndpointMessages:
		return sdktranslator.FormatClaude, nil
	default:
		return "", fmt.Errorf("unsupported Copilot endpoint %q", endpoint)
	}
}

func setModelAndStream(body []byte, model string, streamEnabled bool) ([]byte, error) {
	var value map[string]any
	if errUnmarshal := json.Unmarshal(body, &value); errUnmarshal != nil {
		return nil, fmt.Errorf("decode translated request: %w", errUnmarshal)
	}
	value["model"] = model
	value["stream"] = streamEnabled
	out, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode translated request: %w", errMarshal)
	}
	return out, nil
}
