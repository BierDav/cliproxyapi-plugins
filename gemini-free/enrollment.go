package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	loginLifetime = 10 * time.Minute
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"
	userEndpoint  = "https://openidconnect.googleapis.com/v1/userinfo"
)

type enrollment struct {
	mu       sync.Mutex
	expires  time.Time
	config   configuration
	verifier string
	authDir  string
	cloud    setup
	done     *pluginapi.AuthLoginPollResponse
}

func (cfg configuration) redirect() string { return cfg.PublicOrigin + "/v0/management/oauth-callback" }

func (p *providerPlugin) start(r rpc) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.config
	if cfg.ClientID == "" || cfg.PublicOrigin == "" || clientSecret() == "" {
		return nil, failure(http.StatusServiceUnavailable, "Configure the Google OAuth web client ID, public origin, and gateway client secret before enrolling Gemini Free")
	}
	now := time.Now()
	for state, session := range p.pending {
		if now.After(session.expires) {
			delete(p.pending, state)
			_ = os.Remove(callbackFile(session.authDir, state))
		}
	}
	if len(p.pending) >= 32 {
		return nil, failure(http.StatusTooManyRequests, "Too many Gemini Free enrollments in progress")
	}
	state := hex.EncodeToString(random(16))
	verifier := base64.RawURLEncoding.EncodeToString(random(32))
	challenge := sha256.Sum256([]byte(verifier))
	session := &enrollment{expires: now.Add(loginLifetime), config: cfg, verifier: verifier, authDir: framework.ResolveAuthDir(r.Host.AuthDir)}
	p.pending[state] = session
	query := url.Values{
		"client_id": {cfg.ClientID}, "redirect_uri": {cfg.redirect()}, "response_type": {"code"},
		"scope": {"openid email https://www.googleapis.com/auth/cloud-platform"},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"}, "access_type": {"online"}, "prompt": {"select_account"},
	}
	// Vertex AI models are called as the signed-in user, so the gateway keeps
	// a refresh token. Google issues one only for offline access with consent.
	if len(p.catalog.vertex) > 0 {
		query.Set("access_type", "offline")
		query.Set("prompt", "consent select_account")
	}
	return pluginapi.AuthLoginStartResponse{Provider: provider, State: state, URL: authEndpoint + "?" + query.Encode(), ExpiresAt: session.expires}, nil
}

func random(size int) []byte {
	out := make([]byte, size)
	_, _ = rand.Read(out) // Go 1.26 rand.Read never returns an error.
	return out
}

func callbackFile(dir, state string) string {
	return filepath.Join(dir, ".oauth-"+provider+"-"+state+".oauth")
}

func (p *providerPlugin) forget(state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, state)
}

func (p *providerPlugin) poll(r rpc) (any, error) {
	pending := pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}
	if !statePattern.MatchString(r.State) {
		return nil, failure(http.StatusBadRequest, "Invalid Gemini Free enrollment state")
	}
	p.mu.Lock()
	session := p.pending[r.State]
	p.mu.Unlock()
	if session == nil {
		return nil, failure(http.StatusBadRequest, "Gemini Free enrollment expired; start sign-in again")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if time.Now().After(session.expires) {
		p.forget(r.State)
		_ = os.Remove(callbackFile(session.authDir, r.State))
		return nil, failure(http.StatusBadRequest, "Gemini Free enrollment expired; start sign-in again")
	}
	if session.done != nil {
		return *session.done, nil
	}
	if session.cloud.token == "" {
		path := callbackFile(session.authDir, r.State)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return pending, nil
		}
		_ = os.Remove(path)
		if err != nil {
			p.forget(r.State)
			return nil, failure(http.StatusInternalServerError, "Could not read the Google OAuth callback")
		}
		if err := p.exchange(r.HostCallbackID, r.State, session, data); err != nil {
			p.forget(r.State)
			return nil, err
		}
	}
	c, err := session.cloud.advance(p, r.HostCallbackID)
	if err != nil {
		session.cloud = setup{}
		p.forget(r.State)
		return nil, err
	}
	if c == nil {
		return pending, nil
	}
	session.cloud = setup{}
	session.done = &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: c.auth()}
	return *session.done, nil
}

func (p *providerPlugin) exchange(callback, state string, session *enrollment, data []byte) error {
	var result struct{ Code, State, Error string }
	if json.Unmarshal(data, &result) != nil || result.State != state || result.Code == "" || result.Error != "" {
		return failure(http.StatusUnauthorized, "Google did not authorize Gemini Free enrollment")
	}
	form := url.Values{
		"code": {result.Code}, "code_verifier": {session.verifier}, "client_id": {session.config.ClientID},
		"client_secret": {clientSecret()}, "grant_type": {"authorization_code"}, "redirect_uri": {session.config.redirect()},
	}
	session.verifier = ""
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := p.call(callback, "exchange the Google OAuth code", framework.HTTPRequest{
		Method: http.MethodPost, URL: tokenEndpoint,
		Headers: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, Body: []byte(form.Encode()),
	}, &tokens); err != nil {
		return err
	}
	if tokens.AccessToken == "" {
		return failure(http.StatusBadGateway, "Google did not return an OAuth access token")
	}
	session.cloud = setup{token: tokens.AccessToken, clientID: session.config.ClientID, phase: identify}
	if c, err := parseCatalog(session.config); err == nil && len(c.vertex) > 0 {
		session.cloud.refresh = tokens.RefreshToken
	}
	return nil
}
