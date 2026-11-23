package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/BierDav/cliproxyapi-plugins/copilot/internal/redact"
	"github.com/BierDav/cliproxyapi-plugins/copilot/internal/translate"
	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/BierDav/cliproxyapi-plugins/framework/sse"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const (
	copilotUserAgent     = "GitHubCopilotChat/0.35.0"
	copilotEditorVersion = "vscode/1.107.0"
	copilotPluginVersion = "copilot-chat/0.35.0"
	copilotIntegrationID = "vscode-chat"
	copilotAPIVersion    = "2025-04-01"
	responsesAPIVersion  = "2026-08-01"
)

type ExecuteRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type HTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (pluginapi.ExecutorResponse, error) {
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return pluginapi.ExecutorResponse{}, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return pluginapi.ExecutorResponse{}, errParse
	}
	endpoint, token, requestBody, errEndpoint := s.prepareModelRequest(ctx, req, sourceFormat, storage, false)
	if errEndpoint != nil {
		return pluginapi.ExecutorResponse{}, errEndpoint
	}
	resp, token, errDo := s.doModelRequest(ctx, req.HostCallbackID, req.AuthID, storage, token, endpoint, requestBody, false)
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	body, errResponse := translate.ResponseFromEndpoint(ctx, endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, resp.Body)
	if errResponse != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", errResponse.Error(), http.StatusBadGateway)
	}
	return pluginapi.ExecutorResponse{
		Payload: body,
		Headers: filterResponseHeaders(resp.Headers),
		Metadata: map[string]any{
			"copilot_endpoint": endpoint,
			"copilot_model":    gjson.GetBytes(requestBody, "model").String(),
			"token_expires_at": token.ExpiresAt.UTC().Format(http.TimeFormat),
		},
	}, nil
}

func (s *Service) ExecuteStream(ctx context.Context, req ExecuteRequest) (http.Header, error) {
	if strings.TrimSpace(req.StreamID) == "" {
		return nil, statusError("invalid_request", "stream_id is required", http.StatusBadRequest)
	}
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return nil, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}
	endpoint, token, requestBody, errEndpoint := s.prepareModelRequest(ctx, req, sourceFormat, storage, true)
	if errEndpoint != nil {
		return nil, errEndpoint
	}
	upstream, token, errOpen := s.openModelStream(ctx, req.HostCallbackID, req.AuthID, storage, token, endpoint, requestBody)
	if errOpen != nil {
		return nil, errOpen
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		body, errCollect := s.collectStreamError(ctx, upstream)
		if errCollect != nil {
			return nil, errCollect
		}
		s.noteModelRefusal(req.AuthID, gjson.GetBytes(requestBody, "model").String(), body)
		return nil, upstreamStatusError(upstream.StatusCode, redact.ErrorBody(body, token.Token, token.SessionToken, storage.GitHubAccessToken))
	}
	go s.pumpStream(req.StreamID, endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, upstream)
	headers := filterResponseHeaders(upstream.Headers)
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	return headers, nil
}

func (s *Service) doModelRequest(ctx context.Context, callbackID, authID string, storage authStorage, token copilotTokenEntry, endpoint string, body []byte, stream bool) (framework.HTTPResponse, copilotTokenEntry, error) {
	request := framework.HTTPRequest{
		Method:  http.MethodPost,
		URL:     token.APIBaseURL + endpoint,
		Headers: modelRequestHeaders(token, stream, endpoint),
		Body:    body,
	}
	resp, errDo := s.host.Do(ctx, callbackID, request)
	if errDo != nil {
		return framework.HTTPResponse{}, token, fmt.Errorf("call Copilot model endpoint: %w", errDo)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		refreshed, errToken := s.refreshModelToken(ctx, callbackID, authID, storage, token, body)
		if errToken != nil {
			return framework.HTTPResponse{}, token, errToken
		}
		token = refreshed
		request.URL = token.APIBaseURL + endpoint
		request.Headers = modelRequestHeaders(token, stream, endpoint)
		resp, errDo = s.host.Do(ctx, callbackID, request)
		if errDo != nil {
			return framework.HTTPResponse{}, token, fmt.Errorf("call Copilot model endpoint after token refresh: %w", errDo)
		}
	}
	if endpoint == translate.EndpointResponses && resp.StatusCode == http.StatusBadRequest {
		if token.SessionToken != "" {
			if retryBody := fusionReasoningContextFallback(body, resp.Body); retryBody != nil {
				return s.doModelRequest(ctx, callbackID, authID, storage, token, endpoint, retryBody, stream)
			}
		} else if modelNotSupported(resp.Body) {
			_, token, errDo = s.fusionSession(ctx, callbackID, authID, storage, token, gjson.GetBytes(body, "model").String(), body)
			if errDo != nil {
				return resp, token, errDo
			}
			return s.doModelRequest(ctx, callbackID, authID, storage, token, endpoint, body, stream)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.noteModelRefusal(authID, gjson.GetBytes(body, "model").String(), resp.Body)
		return resp, token, upstreamStatusError(resp.StatusCode, redact.ErrorBody(resp.Body, token.Token, token.SessionToken, storage.GitHubAccessToken))
	}
	return resp, token, nil
}

func (s *Service) openModelStream(ctx context.Context, callbackID, authID string, storage authStorage, token copilotTokenEntry, endpoint string, body []byte) (framework.HTTPStream, copilotTokenEntry, error) {
	request := framework.HTTPRequest{
		Method:  http.MethodPost,
		URL:     token.APIBaseURL + endpoint,
		Headers: modelRequestHeaders(token, true, endpoint),
		Body:    body,
	}
	stream, errOpen := s.host.OpenStream(ctx, callbackID, request)
	if errOpen != nil {
		return framework.HTTPStream{}, token, fmt.Errorf("open Copilot model stream: %w", errOpen)
	}
	if stream.StatusCode == http.StatusUnauthorized {
		_ = s.host.CloseStream(ctx, stream.ID)
		refreshed, errToken := s.refreshModelToken(ctx, callbackID, authID, storage, token, body)
		if errToken != nil {
			return framework.HTTPStream{}, token, errToken
		}
		token = refreshed
		request.URL = token.APIBaseURL + endpoint
		request.Headers = modelRequestHeaders(token, true, endpoint)
		stream, errOpen = s.host.OpenStream(ctx, callbackID, request)
		if errOpen != nil {
			return framework.HTTPStream{}, token, fmt.Errorf("open Copilot model stream after token refresh: %w", errOpen)
		}
	}
	if endpoint == translate.EndpointResponses && stream.StatusCode == http.StatusBadRequest {
		errorBody, err := s.collectStreamError(ctx, stream)
		if err != nil {
			return framework.HTTPStream{}, token, err
		}
		model := gjson.GetBytes(body, "model").String()
		if token.SessionToken != "" {
			if retryBody := fusionReasoningContextFallback(body, errorBody); retryBody != nil {
				return s.openModelStream(ctx, callbackID, authID, storage, token, endpoint, retryBody)
			}
		} else if modelNotSupported(errorBody) {
			_, token, err = s.fusionSession(ctx, callbackID, authID, storage, token, model, body)
			if err != nil {
				return framework.HTTPStream{}, token, err
			}
			return s.openModelStream(ctx, callbackID, authID, storage, token, endpoint, body)
		}
		s.noteModelRefusal(authID, model, errorBody)
		return framework.HTTPStream{}, token, upstreamStatusError(stream.StatusCode, redact.ErrorBody(errorBody, token.Token, token.SessionToken, storage.GitHubAccessToken))
	}
	return stream, token, nil
}

func (s *Service) collectStreamError(ctx context.Context, stream framework.HTTPStream) ([]byte, error) {
	defer func() { _ = s.host.CloseStream(context.Background(), stream.ID) }()
	var body []byte
	for {
		chunk, errRead := s.host.ReadStream(ctx, stream.ID)
		if errRead != nil {
			return nil, fmt.Errorf("read Copilot error stream: %w", errRead)
		}
		body = append(body, chunk.Payload...)
		if chunk.Error != "" {
			return nil, fmt.Errorf("read Copilot error stream: %s", redact.Text(chunk.Error))
		}
		if chunk.Done {
			return body, nil
		}
	}
}

func (s *Service) pumpStream(outputID, endpoint, destination, model string, original, translated []byte, upstream framework.HTTPStream) {
	ctx := context.Background()
	var terminalErr error
	defer func() {
		_ = s.host.CloseStream(ctx, upstream.ID)
		message := ""
		if terminalErr != nil {
			message = redact.Text(terminalErr.Error())
		}
		s.host.CloseOutput(ctx, outputID, message)
	}()

	decoder := &sse.Decoder{}
	var state any
	emit := func(frame []byte) error {
		frames, errTranslate := translate.StreamFromEndpoint(ctx, endpoint, destination, model, original, translated, frame, &state)
		if errTranslate != nil {
			return errTranslate
		}
		for _, output := range frames {
			if destination == "openai" {
				// The host's Chat Completions handler adds the SSE envelope
				// and the [DONE] marker itself.
				output = sse.DataPayload(output)
			}
			if len(output) == 0 {
				continue
			}
			if errEmit := s.host.Emit(ctx, outputID, output); errEmit != nil {
				return errEmit
			}
		}
		return nil
	}

	for {
		chunk, errRead := s.host.ReadStream(ctx, upstream.ID)
		if errRead != nil {
			terminalErr = fmt.Errorf("read Copilot stream: %w", errRead)
			return
		}
		if chunk.Error != "" {
			terminalErr = fmt.Errorf("Copilot stream error: %s", redact.Text(chunk.Error))
			return
		}
		for _, frame := range decoder.Feed(chunk.Payload) {
			if errEmit := emit(frame); errEmit != nil {
				terminalErr = fmt.Errorf("translate Copilot stream: %w", errEmit)
				return
			}
		}
		if chunk.Done {
			if trailing := decoder.Flush(); len(trailing) > 0 {
				if errEmit := emit(trailing); errEmit != nil {
					terminalErr = fmt.Errorf("translate final Copilot stream frame: %w", errEmit)
				}
			}
			return
		}
	}
}

func normalizeRequestFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "responses", "openai-response", "openai-responses":
		return "openai-response"
	case "openai", "chat", "chat-completions":
		return "openai"
	case "claude", "anthropic":
		return "claude"
	default:
		return ""
	}
}

func (s *Service) HTTP(ctx context.Context, req HTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return pluginapi.ExecutorHTTPResponse{}, errParse
	}
	token, errToken := s.copilotToken(ctx, req.HostCallbackID, req.AuthID, storage)
	if errToken != nil {
		return pluginapi.ExecutorHTTPResponse{}, errToken
	}
	target, errURL := url.Parse(strings.TrimSpace(req.URL))
	if errURL != nil || target.Hostname() == "" {
		return pluginapi.ExecutorHTTPResponse{}, statusError("invalid_request", "executor HTTP URL is invalid", http.StatusBadRequest)
	}
	base, _ := url.Parse(token.APIBaseURL)
	if !strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Host, base.Host) {
		return pluginapi.ExecutorHTTPResponse{}, statusError("forbidden_url", "executor HTTP URL is outside the authenticated Copilot API origin", http.StatusForbidden)
	}
	headers := cloneHeader(req.Headers)
	headers.Set("Authorization", "Bearer "+token.Token)
	headers.Set("User-Agent", copilotUserAgent)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
	resp, errDo := s.host.Do(ctx, req.HostCallbackID, framework.HTTPRequest{
		Method:  req.Method,
		URL:     target.String(),
		Headers: headers,
		Body:    append([]byte(nil), req.Body...),
	})
	if errDo != nil {
		return pluginapi.ExecutorHTTPResponse{}, errDo
	}
	return pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    filterResponseHeaders(resp.Headers),
		Body:       resp.Body,
	}, nil
}

func copilotHeaders(token string, stream bool) http.Header {
	return copilotHeadersForEndpoint(token, stream, "")
}

func copilotHeadersForEndpoint(token string, stream bool, endpoint string) http.Header {
	requestID, _ := randomIdentifier(16)
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	headers := http.Header{}
	headers.Set("Accept", accept)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Content-Type", "application/json")
	headers.Set("Copilot-Integration-Id", copilotIntegrationID)
	headers.Set("Editor-Plugin-Version", copilotPluginVersion)
	headers.Set("Editor-Version", copilotEditorVersion)
	headers.Set("User-Agent", copilotUserAgent)
	headers.Set("X-Agent-Task-Id", requestID)
	headers.Set("X-Initiator", "user")
	headers.Set("X-Request-Id", requestID)
	if endpoint == translate.EndpointResponses {
		headers.Set("OpenAI-Intent", "conversation-edits")
		headers.Set("X-GitHub-Api-Version", responsesAPIVersion)
		headers.Set("X-Interaction-Type", "conversation-edits")
		headers.Set("X-Initiator", "agent")
	} else {
		headers.Set("OpenAI-Intent", "conversation-edits")
		headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
		headers.Set("X-Interaction-Type", "conversation-edits")
	}
	return headers
}

func filterResponseHeaders(headers http.Header) http.Header {
	out := http.Header{}
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "content-type", "cache-control", "retry-after", "x-github-request-id", "x-request-id":
			out[key] = append([]string(nil), values...)
		}
	}
	return out
}

func cloneHeader(headers http.Header) http.Header {
	if len(headers) == 0 {
		return http.Header{}
	}
	return headers.Clone()
}
