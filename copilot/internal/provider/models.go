package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/copilot/internal/redact"
	"github.com/BierDav/cliproxyapi-plugins/copilot/internal/translate"
	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type modelListResponse struct {
	Data []upstreamModel `json:"data"`
}

type upstreamModel struct {
	ID                  string            `json:"id"`
	Vendor              string            `json:"vendor"`
	Name                string            `json:"name"`
	Version             string            `json:"version"`
	Object              string            `json:"object"`
	ModelPickerEnabled  bool              `json:"model_picker_enabled"`
	Preview             bool              `json:"preview"`
	SupportedEndpoints  []string          `json:"supported_endpoints"`
	WarningMessages     []modelMessage    `json:"warning_messages"`
	InformationMessages []modelMessage    `json:"info_messages"`
	Capabilities        modelCapabilities `json:"capabilities"`
	Policy              modelPolicy       `json:"policy"`
}

type modelPolicy struct {
	State string `json:"state"`
}

type modelMessage struct {
	Message string `json:"message"`
}

type modelCapabilities struct {
	Type      string        `json:"type"`
	Tokenizer string        `json:"tokenizer"`
	Family    string        `json:"family"`
	Object    string        `json:"object"`
	Supports  modelSupports `json:"supports"`
	Limits    modelLimits   `json:"limits"`
}

type modelSupports struct {
	ToolCalls         bool     `json:"tool_calls"`
	ParallelToolCalls bool     `json:"parallel_tool_calls"`
	Streaming         bool     `json:"streaming"`
	Vision            bool     `json:"vision"`
	AdaptiveThinking  bool     `json:"adaptive_thinking"`
	ReasoningEffort   []string `json:"reasoning_effort"`
}

type modelLimits struct {
	MaxInputs                   int64 `json:"max_inputs"`
	MaxPromptTokens             int64 `json:"max_prompt_tokens"`
	MaxOutputTokens             int64 `json:"max_output_tokens"`
	MaxNonStreamingOutputTokens int64 `json:"max_non_streaming_output_tokens"`
	MaxContextWindowTokens      int64 `json:"max_context_window_tokens"`
}

type modelCacheEntry struct {
	Fingerprint string
	APIBaseURL  string
	ExpiresAt   time.Time
	Models      []upstreamModel
	ByID        map[string]upstreamModel
}

func (s *Service) StaticModels() pluginapi.ModelResponse {
	return pluginapi.ModelResponse{Provider: providerID, Models: []pluginapi.ModelInfo{}}
}

func (s *Service) ModelsForAuth(ctx context.Context, callbackID string, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return pluginapi.ModelResponse{}, errParse
	}
	models, _, errModels := s.models(ctx, callbackID, req.AuthID, storage, false)
	if errModels != nil {
		return pluginapi.ModelResponse{}, errModels
	}
	aliases := storage.ModelAliases
	models, aliases = s.withoutRefusedModels(req.AuthID, models, aliases)
	models = append(models, fusionModel())
	return pluginapi.ModelResponse{Provider: providerID, Models: modelInfosWithAliases(models, aliases)}, nil
}

func (s *Service) models(ctx context.Context, callbackID, authID string, storage authStorage, force bool) ([]upstreamModel, copilotTokenEntry, error) {
	token, errToken := s.copilotToken(ctx, callbackID, authID, storage)
	if errToken != nil {
		return nil, copilotTokenEntry{}, errToken
	}
	fingerprint := tokenFingerprint(storage.GitHubAccessToken)
	key := strings.TrimSpace(authID)
	if key == "" {
		key = fingerprint
	}
	now := s.now()
	if !force {
		s.modelMu.Lock()
		cached, ok := s.modelEntries[key]
		s.modelMu.Unlock()
		if ok && cached.Fingerprint == fingerprint && cached.APIBaseURL == token.APIBaseURL && cached.ExpiresAt.After(now) {
			return cloneUpstreamModels(cached.Models), token, nil
		}
	}

	resp, errDo := s.host.Do(ctx, callbackID, framework.HTTPRequest{
		Method:  http.MethodGet,
		URL:     token.APIBaseURL + "/models",
		Headers: copilotHeaders(token.Token, false),
	})
	if errDo != nil {
		return nil, copilotTokenEntry{}, fmt.Errorf("discover Copilot models: %w", errDo)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.invalidateAuth(authID)
		token, errToken = s.copilotToken(ctx, callbackID, authID, storage)
		if errToken != nil {
			return nil, copilotTokenEntry{}, errToken
		}
		resp, errDo = s.host.Do(ctx, callbackID, framework.HTTPRequest{
			Method:  http.MethodGet,
			URL:     token.APIBaseURL + "/models",
			Headers: copilotHeaders(token.Token, false),
		})
		if errDo != nil {
			return nil, copilotTokenEntry{}, fmt.Errorf("discover Copilot models after token refresh: %w", errDo)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, copilotTokenEntry{}, upstreamStatusError(resp.StatusCode, redact.ErrorBody(resp.Body, token.Token, storage.GitHubAccessToken))
	}
	var list modelListResponse
	if errUnmarshal := json.Unmarshal(resp.Body, &list); errUnmarshal != nil {
		return nil, copilotTokenEntry{}, fmt.Errorf("decode Copilot models response: %w", errUnmarshal)
	}
	cleaned := filterModels(normalizeModels(list.Data), s.Config().ExcludedModelPrefixes)
	if len(cleaned) == 0 {
		return nil, copilotTokenEntry{}, fmt.Errorf("Copilot models endpoint returned no usable models")
	}

	byID := make(map[string]upstreamModel, len(cleaned))
	for _, model := range cleaned {
		byID[strings.ToLower(model.ID)] = model
	}
	s.modelMu.Lock()
	s.modelEntries[key] = modelCacheEntry{
		Fingerprint: fingerprint,
		APIBaseURL:  token.APIBaseURL,
		ExpiresAt:   now.Add(s.Config().modelCacheTTL()),
		Models:      cloneUpstreamModels(cleaned),
		ByID:        byID,
	}
	s.modelMu.Unlock()
	return cleaned, token, nil
}

// refusedModelTTL is how long a model GitHub refused for an account stays out
// of that account's catalog. The catalog lists models a plan does not include
// (for example premium models on a free educational plan), and requests for
// them fail with model_not_supported.
const refusedModelTTL = 12 * time.Hour

func refusedModelKey(authID, model string) string {
	return strings.TrimSpace(authID) + "\n" + strings.ToLower(strings.TrimSpace(model))
}

// noteModelRefusal remembers that GitHub refused model for authID when body
// is a model_not_supported error.
func (s *Service) noteModelRefusal(authID, model string, body []byte) {
	if strings.TrimSpace(authID) == "" || strings.TrimSpace(model) == "" || strings.EqualFold(model, "auto") || !modelNotSupported(body) {
		return
	}
	s.modelMu.Lock()
	s.unsupported[refusedModelKey(authID, model)] = s.now().Add(refusedModelTTL)
	s.modelMu.Unlock()
}

// withoutRefusedModels drops the models GitHub refused for authID, and the
// aliases that name them.
func (s *Service) withoutRefusedModels(authID string, models []upstreamModel, aliases []modelAlias) ([]upstreamModel, []modelAlias) {
	now := s.now()
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	refused := func(model string) bool {
		key := refusedModelKey(authID, model)
		until, ok := s.unsupported[key]
		if ok && !now.Before(until) {
			delete(s.unsupported, key)
			return false
		}
		return ok
	}
	keptModels := models[:0:0]
	for _, model := range models {
		if !refused(model.ID) {
			keptModels = append(keptModels, model)
		}
	}
	keptAliases := aliases[:0:0]
	for _, alias := range aliases {
		if !refused(alias.Name) {
			keptAliases = append(keptAliases, alias)
		}
	}
	return keptModels, keptAliases
}

func (s *Service) endpointForModel(ctx context.Context, callbackID, authID string, storage authStorage, modelID string) (string, copilotTokenEntry, error) {
	if resolveModelAlias(storage, modelID) == "auto" {
		token, err := s.copilotToken(ctx, callbackID, authID, storage)
		return translate.EndpointResponses, token, err
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "", copilotTokenEntry{}, statusError("invalid_request", "model is required", http.StatusBadRequest)
	}
	modelID = resolveModelAlias(storage, modelID)
	models, token, errModels := s.models(ctx, callbackID, authID, storage, false)
	if errModels != nil {
		if endpoint, ok := specialResponsesModel(modelID); ok {
			token, errToken := s.copilotToken(ctx, callbackID, authID, storage)
			return endpoint, token, errToken
		}
		return "", copilotTokenEntry{}, errModels
	}
	for _, model := range models {
		if strings.EqualFold(model.ID, modelID) {
			endpoint, errEndpoint := selectEndpoint(model)
			return endpoint, token, errEndpoint
		}
	}
	if endpoint, ok := specialResponsesModel(modelID); ok {
		return endpoint, token, nil
	}
	return "", token, statusError("model_not_found", "Copilot model is not present in the authenticated model catalog", http.StatusNotFound)
}

func selectEndpoint(model upstreamModel) (string, error) {
	if endpoint, ok := specialResponsesModel(model.ID); ok {
		return endpoint, nil
	}
	endpoints := normalizeEndpoints(model.SupportedEndpoints)
	for _, preferred := range []string{translate.EndpointResponses, translate.EndpointChatCompletions, translate.EndpointMessages} {
		for _, endpoint := range endpoints {
			if endpoint == preferred {
				return preferred, nil
			}
		}
	}
	return "", statusError("unsupported_model_endpoint", "Copilot model exposes no supported chat endpoint", http.StatusUnprocessableEntity)
}

func fusionModel() upstreamModel {
	return upstreamModel{
		ID: "auto", Name: "Copilot Fusion", Vendor: "GitHub", Object: "model",
		SupportedEndpoints:  []string{translate.EndpointResponses},
		InformationMessages: []modelMessage{{Message: "GitHub selects a model authorized for this account through Copilot Fusion."}},
		Capabilities: modelCapabilities{
			Type: "chat", Family: "auto",
			Supports: modelSupports{Streaming: true, ToolCalls: true, ParallelToolCalls: true, Vision: true},
			Limits:   modelLimits{MaxContextWindowTokens: 128000, MaxPromptTokens: 96000, MaxOutputTokens: 32000},
		},
	}
}

func specialResponsesModel(modelID string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "gpt-5.4-nano",
		"gpt-5.6-luna",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-6-astra",
		"gpt-6-luna",
		"gpt-6-sol",
		"gpt-6.1-sol",
		"gpt-6.2-sol":
		return translate.EndpointResponses, true
	default:
		return "", false
	}
}

func normalizeModels(models []upstreamModel) []upstreamModel {
	seen := make(map[string]struct{}, len(models))
	out := make([]upstreamModel, 0, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		key := strings.ToLower(model.ID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		model.Name = strings.TrimSpace(model.Name)
		model.Vendor = strings.TrimSpace(model.Vendor)
		model.Version = strings.TrimSpace(model.Version)
		model.Object = strings.TrimSpace(model.Object)
		if !servesChat(model) {
			continue
		}
		model.SupportedEndpoints = normalizeEndpoints(model.SupportedEndpoints)
		if len(model.SupportedEndpoints) == 0 {
			// Older chat models such as gpt-4o-mini list no endpoints and
			// are served by Chat Completions.
			model.SupportedEndpoints = []string{translate.EndpointChatCompletions}
		}
		if endpoint, ok := specialResponsesModel(model.ID); ok && !contains(model.SupportedEndpoints, endpoint) {
			model.SupportedEndpoints = append(model.SupportedEndpoints, endpoint)
		}
		out = append(out, model)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].ID) < strings.ToLower(out[j].ID)
	})
	return out
}

// servesChat reports whether a catalog entry is a chat model the account may
// use. Embedding and completion models, and models disabled by the account's
// Copilot policy, cannot serve requests through this executor.
func servesChat(model upstreamModel) bool {
	if kind := strings.ToLower(strings.TrimSpace(model.Capabilities.Type)); kind != "" && kind != "chat" {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(model.Policy.State), "disabled")
}

func filterModels(models []upstreamModel, excludedPrefixes []string) []upstreamModel {
	if len(excludedPrefixes) == 0 {
		return models
	}
	out := make([]upstreamModel, 0, len(models))
	for _, model := range models {
		modelID := strings.ToLower(model.ID)
		excluded := false
		for _, prefix := range excludedPrefixes {
			if strings.HasPrefix(modelID, prefix) {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, model)
		}
	}
	return out
}

func normalizeEndpoints(endpoints []string) []string {
	seen := make(map[string]struct{}, len(endpoints))
	out := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		switch strings.ToLower(strings.TrimSpace(endpoint)) {
		case "/responses", "responses", "openai-responses":
			endpoint = translate.EndpointResponses
		case "/chat/completions", "chat", "chat-completions":
			endpoint = translate.EndpointChatCompletions
		case "/v1/messages", "messages", "anthropic":
			endpoint = translate.EndpointMessages
		default:
			continue
		}
		if _, exists := seen[endpoint]; exists {
			continue
		}
		seen[endpoint] = struct{}{}
		out = append(out, endpoint)
	}
	return out
}

func modelInfos(models []upstreamModel) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		owner := model.Vendor
		if owner == "" {
			owner = "github-copilot"
		}
		displayName := model.Name
		if displayName == "" {
			displayName = model.ID
		}
		contextLength := model.Capabilities.Limits.MaxContextWindowTokens
		if contextLength == 0 {
			contextLength = model.Capabilities.Limits.MaxPromptTokens + model.Capabilities.Limits.MaxOutputTokens
		}
		parameters := []string{}
		if model.Capabilities.Supports.Streaming {
			parameters = append(parameters, "stream")
		}
		if model.Capabilities.Supports.ToolCalls {
			parameters = append(parameters, "tools", "tool_choice")
		}
		if model.Capabilities.Supports.ParallelToolCalls {
			parameters = append(parameters, "parallel_tool_calls")
		}
		if len(model.Capabilities.Supports.ReasoningEffort) > 0 {
			parameters = append(parameters, "reasoning_effort")
		}
		inputModalities := []string{"TEXT"}
		if model.Capabilities.Supports.Vision {
			inputModalities = append(inputModalities, "IMAGE")
		}
		var thinking *pluginapi.ThinkingSupport
		if model.Capabilities.Supports.AdaptiveThinking || len(model.Capabilities.Supports.ReasoningEffort) > 0 {
			thinking = &pluginapi.ThinkingSupport{
				DynamicAllowed: model.Capabilities.Supports.AdaptiveThinking,
				Levels:         append([]string(nil), model.Capabilities.Supports.ReasoningEffort...),
			}
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         model.ID,
			Object:                     firstNonEmpty(model.Object, "model"),
			OwnedBy:                    owner,
			Type:                       firstNonEmpty(model.Capabilities.Type, "chat"),
			DisplayName:                displayName,
			Name:                       model.ID,
			Version:                    model.Version,
			Description:                modelDescription(model),
			InputTokenLimit:            model.Capabilities.Limits.MaxPromptTokens,
			OutputTokenLimit:           model.Capabilities.Limits.MaxOutputTokens,
			SupportedGenerationMethods: append([]string(nil), model.SupportedEndpoints...),
			ContextLength:              contextLength,
			MaxCompletionTokens:        model.Capabilities.Limits.MaxOutputTokens,
			SupportedParameters:        parameters,
			SupportedInputModalities:   inputModalities,
			SupportedOutputModalities:  []string{"TEXT"},
			Thinking:                   thinking,
		})
	}
	return out
}

func modelInfosWithAliases(models []upstreamModel, aliases []modelAlias) []pluginapi.ModelInfo {
	out := modelInfos(models)
	byID := make(map[string]pluginapi.ModelInfo, len(out))
	for _, model := range out {
		byID[strings.ToLower(model.ID)] = model
	}
	for _, alias := range aliases {
		name := strings.TrimSpace(alias.Name)
		aliasID := strings.TrimSpace(alias.Alias)
		if name == "" || aliasID == "" || strings.EqualFold(name, aliasID) {
			continue
		}
		model, ok := byID[strings.ToLower(name)]
		if !ok {
			if endpoint, special := specialResponsesModel(name); !special {
				continue
			} else {
				model = pluginapi.ModelInfo{
					ID:                         name,
					Object:                     "model",
					OwnedBy:                    "github-copilot",
					Type:                       "chat",
					DisplayName:                name,
					Name:                       name,
					SupportedGenerationMethods: []string{endpoint},
					SupportedInputModalities:   []string{"TEXT"},
					SupportedOutputModalities:  []string{"TEXT"},
				}
			}
		}
		model.ID = aliasID
		model.Name = aliasID
		if strings.TrimSpace(alias.DisplayName) != "" {
			model.DisplayName = alias.DisplayName
		}
		out = append(out, model)
	}
	return out
}

func resolveModelAlias(storage authStorage, modelID string) string {
	modelID = strings.TrimSpace(modelID)
	for _, alias := range storage.ModelAliases {
		if strings.EqualFold(strings.TrimSpace(alias.Alias), modelID) {
			if name := strings.TrimSpace(alias.Name); name != "" {
				return name
			}
		}
	}
	return modelID
}

func modelDescription(model upstreamModel) string {
	for _, item := range append(model.WarningMessages, model.InformationMessages...) {
		if message := strings.TrimSpace(item.Message); message != "" {
			return message
		}
	}
	parts := []string{"GitHub Copilot model"}
	if family := strings.TrimSpace(model.Capabilities.Family); family != "" {
		parts = append(parts, "family "+family)
	}
	if len(model.SupportedEndpoints) > 0 {
		parts = append(parts, "endpoints "+strings.Join(model.SupportedEndpoints, ", "))
	}
	return strings.Join(parts, "; ")
}

func cloneUpstreamModels(in []upstreamModel) []upstreamModel {
	out := make([]upstreamModel, len(in))
	copy(out, in)
	for i := range out {
		out[i].SupportedEndpoints = append([]string(nil), in[i].SupportedEndpoints...)
		out[i].Capabilities.Supports.ReasoningEffort = append([]string(nil), in[i].Capabilities.Supports.ReasoningEffort...)
		out[i].WarningMessages = append([]modelMessage(nil), in[i].WarningMessages...)
		out[i].InformationMessages = append([]modelMessage(nil), in[i].InformationMessages...)
	}
	return out
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
