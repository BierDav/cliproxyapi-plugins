package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type authStorage struct {
	Type                  string       `json:"type"`
	GitHubAccessToken     string       `json:"github_access_token"`
	GitHubRefreshToken    string       `json:"github_refresh_token,omitempty"`
	TokenType             string       `json:"token_type,omitempty"`
	Scope                 string       `json:"scope,omitempty"`
	ExpiresAt             int64        `json:"expires_at,omitempty"`
	RefreshTokenExpiresAt int64        `json:"refresh_token_expires_at,omitempty"`
	GitHubLogin           string       `json:"github_login"`
	GitHubUserID          int64        `json:"github_user_id,omitempty"`
	GitHubEmail           string       `json:"github_email,omitempty"`
	OAuthClientID         string       `json:"oauth_client_id,omitempty"`
	UpdatedAt             string       `json:"updated_at"`
	ModelAliases          []modelAlias `json:"model_aliases,omitempty"`
}

type modelAlias struct {
	Name        string `json:"name"`
	Alias       string `json:"alias"`
	Fork        bool   `json:"fork,omitempty"`
	DisplayName string `json:"display-name,omitempty"`
}

func parseStorage(raw []byte) (authStorage, error) {
	var storage authStorage
	if len(raw) == 0 {
		return storage, fmt.Errorf("Copilot auth storage is empty")
	}
	if errUnmarshal := json.Unmarshal(raw, &storage); errUnmarshal != nil {
		return storage, fmt.Errorf("decode Copilot auth storage: %w", errUnmarshal)
	}
	if !strings.EqualFold(strings.TrimSpace(storage.Type), providerID) {
		return storage, fmt.Errorf("auth type is not %s", providerID)
	}
	storage.GitHubAccessToken = strings.TrimSpace(storage.GitHubAccessToken)
	storage.GitHubRefreshToken = strings.TrimSpace(storage.GitHubRefreshToken)
	storage.GitHubLogin = strings.TrimSpace(storage.GitHubLogin)
	storage.OAuthClientID = strings.TrimSpace(storage.OAuthClientID)
	if storage.GitHubAccessToken == "" {
		return storage, fmt.Errorf("Copilot auth storage has no GitHub access token")
	}
	return storage, nil
}

func marshalStorage(storage authStorage) ([]byte, error) {
	return json.Marshal(storage)
}

func authData(storage authStorage, id, fileName, prefix, proxyURL string, disabled bool, metadata map[string]any, attributes map[string]string) (pluginapi.AuthData, error) {
	raw, errMarshal := marshalStorage(storage)
	if errMarshal != nil {
		return pluginapi.AuthData{}, fmt.Errorf("encode Copilot auth storage: %w", errMarshal)
	}
	if fileName == "" {
		fileName = credentialFileName(storage.GitHubLogin)
	}
	if id == "" {
		id = fileName
	}
	mergedMetadata := make(map[string]any, len(metadata)+3)
	for key, value := range metadata {
		mergedMetadata[key] = value
	}
	email := strings.TrimSpace(storage.GitHubEmail)
	if email == "" {
		email = strings.TrimSpace(storage.GitHubLogin)
	}
	mergedMetadata["type"] = providerID
	mergedMetadata["github_login"] = strings.TrimSpace(storage.GitHubLogin)
	mergedMetadata["email"] = email
	if len(attributes) == 0 {
		attributes = map[string]string{"auth_kind": "oauth"}
	}
	nextRefresh := nextGitHubRefresh(storage, time.Now())
	label := strings.TrimSpace(storage.GitHubEmail)
	if label == "" {
		label = strings.TrimSpace(storage.GitHubLogin)
	}
	return pluginapi.AuthData{
		Provider:         providerID,
		ID:               id,
		FileName:         fileName,
		Label:            label,
		Prefix:           prefix,
		ProxyURL:         proxyURL,
		Disabled:         disabled,
		StorageJSON:      raw,
		Metadata:         mergedMetadata,
		Attributes:       attributes,
		NextRefreshAfter: nextRefresh,
	}, nil
}

func nextGitHubRefresh(storage authStorage, now time.Time) time.Time {
	if storage.ExpiresAt > 0 {
		refreshAt := time.Unix(storage.ExpiresAt, 0).Add(-10 * time.Minute)
		if refreshAt.Before(now.Add(time.Minute)) {
			return now.Add(time.Minute)
		}
		return refreshAt
	}
	return now.Add(24 * time.Hour)
}

func credentialFileName(login string) string {
	login = strings.ToLower(strings.TrimSpace(login))
	var clean strings.Builder
	for _, r := range login {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			clean.WriteRune(r)
		case r == '-', r == '_', r == '.':
			clean.WriteRune(r)
		default:
			clean.WriteByte('-')
		}
	}
	name := strings.Trim(clean.String(), "-.")
	if name == "" {
		name = "account"
	}
	return "copilot-" + name + ".json"
}

func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
