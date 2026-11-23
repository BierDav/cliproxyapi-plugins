package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/copilot/internal/translate"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestSelectEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		model     upstreamModel
		want      string
		wantError bool
	}{
		{
			name:  "responses preferred",
			model: upstreamModel{ID: "model-a", SupportedEndpoints: []string{"/chat/completions", "/responses"}},
			want:  translate.EndpointResponses,
		},
		{
			name:  "messages fallback",
			model: upstreamModel{ID: "model-b", SupportedEndpoints: []string{"messages"}},
			want:  translate.EndpointMessages,
		},
		{
			name:  "sol forced to responses",
			model: upstreamModel{ID: "gpt-5.6-sol", SupportedEndpoints: []string{"/chat/completions"}},
			want:  translate.EndpointResponses,
		},
		{
			name:  "terra forced to responses",
			model: upstreamModel{ID: "GPT-5.6-TERRA"},
			want:  translate.EndpointResponses,
		},
		{
			name:  "luna forced to responses",
			model: upstreamModel{ID: "GPT-5.6-LUNA"},
			want:  translate.EndpointResponses,
		},
		{
			name:  "gpt-5.4 nano forced to responses",
			model: upstreamModel{ID: "gpt-5.4-nano"},
			want:  translate.EndpointResponses,
		},
		{
			name:  "gpt-6 astra forced to responses",
			model: upstreamModel{ID: "gpt-6-astra"},
			want:  translate.EndpointResponses,
		},
		{
			name:  "gpt-6 luna forced to responses",
			model: upstreamModel{ID: "gpt-6-luna"},
			want:  translate.EndpointResponses,
		},
		{
			name:  "gpt-6 sol forced to responses",
			model: upstreamModel{ID: "gpt-6-sol"},
			want:  translate.EndpointResponses,
		},
		{
			name:  "gpt-6.1 sol forced to responses",
			model: upstreamModel{ID: "gpt-6.1-sol"},
			want:  translate.EndpointResponses,
		},
		{
			name:      "unsupported",
			model:     upstreamModel{ID: "embedding-model", SupportedEndpoints: []string{"/embeddings"}},
			wantError: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectEndpoint(test.model)
			if test.wantError {
				if err == nil {
					t.Fatal("expected an endpoint selection error")
				}
				return
			}
			if err != nil {
				t.Fatalf("select endpoint: %v", err)
			}
			if got != test.want {
				t.Fatalf("endpoint = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeModelsAddsResponsesMetadata(t *testing.T) {
	t.Parallel()

	models := normalizeModels([]upstreamModel{
		{
			ID:                 "gpt-5.6-sol",
			SupportedEndpoints: []string{"/chat/completions"},
			Capabilities: modelCapabilities{
				Supports: modelSupports{Streaming: true, ToolCalls: true, Vision: true},
				Limits:   modelLimits{MaxPromptTokens: 100, MaxOutputTokens: 20},
			},
		},
		{
			ID:                 "gpt-5.6-luna",
			SupportedEndpoints: []string{"/chat/completions"},
		},
	})
	if len(models) != 2 {
		t.Fatalf("normalized models = %#v", models)
	}
	for _, model := range models {
		if !contains(model.SupportedEndpoints, translate.EndpointResponses) {
			t.Fatalf("responses endpoint was not added for %q: %#v", model.ID, model)
		}
	}
	var sol upstreamModel
	for _, model := range models {
		if model.ID == "gpt-5.6-sol" {
			sol = model
			break
		}
	}
	info := modelInfos([]upstreamModel{sol})[0]
	if !contains(info.SupportedGenerationMethods, translate.EndpointResponses) {
		t.Fatalf("model metadata omits responses endpoint: %#v", info.SupportedGenerationMethods)
	}
	if !contains(info.SupportedInputModalities, "IMAGE") {
		t.Fatalf("model metadata omits image support: %#v", info.SupportedInputModalities)
	}
}

func TestFilterModelsExcludesConfiguredPrefixes(t *testing.T) {
	t.Parallel()

	models := filterModels([]upstreamModel{
		{ID: "gpt-5.6-sol"},
		{ID: "claude-sonnet-5"},
		{ID: "Claude-Haiku-4.5"},
	}, []string{"claude-"})
	if len(models) != 1 || models[0].ID != "gpt-5.6-sol" {
		t.Fatalf("filtered models = %#v", models)
	}
}

func TestNormalizeModelPrefixes(t *testing.T) {
	t.Parallel()

	got := normalizeModelPrefixes([]string{" Claude- ", "claude-", "", "GPT-"})
	if len(got) != 2 || got[0] != "claude-" || got[1] != "gpt-" {
		t.Fatalf("normalized prefixes = %#v", got)
	}
}

func TestResolveModelAlias(t *testing.T) {
	t.Parallel()

	storage := authStorage{
		ModelAliases: []modelAlias{
			{Name: "gpt-6.1-sol", Alias: "gpt-6.1-sol-codex-team"},
		},
	}
	if got := resolveModelAlias(storage, "GPT-6.1-SOL-CODEX-TEAM"); got != "gpt-6.1-sol" {
		t.Fatalf("resolved model = %q, want %q", got, "gpt-6.1-sol")
	}
	if got := resolveModelAlias(storage, "gpt-6-luna"); got != "gpt-6-luna" {
		t.Fatalf("unmapped model = %q, want unchanged", got)
	}
}

func TestModelInfosWithAliases(t *testing.T) {
	t.Parallel()

	models := modelInfosWithAliases(
		[]upstreamModel{{ID: "gpt-6.1-sol", SupportedEndpoints: []string{"/responses"}}},
		[]modelAlias{{Name: "gpt-6.1-sol", Alias: "gpt-6.1-sol-codex-team", DisplayName: "Sol Codex"}},
	)
	if len(models) != 2 {
		t.Fatalf("model count = %d, want 2", len(models))
	}
	alias := models[1]
	if alias.ID != "gpt-6.1-sol-codex-team" || alias.Name != alias.ID || alias.DisplayName != "Sol Codex" {
		t.Fatalf("alias model = %#v", alias)
	}
}

func TestNormalizeModelsKeepsOnlyUsableChatModels(t *testing.T) {
	t.Parallel()

	models := normalizeModels([]upstreamModel{
		{ID: "gpt-4o-mini", Capabilities: modelCapabilities{Type: "chat"}},
		{ID: "text-embedding-3-small", Capabilities: modelCapabilities{Type: "embeddings"}},
		{ID: "gpt-41-copilot", Capabilities: modelCapabilities{Type: "completion"}},
		{ID: "claude-opus-5", SupportedEndpoints: []string{"/v1/messages"}, Capabilities: modelCapabilities{Type: "chat"}, Policy: modelPolicy{State: "disabled"}},
		{ID: "claude-sonnet-5", SupportedEndpoints: []string{"/v1/messages"}, Capabilities: modelCapabilities{Type: "chat"}, Policy: modelPolicy{State: "enabled"}},
	})
	if len(models) != 2 || models[0].ID != "claude-sonnet-5" || models[1].ID != "gpt-4o-mini" {
		t.Fatalf("normalized models = %#v", models)
	}
	// A chat model without endpoints is served by Chat Completions.
	if endpoint, err := selectEndpoint(models[1]); err != nil || endpoint != translate.EndpointChatCompletions {
		t.Fatalf("gpt-4o-mini endpoint = %q, %v", endpoint, err)
	}
}

func TestRefusedModelsLeaveTheAccountCatalogUntilTheyExpire(t *testing.T) {
	s, _, storage := newFusionTestService(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	refused := []byte(`{"error":{"code":"model_not_supported","message":"The requested model is not supported."}}`)
	s.noteModelRefusal("account", "gpt-5.4-nano", []byte(`{"error":{"code":"invalid_request"}}`))
	s.noteModelRefusal("account", "GPT-5.6-Luna", refused)
	s.noteModelRefusal("other", "gpt-6.1-sol", refused)
	catalog := func() []string {
		t.Helper()
		resp, err := s.ModelsForAuth(context.Background(), "", pluginapi.AuthModelRequest{AuthID: "account", StorageJSON: storage})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, model := range resp.Models {
			ids = append(ids, model.ID)
		}
		return ids
	}
	ids := strings.Join(catalog(), ",")
	if strings.Contains(ids, "gh/gpt-5.6-luna") || strings.Contains(ids, ",gpt-5.6-luna") || !strings.Contains(ids, "gpt-6.1-sol") || !strings.Contains(ids, "gpt-5.4-nano") {
		t.Fatalf("catalog after refusal = %s", ids)
	}
	now = now.Add(refusedModelTTL + time.Second)
	entry := s.modelEntries["account"]
	entry.ExpiresAt = now.Add(time.Hour)
	s.modelEntries["account"] = entry
	token := s.tokenEntries["account"]
	token.ExpiresAt = now.Add(time.Hour)
	s.tokenEntries["account"] = token
	if ids := strings.Join(catalog(), ","); !strings.Contains(ids, "gpt-5.6-luna") {
		t.Fatalf("refusal did not expire: %s", ids)
	}
}
