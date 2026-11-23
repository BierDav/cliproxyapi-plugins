package provider

import (
	"encoding/json"
	"testing"
)

func TestAuthDataMergesAccountMetadata(t *testing.T) {
	metadata := map[string]any{"host_value": "preserved", "email": "stale@example.com"}
	data, err := authData(authStorage{
		Type:              providerID,
		GitHubLogin:       "octocat",
		GitHubEmail:       "user@example.com",
		GitHubAccessToken: "token",
	}, "auth-id", "copilot-octocat.json", "", "", false, metadata, nil)
	if err != nil {
		t.Fatalf("authData() error = %v", err)
	}

	if got := data.Metadata["email"]; got != "user@example.com" {
		t.Fatalf("metadata email = %#v, want %q", got, "user@example.com")
	}
	if got := data.Metadata["github_login"]; got != "octocat" {
		t.Fatalf("metadata github_login = %#v, want %q", got, "octocat")
	}
	if got := data.Metadata["host_value"]; got != "preserved" {
		t.Fatalf("metadata host_value = %#v, want %q", got, "preserved")
	}
	if data.Label != "user@example.com" {
		t.Fatalf("label = %q, want %q", data.Label, "user@example.com")
	}
	if metadata["email"] != "stale@example.com" {
		t.Fatalf("input metadata was mutated: %#v", metadata)
	}
}

func TestAuthStoragePreservesModelAliases(t *testing.T) {
	raw := []byte(`{
		"type": "copilot",
		"github_access_token": "token",
		"github_login": "octocat",
		"updated_at": "2026-11-07T00:00:00Z",
		"model_aliases": [
			{"name": "gpt-6.1-sol", "alias": "gpt-6.1-sol-codex-team", "fork": true}
		]
	}`)

	storage, err := parseStorage(raw)
	if err != nil {
		t.Fatalf("parseStorage() error = %v", err)
	}
	encoded, err := marshalStorage(storage)
	if err != nil {
		t.Fatalf("marshalStorage() error = %v", err)
	}

	var got struct {
		ModelAliases []modelAlias `json:"model_aliases"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decode marshaled storage: %v", err)
	}
	if len(got.ModelAliases) != 1 || got.ModelAliases[0].Alias != "gpt-6.1-sol-codex-team" {
		t.Fatalf("model_aliases = %#v, want preserved account alias", got.ModelAliases)
	}
}
