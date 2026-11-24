package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	authorizeURL = "https://www.make.com/oauth/v2/authorize"
	tokenURL     = "https://www.make.com/oauth/v2/token"
	// Make's MCP authorization server allows dynamic client registration, so
	// sign-in needs no manually approved OAuth client and no API key.
	registerURL = "https://www.make.com/oauth/v2/register/mcp"
	// Make's MCP tools check a "Build & Run" permission bundle: module_spec
	// needs apps:read, and scenario_create additionally needs the hook, data
	// store and data structure scopes even though the gateway uses none.
	// "openid email" puts the user's address in the ID token: Make's REST API
	// refuses MCP tokens, and its userinfo endpoint needs openid.
	oauthScopes = "mcp:use apps:read scenarios:read scenarios:write scenarios:run connections:read connections:write " +
		"hooks:read hooks:write datastores:read datastores:write udts:read udts:write teams:read organizations:read openid email"
	// Make access tokens live five minutes; refresh tokens six months and
	// rotate on every use.
	accessSkew      = 60 * time.Second
	refreshInterval = 12 * time.Hour
	loginLifetime   = 10 * time.Minute
)

type storage struct {
	Type             string `json:"type"`
	ClientID         string `json:"client_id"`
	ClientSecret     string `json:"client_secret,omitempty"`
	RedirectURI      string `json:"redirect_uri"`
	RefreshToken     string `json:"refresh_token"`
	AccessToken      string `json:"access_token,omitempty"`
	AccessExpires    int64  `json:"access_token_expires_at,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	Zone             string `json:"zone,omitempty"`
	OrganizationID   int64  `json:"organization_id"`
	OrganizationName string `json:"organization_name,omitempty"`
	// Email is the signed-in Make user's address. At the top level of the
	// credential file it is what the host's auth-file overview shows.
	Email          string `json:"email,omitempty"`
	TeamID         int64  `json:"team_id,omitempty"`
	ConnectionID   int64  `json:"connection_id,omitempty"`
	ScenarioID     int64  `json:"scenario_id,omitempty"`
	GatewayVersion int    `json:"gateway_version,omitempty"`
	// DisableCooling is the host's per-credential cooldown switch, read from
	// the credential file. Left false, a 429 (a paused organization, used-up
	// credits) puts this credential into cooldown, so the host moves on to
	// another Make organization instead of retrying the paused one. It is
	// written on every save: each refresh rotates the token and rewrites the
	// file, and a field missing here would be dropped.
	DisableCooling bool `json:"disable_cooling"`
	// RotatedAt orders copies of the same credential: refresh tokens rotate,
	// so the newest copy is the only usable one. (The host owns "updated_at".)
	RotatedAt int64 `json:"rotated_at"`
}

type loginSession struct {
	Verifier     string    `json:"verifier"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"`
	RedirectURI  string    `json:"redirect_uri"`
	AuthDir      string    `json:"auth_dir"`
	Expires      time.Time `json:"expires"`
}

var stateMu sync.Mutex
var sessions = map[string]loginSession{}
var loginMu sync.Mutex

// tokenMu serializes refreshes: a rotated refresh token invalidates the
// previous one, so two concurrent refreshes would lose the credential.
var tokenMu sync.Mutex
var tokenCache = map[string]storage{}

func credentialName(s storage) string {
	user := s.UserID
	if user == "" {
		user = "user"
	}
	return fmt.Sprintf("make-%d-%s.json", s.OrganizationID, sanitize(user))
}

func sanitize(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteByte('-')
		}
	}
	return out.String()
}

func authData(s storage, id string) pluginapi.AuthData {
	if id == "" {
		id = credentialName(s)
	}
	// The signed-in user's address names the account best; without one,
	// fall back to the organization.
	label := s.Email
	if label == "" && s.OrganizationName != "" {
		label = "Make " + s.OrganizationName
	} else if label == "" {
		label = "Make organization " + strconv.FormatInt(s.OrganizationID, 10)
	}
	// Short-lived access tokens are an in-memory concern; never persist them.
	persisted := s
	persisted.AccessToken = ""
	persisted.AccessExpires = 0
	metadata := map[string]any{"type": providerKey, "organization_id": s.OrganizationID, "team_id": s.TeamID}
	if s.Email != "" {
		metadata["email"] = s.Email
	}
	return pluginapi.AuthData{Provider: providerKey, ID: id, FileName: id, Label: label, StorageJSON: encode(persisted),
		Metadata:   metadata,
		Attributes: map[string]string{"auth_kind": "oauth"}, NextRefreshAfter: time.Now().Add(refreshInterval)}
}

func readStorage(raw []byte) (storage, error) {
	var s storage
	if json.Unmarshal(raw, &s) != nil || s.Type != providerKey || s.RefreshToken == "" || s.ClientID == "" {
		return s, newStatusError(401, "Invalid Make credential")
	}
	return s, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("Secure random source unavailable")
	}
	return b
}

// tokenClaims reads the unverified access-token payload. The token was just
// received from Make's token endpoint over TLS; only routing metadata is used.
type tokenClaims struct {
	Subject        string `json:"sub"`
	OrganizationID int64  `json:"organization_id"`
	ZoneDomain     string `json:"zone_domain"`
	Expires        int64  `json:"exp"`
}

func readClaims(token string) tokenClaims {
	var claims tokenClaims
	pieces := strings.Split(token, ".")
	if len(pieces) != 3 {
		return claims
	}
	raw, err := base64.RawURLEncoding.DecodeString(pieces[1])
	if err == nil {
		_ = json.Unmarshal(raw, &claims)
	}
	return claims
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"`
	Error        string `json:"error"`
	// Make answers some failures with its own error body instead of OAuth's,
	// for example {"code":"SC404","message":"Refresh token not found"}.
	Message string `json:"message"`
}

// oauthHeaders are the headers of requests that create or rotate credentials.
// Make rotates the refresh token on every use, so a rotation the host cannot
// store loses the credential. While a gateway replica hands over, its drain
// fence refuses the auth store and new outbound connections but not pooled
// ones; Connection: close keeps these requests off pooled connections, so a
// draining replica fails before Make rotates anything.
func oauthHeaders(contentType string) http.Header {
	return http.Header{"Content-Type": {contentType}, "Accept": {"application/json"}, "Connection": {"close"}}
}

// credentialGone reports a Make error that means the stored client or refresh
// token no longer exists, which only a new sign-in fixes.
func credentialGone(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "refresh token not found") || strings.Contains(message, "client not found")
}

func requestToken(callback, operation string, form url.Values) (tokenResponse, error) {
	var tokens tokenResponse
	response, err := httpCall(callback, "POST", tokenURL, oauthHeaders("application/x-www-form-urlencoded"), []byte(form.Encode()))
	if err != nil {
		return tokens, err
	}
	_ = json.Unmarshal(response.Body, &tokens)
	if response.StatusCode < 200 || response.StatusCode >= 300 || tokens.AccessToken == "" {
		status := response.StatusCode
		if status >= 200 && status < 300 {
			status = 502
		}
		detail := ""
		switch tokens.Error {
		case "invalid_request", "invalid_client", "invalid_grant", "unauthorized_client", "unsupported_grant_type", "invalid_scope":
			detail = " (" + tokens.Error + ")"
		}
		if tokens.Error == "invalid_grant" || tokens.Error == "invalid_client" {
			return tokens, newStatusError(401, "Make %s failed%s; sign in to Make again", operation, detail)
		}
		if credentialGone(tokens.Message) {
			return tokens, newStatusError(401, "Make %s failed (%s); sign in to Make again", operation, truncate(strings.TrimSpace(tokens.Message), 100))
		}
		return tokens, newStatusError(status, "Make %s failed (HTTP %d)%s", operation, response.StatusCode, detail)
	}
	return tokens, nil
}

func applyTokens(s *storage, tokens tokenResponse) {
	s.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		s.RefreshToken = tokens.RefreshToken
	}
	expires := time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	if tokens.ExpiresIn <= 0 {
		expires = time.Now().Add(5 * time.Minute)
	}
	if claims := readClaims(tokens.AccessToken); claims.Expires > 0 {
		expires = time.Unix(claims.Expires, 0)
		if claims.OrganizationID > 0 {
			s.OrganizationID = claims.OrganizationID
		}
		if claims.Subject != "" {
			s.UserID = claims.Subject
		}
		if claims.ZoneDomain != "" {
			s.Zone = strings.TrimPrefix(claims.ZoneDomain, "https://")
		}
	}
	// Like the access token, the ID token comes straight from Make's token
	// endpoint over TLS, and only its address is used.
	if tokens.IDToken != "" {
		var id struct {
			Email string `json:"email"`
		}
		if pieces := strings.Split(tokens.IDToken, "."); len(pieces) == 3 {
			if raw, err := base64.RawURLEncoding.DecodeString(pieces[1]); err == nil && json.Unmarshal(raw, &id) == nil {
				if email := strings.TrimSpace(id.Email); strings.Contains(email, "@") {
					s.Email = email
				}
			}
		}
	}
	s.AccessExpires = expires.UnixMilli()
	s.RotatedAt = time.Now().UnixMilli()
}

func persist(name string, s storage) error {
	persisted := s
	persisted.AccessToken = ""
	persisted.AccessExpires = 0
	if err := host().SaveAuth(name, encode(persisted)); err != nil {
		return fmt.Errorf("Cannot persist Make credential")
	}
	return nil
}

func cacheKey(id string, s storage) string {
	if id != "" && filepath.Base(id) == id {
		return id
	}
	return credentialName(s)
}

// liveStorage returns the newest copy of a credential with a usable access
// token, refreshing and persisting the rotated refresh token when needed.
func liveStorage(callback, id string, raw []byte, force bool) (storage, string, error) {
	s, err := readStorage(raw)
	if err != nil {
		return s, "", err
	}
	key := cacheKey(id, s)
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if cached, ok := tokenCache[key]; ok && cached.RotatedAt >= s.RotatedAt {
		s = cached
	}
	if !force && s.AccessToken != "" && time.UnixMilli(s.AccessExpires).After(time.Now().Add(accessSkew)) {
		return s, key, nil
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.RefreshToken}, "client_id": {s.ClientID}}
	if s.ClientSecret != "" {
		form.Set("client_secret", s.ClientSecret)
	}
	tokens, err := requestToken(callback, "token refresh", form)
	if err != nil {
		return s, key, err
	}
	applyTokens(&s, tokens)
	tokenCache[key] = s
	// The previous refresh token is now invalid; persist before using the new one.
	if err := persist(key, s); err != nil {
		return s, key, err
	}
	return s, key, nil
}

// updateStorage records non-token changes (for example a re-provisioned
// gateway) on the newest cached copy and persists them.
func updateStorage(key string, change func(*storage)) (storage, error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	s := tokenCache[key]
	change(&s)
	s.RotatedAt = time.Now().UnixMilli()
	tokenCache[key] = s
	return s, persist(key, s)
}

// zoneHost reports whether a zone from the access token is a Make API host,
// so the token is never sent anywhere else.
func zoneHost(zone string) bool {
	if zone == "" || strings.ContainsAny(zone, "/:@?#") {
		return false
	}
	return strings.HasSuffix(zone, ".make.com") || strings.HasSuffix(zone, ".make.celonis.com")
}

const userinfoURL = "https://www.make.com/oauth/v2/oidc/userinfo"

// fetchEmail finds the signed-in user's address. Make's API in the
// credential's zone (GET /api/v2/users/me) and its OpenID userinfo endpoint
// come first; the MCP server, which always accepts the sign-in's token, is the
// last resort. The address is only shown in the auth-file overview, so
// failures are logged and leave it empty.
func fetchEmail(callback string, s storage) string {
	if s.AccessToken == "" {
		return ""
	}
	var failures []string
	if zoneHost(s.Zone) {
		email, failure := lookupEmail(callback, s, "https://"+s.Zone+"/api/v2/users/me")
		if email != "" {
			return email
		}
		failures = append(failures, failure)
	}
	email, failure := lookupEmail(callback, s, userinfoURL)
	if email != "" {
		return email
	}
	failures = append(failures, failure)
	email, failure = mcpEmail(callback, s)
	if email != "" {
		return email
	}
	failures = append(failures, failure)
	// The host log shows the message, not its fields, so the details go in it.
	host().Log(callback, "warn", fmt.Sprintf("Make user email lookup failed for organization %d (signing in again grants the openid scope that carries it): %s",
		s.OrganizationID, strings.Join(failures, "; ")), nil)
	return ""
}

// lookupEmail reads {"authUser":{"email":…}} (Make API) or {"email":…}
// (OpenID userinfo) from address, or says why it could not.
func lookupEmail(callback string, s storage, address string) (string, string) {
	headers := http.Header{"Authorization": {"Bearer " + s.AccessToken}, "Accept": {"application/json"}}
	response, err := httpCall(callback, "GET", address, headers, nil)
	if err != nil {
		return "", address + ": " + err.Error()
	}
	var me struct {
		AuthUser struct {
			Email string `json:"email"`
		} `json:"authUser"`
		Email   string `json:"email"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(response.Body, &me)
	email := strings.TrimSpace(me.AuthUser.Email)
	if email == "" {
		email = strings.TrimSpace(me.Email)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 && email != "" {
		return email, ""
	}
	// Make's error names the problem and holds no credentials.
	return "", fmt.Sprintf("%s: HTTP %d %s", address, response.StatusCode, truncate(strings.TrimSpace(me.Message+" "+me.Error), 160))
}

// mcpEmail looks for the address in environment_get, then in a read-only
// user tool of Make's MCP server.
func mcpEmail(callback string, s storage) (string, string) {
	if raw, err := callTool(callback, s.AccessToken, "environment_get", map[string]any{}); err == nil {
		if email := findEmail(raw); email != "" {
			return email, ""
		}
	}
	tools, err := listTools(callback, s.AccessToken)
	if err != nil {
		return "", "MCP tools/list: " + truncate(err.Error(), 160)
	}
	for _, tool := range tools {
		if !userTool(tool) {
			continue
		}
		if raw, err := callTool(callback, s.AccessToken, tool, map[string]any{}); err == nil {
			if email := findEmail(raw); email != "" {
				return email, ""
			}
		}
	}
	return "", "MCP: no email in environment_get or a user tool; tools: " + truncate(strings.Join(tools, ","), 600)
}

// userTool reports whether an MCP tool reads the current user, judged by the
// words of its name (users_me, current_user, whoami); such tools change
// nothing.
func userTool(name string) bool {
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
	user, current := false, false
	for _, word := range words {
		switch word {
		case "user", "users":
			user = true
		case "me", "current", "self":
			current = true
		case "whoami":
			return true
		}
	}
	return user && current
}

// findEmail returns the address in a user record: a top-level "email", or one
// in "authUser", "user", "currentUser" or "me". Other nested addresses (team
// members, connections) belong to someone else.
func findEmail(raw json.RawMessage) string {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	read := func(raw json.RawMessage) string {
		var record struct {
			Email string `json:"email"`
		}
		_ = json.Unmarshal(raw, &record)
		if email := strings.TrimSpace(record.Email); strings.Contains(email, "@") {
			return email
		}
		return ""
	}
	if email := read(raw); email != "" {
		return email
	}
	for _, key := range []string{"authUser", "user", "currentUser", "me"} {
		if nested, ok := doc[key]; ok {
			if email := read(nested); email != "" {
				return email
			}
		}
	}
	return ""
}

// ensureEmail fills in a missing user address on the credential and its cached
// copy. The host writes it to the credential file with the returned auth.
func ensureEmail(callback, key string, s storage) storage {
	if s.Email != "" {
		return s
	}
	email := fetchEmail(callback, s)
	if email == "" {
		return s
	}
	s.Email = email
	tokenMu.Lock()
	if cached, ok := tokenCache[key]; ok {
		cached.Email = email
		tokenCache[key] = cached
	}
	tokenMu.Unlock()
	return s
}

func refreshCredential(callback, id string, raw []byte) (storage, error) {
	s, key, err := liveStorage(callback, id, raw, true)
	if err != nil {
		return s, err
	}
	// Credentials signed in before the address was recorded get it here.
	s = ensureEmail(callback, key, s)
	// Repair a deleted or outdated gateway during the scheduled refresh rather
	// than on the first user request.
	if s.ScenarioID == 0 || s.GatewayVersion != gatewayVersion {
		if s, err = ensureGateway(callback, key, s); err != nil {
			return s, err
		}
	}
	return s, nil
}

func redirectURI(baseURL string) string {
	if public := currentConfig().PublicBaseURL; public != "" {
		return public + "/v0/management/oauth-callback"
	}
	if strings.TrimSpace(baseURL) != "" {
		return strings.TrimSpace(baseURL)
	}
	return "http://localhost:8317/v0/management/oauth-callback"
}

func registerClient(callback, redirect string) (string, string, error) {
	body := map[string]any{"client_name": "CLIProxyAPI Make provider", "redirect_uris": []string{redirect},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "client_secret_post", "scope": oauthScopes}
	response, err := httpCall(callback, "POST", registerURL, oauthHeaders("application/json"), encode(body))
	if err != nil {
		return "", "", err
	}
	var client struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || json.Unmarshal(response.Body, &client) != nil || client.ClientID == "" {
		return "", "", newStatusError(502, "Make OAuth client registration failed (HTTP %d)", response.StatusCode)
	}
	return client.ClientID, client.ClientSecret, nil
}

// resolveAuthDir is the host's auth directory, by default ~/.cli-proxy-api.
func resolveAuthDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		dir = framework.DefaultAuthDir
	}
	return framework.ResolveAuthDir(dir)
}

func validState(state string) bool {
	if len(state) != 32 {
		return false
	}
	_, err := hex.DecodeString(state)
	return err == nil
}

func sessionPath(dir, state string) string {
	return filepath.Join(dir, ".make-oauth-"+state+".pending")
}

func startLogin(r rpcRequest) (any, error) {
	redirect := redirectURI(r.BaseURL)
	clientID, clientSecret, err := registerClient(r.HostCallbackID, redirect)
	if err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(randomBytes(48))
	challenge := sha256.Sum256([]byte(verifier))
	state := hex.EncodeToString(randomBytes(16))
	authDir := resolveAuthDir(r.Host.AuthDir)
	session := loginSession{Verifier: verifier, ClientID: clientID, ClientSecret: clientSecret, RedirectURI: redirect, AuthDir: authDir, Expires: time.Now().Add(loginLifetime)}
	if authDir != "" {
		// Survives a plugin reload between start and callback; mode 0600 because
		// it holds the PKCE verifier and client secret.
		_ = os.MkdirAll(authDir, 0o700)
		_ = os.WriteFile(sessionPath(authDir, state), encode(session), 0o600)
		pending, _ := filepath.Glob(filepath.Join(authDir, ".make-oauth-*.pending"))
		for _, path := range pending {
			var old loginSession
			if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &old) == nil && time.Now().After(old.Expires) {
				_ = os.Remove(path)
			}
		}
	}
	stateMu.Lock()
	for key, value := range sessions {
		if time.Now().After(value.Expires) {
			delete(sessions, key)
		}
	}
	sessions[state] = session
	stateMu.Unlock()
	query := url.Values{"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {oauthScopes},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	return pluginapi.AuthLoginStartResponse{Provider: providerKey, URL: authorizeURL + "?" + query.Encode(), State: state, ExpiresAt: session.Expires}, nil
}

func pollLogin(r rpcRequest) (any, error) {
	if !validState(r.State) {
		return nil, fmt.Errorf("Invalid Make sign-in state")
	}
	loginMu.Lock()
	defer loginMu.Unlock()
	stateMu.Lock()
	session, ok := sessions[r.State]
	stateMu.Unlock()
	authDir := resolveAuthDir(r.Host.AuthDir)
	if !ok && authDir != "" {
		if raw, err := os.ReadFile(sessionPath(authDir, r.State)); err == nil && json.Unmarshal(raw, &session) == nil {
			ok = true
		}
	}
	if !ok || time.Now().After(session.Expires) {
		return nil, fmt.Errorf("Make sign-in expired")
	}
	if session.AuthDir != "" {
		authDir = session.AuthDir
	}
	callbackPath := filepath.Join(authDir, ".oauth-"+providerKey+"-"+r.State+".oauth")
	raw, err := os.ReadFile(callbackPath)
	if os.IsNotExist(err) {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("Cannot read Make callback")
	}
	var callback struct {
		Code, State, Error string
	}
	if json.Unmarshal(raw, &callback) != nil || callback.State != r.State {
		return nil, fmt.Errorf("Invalid Make callback state")
	}
	cleanup := func() {
		_ = os.Remove(callbackPath)
		_ = os.Remove(sessionPath(authDir, r.State))
		stateMu.Lock()
		delete(sessions, r.State)
		stateMu.Unlock()
	}
	if callback.Error != "" || callback.Code == "" {
		cleanup()
		if callback.Error != "" {
			// An OAuth error code from Make, for example invalid_scope.
			return nil, fmt.Errorf("Make sign-in was not authorized: %s", truncate(callback.Error, 200))
		}
		return nil, fmt.Errorf("Make sign-in was not authorized")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {callback.Code}, "code_verifier": {session.Verifier},
		"client_id": {session.ClientID}, "redirect_uri": {session.RedirectURI}}
	if session.ClientSecret != "" {
		form.Set("client_secret", session.ClientSecret)
	}
	tokens, err := requestToken(r.HostCallbackID, "OAuth exchange", form)
	cleanup()
	if err != nil {
		return nil, err
	}
	s := storage{Type: providerKey, ClientID: session.ClientID, ClientSecret: session.ClientSecret, RedirectURI: session.RedirectURI}
	applyTokens(&s, tokens)
	if s.OrganizationID == 0 {
		return nil, fmt.Errorf("Make sign-in did not select an organization")
	}
	key := credentialName(s)
	tokenMu.Lock()
	tokenCache[key] = s
	tokenMu.Unlock()
	// Save the rotating refresh token before provisioning, so a provisioning
	// failure never forces another browser sign-in.
	if err := persist(key, s); err != nil {
		return nil, err
	}
	s = ensureEmail(r.HostCallbackID, key, s)
	s, err = ensureGateway(r.HostCallbackID, key, s)
	if err != nil {
		return nil, err
	}
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: authData(s, key)}, nil
}
