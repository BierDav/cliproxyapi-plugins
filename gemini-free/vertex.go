package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const vertexAPI = "https://aiplatform.googleapis.com/v1/"

// vertexRequest calls a Google model at the global endpoint, the only one
// that serves the newest Gemini models.
func vertexRequest(token, project, model, action string, body []byte) framework.HTTPRequest {
	return framework.HTTPRequest{
		Method:  http.MethodPost,
		URL:     vertexAPI + "projects/" + project + "/locations/global/publishers/google/models/" + model + ":" + action,
		Headers: http.Header{"Authorization": {"Bearer " + token}, "Content-Type": {"application/json"}},
		Body:    body,
	}
}

// vertexAccessToken returns a cached access token for the credential's
// refresh token, refreshing it with Google when it is about to expire.
func (p *providerPlugin) vertexAccessToken(cb string, c credential) (string, error) {
	sum := sha256.Sum256([]byte(c.Refresh))
	id := hex.EncodeToString(sum[:])
	p.mu.Lock()
	cached, cfg := p.tokens[id], p.config
	p.mu.Unlock()
	if time.Until(cached.expires) > time.Minute {
		return cached.value, nil
	}
	if cfg.ClientID == "" || clientSecret() == "" {
		return "", failure(http.StatusServiceUnavailable, "Configure the Google OAuth client before using Vertex AI models")
	}
	form := url.Values{"client_id": {cfg.ClientID}, "client_secret": {clientSecret()},
		"grant_type": {"refresh_token"}, "refresh_token": {c.Refresh}}
	var tokens struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	err := p.call(cb, "refresh the Vertex AI sign-in", framework.HTTPRequest{
		Method: http.MethodPost, URL: tokenEndpoint,
		Headers: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, Body: []byte(form.Encode()),
	}, &tokens)
	var google *googleError
	if errors.As(err, &google) && (google.status == http.StatusBadRequest || google.status == http.StatusUnauthorized) {
		return "", failure(http.StatusUnauthorized, "Google revoked the Vertex AI sign-in for "+c.Email+"; enroll the account again")
	}
	if err != nil {
		return "", err
	}
	if tokens.AccessToken == "" || tokens.ExpiresIn <= 0 {
		return "", failure(http.StatusBadGateway, "Google did not return a Vertex AI access token")
	}
	p.mu.Lock()
	p.tokens[id] = accessToken{value: tokens.AccessToken, expires: time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)}
	p.mu.Unlock()
	return tokens.AccessToken, nil
}

// thinkingLevel turns a numeric thinking budget into the level that current
// Gemini models require; they reject thinkingBudget. The thresholds are the
// host's (internal/thinking/convert.go), limited to the levels the plugin
// declares. Dynamic (-1) and zero budgets keep the model's default level.
func thinkingLevel(body []byte) []byte {
	for _, field := range []string{"thinkingBudget", "thinking_budget"} {
		path := "generationConfig.thinkingConfig." + field
		budget := gjson.GetBytes(body, path)
		if !budget.Exists() {
			continue
		}
		body, _ = sjson.DeleteBytes(body, path)
		if gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingLevel").Exists() ||
			gjson.GetBytes(body, "generationConfig.thinkingConfig.thinking_level").Exists() {
			continue
		}
		level := ""
		switch b := budget.Int(); {
		case b <= 0:
		case b <= 1024:
			level = "low"
		case b <= 8192:
			level = "medium"
		default:
			level = "high"
		}
		if level != "" {
			body, _ = sjson.SetBytes(body, "generationConfig.thinkingConfig.thinkingLevel", level)
		}
	}
	return body
}

// vertexBody adapts a Gemini API request to Vertex AI, which requires a role
// on every content entry.
func vertexBody(body []byte) []byte {
	for i, content := range gjson.GetBytes(body, "contents").Array() {
		if content.Get("role").String() == "" {
			body, _ = sjson.SetBytes(body, "contents."+strconv.Itoa(i)+".role", "user")
		}
	}
	return body
}

// vertexCountBody turns a Gemini API countTokens or generation request into
// a Vertex AI countTokens request, which takes the generation request's
// contents, instructions, tools and generation config at the top level.
func vertexCountBody(body []byte) []byte {
	if inner := gjson.GetBytes(body, "generateContentRequest"); inner.IsObject() {
		body = []byte(inner.Raw)
	}
	for _, field := range []string{"model", "safetySettings", "safety_settings", "toolConfig", "tool_config", "cachedContent", "cached_content", "labels"} {
		body, _ = sjson.DeleteBytes(body, field)
	}
	return vertexBody(body)
}
