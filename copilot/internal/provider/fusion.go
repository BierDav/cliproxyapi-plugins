package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/BierDav/cliproxyapi-plugins/copilot/internal/redact"
	"github.com/BierDav/cliproxyapi-plugins/copilot/internal/translate"
	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Fusion grants access through a per-request session token, not the account
// token or the public model catalogue. Never substitute a different model for
// an explicit model request.
func (s *Service) fusionSession(ctx context.Context, callbackID, authID string, storage authStorage, token copilotTokenEntry, requested string, body []byte) (upstreamModel, copilotTokenEntry, error) {
	prompt, hasImage := fusionPrompt(body)
	payload, _ := json.Marshal(map[string]any{
		"prompt": prompt, "has_image": hasImage,
		"multi_turn":          map[string]any{"routing_intent": "anchor", "turns_since_anchor": 0},
		"hydra_rl_multi_turn": map[string]any{},
	})
	request := framework.HTTPRequest{Method: http.MethodPost, URL: token.APIBaseURL + "/auto", Headers: fusionHeaders(token.Token, false), Body: payload}
	resp, err := s.host.Do(ctx, callbackID, request)
	if err != nil {
		return upstreamModel{}, token, fmt.Errorf("create Copilot Fusion session: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.invalidateAuth(authID)
		token, err = s.copilotToken(ctx, callbackID, authID, storage)
		if err != nil {
			return upstreamModel{}, token, err
		}
		request.URL = token.APIBaseURL + "/auto"
		request.Headers = fusionHeaders(token.Token, false)
		resp, err = s.host.Do(ctx, callbackID, request)
		if err != nil {
			return upstreamModel{}, token, fmt.Errorf("create Copilot Fusion session after token refresh: %w", err)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return upstreamModel{}, token, upstreamStatusError(resp.StatusCode, redact.ErrorBody(resp.Body, token.Token, storage.GitHubAccessToken))
	}
	var session struct {
		SessionToken string        `json:"session_token"`
		Model        upstreamModel `json:"selected_model"`
	}
	if err := json.Unmarshal(resp.Body, &session); err != nil {
		return upstreamModel{}, token, fmt.Errorf("decode Copilot Fusion session: %w", err)
	}
	if strings.TrimSpace(session.SessionToken) == "" || strings.TrimSpace(session.Model.ID) == "" {
		return upstreamModel{}, token, statusError("fusion_unavailable", "Copilot Fusion returned no authorized model or session token", http.StatusBadGateway)
	}
	if requested != "auto" && !strings.EqualFold(requested, session.Model.ID) {
		return upstreamModel{}, token, statusError("model_not_available_for_plan", fmt.Sprintf("Copilot Fusion authorized %s, but this account cannot select %s. Select Copilot Fusion (auto) to let GitHub choose an available model.", session.Model.ID, requested), http.StatusForbidden)
	}
	token.SessionToken = session.SessionToken
	return session.Model, token, nil
}

func fusionHeaders(token string, stream bool) http.Header {
	headers := copilotHeadersForEndpoint(token, stream, translate.EndpointResponses)
	headers.Set("Copilot-Integration-Id", "copilot-developer-cli")
	headers.Set("Copilot-Harness-Id", "copilot-sdk")
	headers.Set("Editor-Version", "copilot/1.0.105")
	headers.Del("Editor-Plugin-Version")
	headers.Set("User-Agent", "copilot/1.0.105")
	headers.Set("OpenAI-Intent", "conversation-agent")
	headers.Set("X-Interaction-Type", "conversation-user")
	headers.Set("X-Initiator", "user")
	return headers
}

func modelRequestHeaders(token copilotTokenEntry, stream bool, endpoint string) http.Header {
	if token.SessionToken == "" {
		return copilotHeadersForEndpoint(token.Token, stream, endpoint)
	}
	headers := fusionHeaders(token.Token, stream)
	headers.Set("Copilot-Session-Token", token.SessionToken)
	return headers
}

func modelNotSupported(body []byte) bool {
	return gjson.GetBytes(body, "error.code").String() == "model_not_supported"
}

// Copilot can validate an inference request against a different model than
// the one returned by the Fusion router. Retry only this rejected setting.
func fusionReasoningContextFallback(body, errorBody []byte) []byte {
	if gjson.GetBytes(body, "reasoning.context").String() != "all_turns" ||
		gjson.GetBytes(errorBody, "error.code").String() != "invalid_request_body" ||
		!strings.HasPrefix(gjson.GetBytes(errorBody, "error.message").String(), "Unsupported value: 'all_turns' is not supported with the 'gpt-5.3-codex' model.") {
		return nil
	}
	retryBody, err := sjson.SetBytes(body, "reasoning.context", "auto")
	if err != nil {
		return nil
	}
	return retryBody
}

// The classifier needs the latest user message, not system instructions or
// tool outputs. Images anywhere in the conversation affect routing.
func fusionPrompt(body []byte) (string, bool) {
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		return input.String(), false
	}
	var prompt string
	var hasImage bool
	for _, item := range input.Array() {
		content := item.Get("content")
		var parts []string
		if content.Type == gjson.String {
			parts = append(parts, content.String())
		} else {
			for _, part := range content.Array() {
				if part.Get("type").String() == "input_image" {
					hasImage = true
				}
				if part.Get("type").String() == "input_text" {
					parts = append(parts, part.Get("text").String())
				}
			}
		}
		if item.Get("role").String() == "user" {
			prompt = strings.Join(parts, "\n")
		}
	}
	return prompt, hasImage
}

func (s *Service) prepareModelRequest(ctx context.Context, req ExecuteRequest, source string, storage authStorage, stream bool) (string, copilotTokenEntry, []byte, error) {
	endpoint, token, err := s.endpointForModel(ctx, req.HostCallbackID, req.AuthID, storage, req.Model)
	if err != nil {
		return "", token, nil, err
	}
	model := resolveModelAlias(storage, req.Model)
	body, err := translate.RequestForEndpointFrom(source, model, req.Payload, stream, endpoint)
	if err != nil {
		return "", token, nil, statusError("translation_error", err.Error(), http.StatusUnprocessableEntity)
	}
	if model == "auto" {
		var selected upstreamModel
		selected, token, err = s.fusionSession(ctx, req.HostCallbackID, req.AuthID, storage, token, "auto", body)
		if err == nil {
			endpoint, err = selectEndpoint(selected)
		}
		if err != nil {
			return "", token, nil, err
		}
		body, err = translate.RequestForEndpointFrom(source, selected.ID, req.Payload, stream, endpoint)
		if err != nil {
			return "", token, nil, statusError("translation_error", err.Error(), http.StatusUnprocessableEntity)
		}
	}
	return endpoint, token, body, nil
}

func (s *Service) refreshModelToken(ctx context.Context, callbackID, authID string, storage authStorage, old copilotTokenEntry, body []byte) (copilotTokenEntry, error) {
	s.invalidateAuth(authID)
	token, err := s.copilotToken(ctx, callbackID, authID, storage)
	if err == nil && old.SessionToken != "" {
		_, token, err = s.fusionSession(ctx, callbackID, authID, storage, token, gjson.GetBytes(body, "model").String(), body)
	}
	return token, err
}
