package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/BierDav/cliproxyapi-plugins/framework/abi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// hostCall reaches the host; tests replace it.
var hostCall framework.Caller = abi.Call

func host() framework.Host { return framework.Host{Call: hostCall} }

var plugin = newPlugin()

func init() { abi.Serve(plugin) }

func main() {}

const apiBase = "https://api.jetbrains.ai"
const cloudBase = "https://api.jetbrains.cloud"
const tokenURL = "https://oauth.account.jetbrains.com/oauth2/token"
const redirectURI = "http://localhost:3000"
const jcpUserManagementAudience = "jcp-user-management"
const grazieLiteLicenseType = "grazie.individual.lite"
const userInfoURL = apiBase + "/auth/jetbrains-jwt/user-info"
const registerURL = apiBase + "/auth/jetbrains-jwt/register"

const jetbrainsLogoSVG = `<svg xmlns="http://www.w3.org/2000/svg" xml:space="preserve" viewBox="0 0 512 512"><linearGradient id="jetbrains_svg__a" x1="6.785" x2="500.945" y1="12.255" y2="499.535" gradientTransform="matrix(1 0 0 -1 0 514)" gradientUnits="userSpaceOnUse"><stop offset="0" style="stop-color:#ff9419"/><stop offset=".43" style="stop-color:#ff021d"/><stop offset=".99" style="stop-color:#e600ff"/></linearGradient><path d="M162.7 29.3 29.3 162.7C10.6 181.4 0 206.9 0 233.4V472c0 22.1 17.9 40 40 40h238.6c26.6 0 51.9-10.6 70.7-29.3l133.4-133.4c18.7-18.7 29.3-44.2 29.3-70.7V40c0-22.1-17.9-40-40-40H233.4c-26.5 0-51.9 10.6-70.7 29.3" style="fill:url(#jetbrains_svg__a)"/><path d="M384 128H64v320h320z"/><path d="M240 376H104v32h136z" style="fill:#fff"/></svg>`

type storage struct {
	Type         string `json:"type"`
	Email        string `json:"email"`
	LicenseID    string `json:"license_id"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	IDExpires    int64  `json:"id_token_expires_at"`
	JWT          string `json:"jwt"`
	JWTExpires   int64  `json:"jwt_expires_at"`
}
type loginSession struct {
	Verifier, License, AuthDir string
	Licenses                   []string
	Expires                    time.Time
}

var stateMu sync.Mutex
var sessions = map[string]loginSession{}
var configuredLicenses []string
var upgradeAttempts = map[string]time.Time{}
var loginMu sync.Mutex
var tokenMu sync.Mutex
var tokenCache = map[string]storage{}

func shutdown() {
	stateMu.Lock()
	clear(sessions)
	clear(upgradeAttempts)
	stateMu.Unlock()
	tokenMu.Lock()
	clear(tokenCache)
	tokenMu.Unlock()
}
func encode(v any) []byte { b, _ := json.Marshal(v); return b }

type statusError struct {
	status    int
	operation string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("JetBrains %s failed (HTTP %d)", e.operation, e.status)
}

// StatusCode gives the error's HTTP status to the framework.
func (e *statusError) StatusCode() int { return e.status }

type rpcRequest struct {
	AuthID         string
	FileName       string
	Provider       string
	RawJSON        []byte
	StorageJSON    []byte
	Metadata       map[string]any
	Attributes     map[string]string
	Host           pluginapi.HostConfigSummary
	State          string
	HostCallbackID string `json:"host_callback_id"`
}

// rpc decodes the request fields most methods share.
func rpc(fn func(rpcRequest) (any, error)) framework.Handler {
	return func(raw []byte) (any, error) {
		var r rpcRequest
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, fmt.Errorf("Invalid plugin request")
			}
		}
		return fn(r)
	}
}

func newPlugin() *framework.Plugin {
	p := &framework.Plugin{
		Identifier: "jetbrains",
		Metadata: pluginapi.Metadata{Name: "JetBrains", Version: "0.1.1", Author: "Prox", GitHubRepository: "https://github.com/BierDav/cliproxyapi-plugins/tree/main/jetbrains",
			Logo: "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(jetbrainsLogoSVG)),
			ConfigFields: []pluginapi.ConfigField{
				{Name: "license_id", Type: pluginapi.ConfigFieldTypeString, Description: "JetBrains AI License ID used for new sign-ins."},
				{Name: "license_ids", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional additional License IDs; the first one accepted for the signed-in account is retained."},
			}},
		Capabilities: framework.Capabilities{AuthProvider: true, ModelProvider: true, Executor: true, QuotaProvider: true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"claude", "openai", "openai-response"},
			ExecutorOutputFormats: []string{"claude", "openai", "openai-response"}},
		Configure:               configure,
		Quiesce:                 shutdown,
		Shutdown:                shutdown,
		ErrorCode:               "jetbrains_error",
		InternalErrorsRetryable: true,
	}
	p.Handle(pluginabi.MethodAuthParse, rpc(func(r rpcRequest) (any, error) {
		var s storage
		if json.Unmarshal(r.RawJSON, &s) != nil || s.Type != "jetbrains" {
			return pluginapi.AuthParseResponse{Handled: false}, nil
		}
		if s.RefreshToken == "" {
			return nil, fmt.Errorf("JetBrains credential needs a refresh token")
		}
		return pluginapi.AuthParseResponse{Handled: true, Auth: authData(s, r.FileName)}, nil
	}))
	p.Handle(pluginabi.MethodAuthRefresh, rpc(func(r rpcRequest) (any, error) {
		s, err := readStorage(r.StorageJSON)
		if err != nil {
			return nil, err
		}
		tokenMu.Lock()
		delete(tokenCache, s.Email)
		tokenMu.Unlock()
		stateMu.Lock()
		delete(upgradeAttempts, s.Email)
		stateMu.Unlock()
		wasLite := isLiteJWT(s.JWT)
		s.JWT = ""
		s, err = refreshStorage(r.HostCallbackID, s, wasLite)
		if err != nil {
			return nil, err
		}
		a := authData(s, r.AuthID)
		return pluginapi.AuthRefreshResponse{Auth: a, NextRefreshAfter: a.NextRefreshAfter}, nil
	}))
	p.Handle(pluginabi.MethodAuthLoginStart, rpc(startLogin))
	p.Handle(pluginabi.MethodAuthLoginPoll, rpc(pollLogin))
	p.Handle(pluginabi.MethodModelStatic, rpc(func(rpcRequest) (any, error) {
		return pluginapi.ModelResponse{Provider: "jetbrains", Models: []pluginapi.ModelInfo{}}, nil
	}))
	p.Handle(pluginabi.MethodModelForAuth, rpc(models))
	p.Handle(pluginabi.MethodExecutorExecute, func(raw []byte) (any, error) {
		return execute(pluginabi.MethodExecutorExecute, raw)
	})
	p.Handle(pluginabi.MethodExecutorExecuteStream, func(raw []byte) (any, error) {
		return execute(pluginabi.MethodExecutorExecuteStream, raw)
	})
	p.Handle(pluginabi.MethodExecutorCountTokens, func([]byte) (any, error) {
		return nil, &statusError{501, "token counting (use the provider's response usage)"}
	})
	p.Handle(pluginabi.MethodExecutorHTTPRequest, executorHTTP)
	p.Handle(pluginabi.MethodQuotaDescribe, func([]byte) (any, error) {
		return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{"jetbrains"}, DisplayName: "JetBrains", SupportsReset: false}, nil
	})
	p.Handle(pluginabi.MethodQuotaFetch, quota)
	return p
}

func configure(configYAML []byte) error {
	var config struct {
		License  any `yaml:"license_id"`
		Licenses any `yaml:"license_ids"`
	}
	if yaml.Unmarshal(configYAML, &config) != nil {
		return fmt.Errorf("Invalid JetBrains configuration")
	}
	stateMu.Lock()
	configuredLicenses = licenseCandidates(licenseValues(config.License), licenseValues(config.Licenses))
	clear(upgradeAttempts)
	stateMu.Unlock()
	tokenMu.Lock()
	clear(tokenCache)
	tokenMu.Unlock()
	return nil
}

func registration() framework.Registration { return plugin.Registration() }

func dispatch(method string, raw []byte) (any, error) { return plugin.Dispatch(method, raw) }

func fileName(email string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(email) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("@._-", r) {
			out.WriteRune(r)
		} else {
			out.WriteByte('-')
		}
	}
	return "jetbrains-" + out.String() + ".json"
}
func authData(s storage, id string) pluginapi.AuthData {
	if id == "" {
		id = fileName(s.Email)
	}
	next := time.UnixMilli(s.IDExpires).Add(-5 * time.Minute)
	if next.Before(time.Now().Add(time.Minute)) {
		next = time.Now().Add(time.Minute)
	}
	return pluginapi.AuthData{Provider: "jetbrains", ID: id, FileName: id, Label: s.Email, StorageJSON: encode(s),
		Metadata: map[string]any{"type": "jetbrains", "email": s.Email}, Attributes: map[string]string{"auth_kind": "oauth"}, NextRefreshAfter: next}
}
func readStorage(raw []byte) (storage, error) {
	var s storage
	if json.Unmarshal(raw, &s) != nil || s.Type != "jetbrains" || s.RefreshToken == "" {
		return s, fmt.Errorf("Invalid JetBrains credential")
	}
	return s, nil
}
func jwtInfo(token string) (string, int64, error) {
	pieces := strings.Split(token, ".")
	if len(pieces) != 3 {
		return "", 0, fmt.Errorf("Invalid JetBrains token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(pieces[1])
	if err != nil {
		return "", 0, fmt.Errorf("Invalid JetBrains token")
	}
	var p struct {
		Email    string `json:"email"`
		Username string `json:"preferred_username"`
		Exp      int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Exp <= 0 {
		return "", 0, fmt.Errorf("Invalid JetBrains token expiry")
	}
	if p.Email == "" {
		p.Email = p.Username
	}
	return p.Email, p.Exp * 1000, nil
}

type upstreamResponse = framework.HTTPResponse

func httpCall(callback, method, address string, headers http.Header, body []byte) (upstreamResponse, error) {
	response, err := host().Do(context.Background(), callback, framework.HTTPRequest{Method: method, URL: address, Headers: headers, Body: body})
	switch {
	case errors.Is(err, framework.ErrHostResponse):
		return response, fmt.Errorf("Invalid JetBrains transport response")
	case err != nil:
		return response, fmt.Errorf("JetBrains transport failed")
	}
	return response, nil
}
func checked(callback, operation, method, address string, headers http.Header, body []byte) ([]byte, error) {
	response, err := httpCall(callback, method, address, headers, body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		if status == 477 {
			status = 429
		}
		if operation == "OAuth exchange" || operation == "OAuth refresh" || operation == "OAuth audience exchange" {
			var oauthError struct {
				Code string `json:"error"`
			}
			_ = json.Unmarshal(response.Body, &oauthError)
			// Include only standard error codes, never arbitrary upstream text.
			switch oauthError.Code {
			case "invalid_request", "invalid_client", "invalid_grant", "unauthorized_client", "unsupported_grant_type", "invalid_scope":
				operation += " (" + oauthError.Code + ")"
			}
		}
		return nil, &statusError{status, operation}
	}
	return response.Body, nil
}
func tokenHeaders(id string) http.Header {
	return http.Header{"Authorization": {"Bearer " + id}, "Content-Type": {"application/json"}, "User-Agent": {"ktor-client"}}
}
func apiHeaders(jwt string) http.Header {
	return http.Header{"Grazie-Authenticate-Jwt": {jwt}, "Content-Type": {"application/json"}, "User-Agent": {"ktor-client"}, "Grazie-Agent": {`{"name":"aia:idea","version":"261.22158.366:261.22158.277"}`}}
}
func obtainGrazieLite(callback, idToken string) (string, error) {
	raw, err := checked(callback, "obtain grazie-lite license", "POST", apiBase+"/auth/jetbrains-jwt/license/obtain/grazie-lite", tokenHeaders(idToken), []byte("{}"))
	if err != nil {
		return "", err
	}
	var response struct {
		License struct {
			LicenseID string `json:"licenseId"`
		} `json:"license"`
	}
	if json.Unmarshal(raw, &response) != nil || strings.TrimSpace(response.License.LicenseID) == "" {
		return "", fmt.Errorf("JetBrains did not return a valid free AI license")
	}
	return strings.TrimSpace(response.License.LicenseID), nil
}

func activateLicense(callback, idToken, licenseID string) (string, int64, error) {
	if strings.TrimSpace(licenseID) == "" {
		return "", 0, fmt.Errorf("JetBrains License ID is empty")
	}
	raw, err := checked(callback, "license activation", "POST", apiBase+"/auth/jetbrains-jwt/provide-access/license/v2", tokenHeaders(idToken), encode(map[string]string{"licenseId": licenseID}))
	if err != nil {
		return "", 0, err
	}
	var response struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Token == "" {
		return "", 0, fmt.Errorf("JetBrains license has no AI access token")
	}
	_, expiry, err := jwtInfo(response.Token)
	if err != nil {
		return "", 0, err
	}
	return response.Token, expiry, nil
}

func isConfiguredLicense(license string) bool {
	license = strings.TrimSpace(license)
	if license == "" {
		return false
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	for _, c := range configuredLicenses {
		if strings.EqualFold(strings.TrimSpace(c), license) {
			return true
		}
	}
	return false
}

func candidateLicenses(savedLicense string) []string {
	stateMu.Lock()
	configured := append([]string(nil), configuredLicenses...)
	stateMu.Unlock()

	savedLicense = strings.TrimSpace(savedLicense)
	var candidates []string
	seen := map[string]bool{}

	add := func(lic string) {
		lic = strings.TrimSpace(lic)
		if lic != "" && !seen[strings.ToLower(lic)] {
			seen[strings.ToLower(lic)] = true
			candidates = append(candidates, lic)
		}
	}

	isConfigured := false
	for _, c := range configured {
		if strings.EqualFold(strings.TrimSpace(c), savedLicense) {
			isConfigured = true
			break
		}
	}

	if isConfigured {
		add(savedLicense)
		for _, c := range configured {
			add(c)
		}
	} else {
		for _, c := range configured {
			add(c)
		}
		add(savedLicense)
	}
	return candidates
}

func jwtLicenseType(token string) string {
	pieces := strings.Split(token, ".")
	if len(pieces) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(pieces[1])
	if err != nil {
		return ""
	}
	var p struct {
		LicenseType string `json:"license_type"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return strings.TrimSpace(p.LicenseType)
}

func isLiteJWT(token string) bool {
	return strings.EqualFold(jwtLicenseType(token), grazieLiteLicenseType)
}

func discoverLicenses(callback, refreshToken string) ([]string, string) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, ""
	}
	form := url.Values{
		"grant_type":    {"switch_audience"},
		"audience":      {jcpUserManagementAudience},
		"client_id":     {"ide"},
		"refresh_token": {refreshToken},
	}
	raw, err := checked(callback, "OAuth audience exchange", "POST", tokenURL, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(form.Encode()))
	if err != nil {
		return nil, ""
	}
	var tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &tokens) != nil || strings.TrimSpace(tokens.Access) == "" {
		return nil, strings.TrimSpace(tokens.Refresh)
	}
	optionsRaw, err := checked(callback, "license discovery", "GET", cloudBase+"/user-management/api/ai-access-options", http.Header{"Authorization": {"Bearer " + tokens.Access}, "User-Agent": {"ktor-client"}}, nil)
	if err != nil {
		return nil, strings.TrimSpace(tokens.Refresh)
	}
	var doc struct {
		Options []struct {
			Type            string `json:"type"`
			Name            string `json:"name"`
			LicenseID       string `json:"licenseId"`
			AIAccessEnabled bool   `json:"aiAccessEnabled"`
			Code            string `json:"code"`
			FullName        string `json:"fullName"`
		} `json:"aiAccessOptions"`
	}
	if json.Unmarshal(optionsRaw, &doc) != nil {
		return nil, strings.TrimSpace(tokens.Refresh)
	}
	var paid, free, lite []string
	seen := map[string]bool{}
	for _, opt := range doc.Options {
		if !strings.EqualFold(strings.TrimSpace(opt.Type), "license") || !opt.AIAccessEnabled {
			continue
		}
		id := strings.TrimSpace(opt.LicenseID)
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		desc := strings.ToLower(opt.Code + " " + opt.Name + " " + opt.FullName)
		switch {
		case strings.Contains(desc, "lite"):
			lite = append(lite, id)
		case strings.Contains(desc, "free") || strings.EqualFold(strings.TrimSpace(opt.Code), "edup") || strings.EqualFold(strings.TrimSpace(opt.Name), "edup"):
			free = append(free, id)
		default:
			paid = append(paid, id)
		}
	}
	out := make([]string, 0, len(paid)+len(free)+len(lite))
	out = append(out, paid...)
	out = append(out, free...)
	out = append(out, lite...)
	return out, strings.TrimSpace(tokens.Refresh)
}

// ensureRegistered registers an account with JetBrains' AI service when its
// user info is refused (4xx), which is how a never-used account looks. A failed
// lookup or a server error is left to the license activation that follows.
func ensureRegistered(callback, idToken string) error {
	response, err := httpCall(callback, "GET", userInfoURL, tokenHeaders(idToken), nil)
	if err != nil || response.StatusCode < 400 || response.StatusCode >= 500 {
		return nil
	}
	if _, err := checked(callback, "JetBrains account registration", "POST", registerURL, tokenHeaders(idToken), nil); err != nil {
		return err
	}
	return nil
}
func refresh(callback string, s storage) (storage, error) {
	return refreshStorage(callback, s, false)
}

func refreshStorage(callback string, s storage, forceDiscovery bool) (storage, error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	stateMu.Lock()
	configuredCount := len(configuredLicenses)
	stateMu.Unlock()

	isConfigured := isConfiguredLicense(s.LicenseID)
	if !forceDiscovery {
		if cached, ok := tokenCache[s.Email]; ok && cached.IDExpires > s.IDExpires && (s.LicenseID == "" || cached.LicenseID == s.LicenseID) && cached.JWT != "" {
			if isConfigured || (configuredCount == 0 && !isLiteJWT(cached.JWT)) {
				s = cached
				isConfigured = isConfiguredLicense(s.LicenseID)
			}
		} else if cached, ok := tokenCache[s.Email]; ok && s.LicenseID != "" && cached.LicenseID != "" && s.LicenseID != cached.LicenseID {
			s.JWT = ""
			s.JWTExpires = 0
		}
	}
	now := time.Now().Add(5 * time.Minute).UnixMilli()
	if s.IDToken == "" || s.IDExpires <= now {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.RefreshToken}, "client_id": {"ide"}}
		raw, err := checked(callback, "OAuth refresh", "POST", tokenURL, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(form.Encode()))
		if err != nil {
			return s, err
		}
		var tokens struct {
			ID      string `json:"id_token"`
			Refresh string `json:"refresh_token"`
		}
		if json.Unmarshal(raw, &tokens) != nil || tokens.ID == "" {
			return s, fmt.Errorf("JetBrains did not return an ID token")
		}
		email, expiry, err := jwtInfo(tokens.ID)
		if err != nil {
			return s, err
		}
		if email != "" && s.Email != "" && !strings.EqualFold(email, s.Email) {
			return s, fmt.Errorf("JetBrains refresh changed account identity")
		}
		s.IDToken = tokens.ID
		s.IDExpires = expiry
		if tokens.Refresh != "" {
			s.RefreshToken = tokens.Refresh
		}
	}

	isLite := forceDiscovery || isLiteJWT(s.JWT)
	var upgradeNeeded bool
	if (!isConfigured && configuredCount > 0) || (configuredCount == 0 && (s.LicenseID == "" || isLite)) {
		stateMu.Lock()
		lastAttempt := upgradeAttempts[s.Email]
		if forceDiscovery || time.Since(lastAttempt) >= 10*time.Minute {
			upgradeNeeded = true
			upgradeAttempts[s.Email] = time.Now()
		}
		stateMu.Unlock()
	}

	if s.JWT == "" || s.JWTExpires <= now || upgradeNeeded {
		// A JetBrains account that never used AI must be registered with
		// the AI service before any license can be activated or obtained.
		if err := ensureRegistered(callback, s.IDToken); err != nil {
			return s, err
		}
		var primary []string
		if configuredCount > 0 {
			for _, c := range candidateLicenses(s.LicenseID) {
				if isConfigured || isConfiguredLicense(c) {
					primary = append(primary, c)
				}
			}
		} else if strings.TrimSpace(s.LicenseID) != "" && !isLite {
			primary = []string{strings.TrimSpace(s.LicenseID)}
		}

		tried := map[string]bool{}
		var token string
		var expiry int64
		var activateErr error

		tryCandidates := func(list []string) bool {
			for _, candidate := range list {
				candidate = strings.TrimSpace(candidate)
				if candidate == "" || tried[strings.ToLower(candidate)] {
					continue
				}
				tried[strings.ToLower(candidate)] = true
				token, expiry, activateErr = activateLicense(callback, s.IDToken, candidate)
				if activateErr == nil {
					s.LicenseID = candidate
					return true
				}
			}
			return false
		}

		activated := tryCandidates(primary)
		if !activated {
			discovered, rotatedRefresh := discoverLicenses(callback, s.RefreshToken)
			if rotatedRefresh != "" {
				s.RefreshToken = rotatedRefresh
			}
			activated = tryCandidates(discovered)
		}
		if !activated && strings.TrimSpace(s.LicenseID) != "" {
			activated = tryCandidates([]string{s.LicenseID})
		}
		if !activated {
			if activateErr != nil && s.JWT != "" && s.JWTExpires > now {
				token = s.JWT
				expiry = s.JWTExpires
				activateErr = nil
			} else {
				liteID, liteErr := obtainGrazieLite(callback, s.IDToken)
				if liteErr == nil && liteID != "" {
					token, expiry, activateErr = activateLicense(callback, s.IDToken, liteID)
					if activateErr == nil {
						s.LicenseID = liteID
					}
				} else if activateErr == nil {
					if liteErr != nil {
						activateErr = liteErr
					} else {
						activateErr = fmt.Errorf("JetBrains did not return a valid free AI license")
					}
				}
			}
		}
		if activateErr != nil {
			return s, activateErr
		}
		s.JWT = token
		s.JWTExpires = expiry
	}
	tokenCache[s.Email] = s
	return s, nil
}
func liveStorage(callback, id string, raw []byte) (storage, error) {
	old, err := readStorage(raw)
	if err != nil {
		return old, err
	}
	s, err := refresh(callback, old)
	if err != nil {
		return s, err
	}
	if string(encode(s)) != string(encode(old)) {
		name := fileName(s.Email)
		if id != "" && filepath.Base(id) == id && strings.HasPrefix(id, "jetbrains-") {
			name = id
		}
		if err := host().SaveAuth(name, encode(s)); err != nil {
			return s, fmt.Errorf("Cannot persist refreshed JetBrains credential")
		}
	}
	return s, nil
}
func randomString() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("Secure random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func licenseValues(raw any) []string {
	switch v := raw.(type) {
	case string:
		return []string{v}
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
func licenseCandidates(primary []string, additional []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(primary)+len(additional))
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	for _, value := range primary {
		for _, candidate := range strings.Split(value, ",") {
			add(candidate)
		}
	}
	for _, value := range additional {
		for _, candidate := range strings.Split(value, ",") {
			add(candidate)
		}
	}
	return result
}
func candidateAuthDirs(dirs ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, d := range dirs {
		add(framework.ResolveAuthDir(d))
		if trimmed := strings.TrimSpace(d); trimmed != "" {
			add(filepath.Clean(trimmed))
		}
	}
	if len(out) == 0 {
		add(framework.ResolveAuthDir(framework.DefaultAuthDir))
	}
	return out
}
func loginSessionPath(dir, state string) (string, error) {
	if len(state) != 32 {
		return "", fmt.Errorf("Invalid JetBrains sign-in state")
	}
	dir = framework.ResolveAuthDir(dir)
	if _, err := hex.DecodeString(state); err != nil || dir == "" {
		return "", fmt.Errorf("Invalid JetBrains sign-in state")
	}
	return filepath.Join(dir, ".jetbrains-oauth-"+state+".pending"), nil
}
func startLogin(r rpcRequest) (any, error) {
	verifier := randomString()
	hash := sha256.Sum256([]byte(verifier))
	stateBytes := sha256.Sum256([]byte(randomString()))
	state := hex.EncodeToString(stateBytes[:16])
	explicit := metadataString(r.Metadata, "license_id", "licenseId", "license")
	stateMu.Lock()
	licenses := append([]string(nil), configuredLicenses...)
	stateMu.Unlock()
	if explicit != "" {
		licenses = licenseCandidates([]string{explicit}, licenses)
	}
	license := ""
	if len(licenses) > 0 {
		license = licenses[0]
	}
	authDir := framework.ResolveAuthDir(r.Host.AuthDir)
	session := loginSession{Verifier: verifier, License: license, Licenses: licenses, AuthDir: authDir, Expires: time.Now().Add(10 * time.Minute)}
	if path, err := loginSessionPath(authDir, state); err == nil {
		_ = os.MkdirAll(authDir, 0700)
		_ = os.WriteFile(path, encode(session), 0600)
		pendingFiles, _ := filepath.Glob(filepath.Join(authDir, ".jetbrains-oauth-*.pending"))
		for _, pending := range pendingFiles {
			var old loginSession
			if raw, err := os.ReadFile(pending); err == nil && json.Unmarshal(raw, &old) == nil && time.Now().After(old.Expires) {
				_ = os.Remove(pending)
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
	query := url.Values{"client_id": {"ide"}, "scope": {"openid offline_access r_ide_auth"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "redirect_uri": {redirectURI}, "state": {state}, "response_type": {"code"}}
	return pluginapi.AuthLoginStartResponse{Provider: "jetbrains", URL: "https://account.jetbrains.com/oauth/login?" + query.Encode(), State: state, ExpiresAt: time.Now().Add(10 * time.Minute)}, nil
}

func metadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func pollLogin(r rpcRequest) (any, error) {
	loginMu.Lock()
	defer loginMu.Unlock()
	stateMu.Lock()
	session, ok := sessions[r.State]
	stateMu.Unlock()
	if !ok {
		for _, dir := range candidateAuthDirs(r.Host.AuthDir) {
			if path, err := loginSessionPath(dir, r.State); err == nil {
				if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &session) == nil {
					ok = true
					break
				}
			}
		}
	}
	if !ok || time.Now().After(session.Expires) {
		return nil, fmt.Errorf("JetBrains sign-in expired")
	}
	var raw []byte
	var callbackPath string
	var readErr error
	for _, dir := range candidateAuthDirs(session.AuthDir, r.Host.AuthDir) {
		candidate := filepath.Join(dir, ".oauth-jetbrains-"+r.State+".oauth")
		data, err := os.ReadFile(candidate)
		if err == nil {
			raw = data
			callbackPath = candidate
			readErr = nil
			break
		}
		if !os.IsNotExist(err) && readErr == nil {
			readErr = err
		}
	}
	if callbackPath == "" {
		if readErr != nil {
			return nil, fmt.Errorf("Cannot read JetBrains callback")
		}
		return pluginapi.AuthLoginPollResponse{Status: "pending"}, nil
	}
	var callback map[string]string
	if json.Unmarshal(raw, &callback) != nil || callback["state"] != r.State {
		return nil, fmt.Errorf("Invalid JetBrains callback state")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {callback["code"]}, "code_verifier": {session.Verifier}, "client_id": {"ide"}, "redirect_uri": {redirectURI}}
	tokens, err := checked(r.HostCallbackID, "OAuth exchange", "POST", tokenURL, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(form.Encode()))
	if err != nil {
		return nil, err
	}
	var result struct {
		ID      string `json:"id_token"`
		Refresh string `json:"refresh_token"`
	}
	if json.Unmarshal(tokens, &result) != nil || result.Refresh == "" {
		return nil, fmt.Errorf("JetBrains sign-in returned no refresh token")
	}
	email, expiry, err := jwtInfo(result.ID)
	if err != nil {
		return nil, err
	}
	s := storage{Type: "jetbrains", Email: email, LicenseID: session.License, RefreshToken: result.Refresh, IDToken: result.ID, IDExpires: expiry}
	// Save before license activation: an incorrect License ID must not discard
	// the successfully authorized refresh token or require another browser login.
	if err := host().SaveAuth(fileName(email), encode(s)); err != nil {
		return nil, fmt.Errorf("Cannot persist JetBrains sign-in")
	}
	_ = os.Remove(callbackPath)
	for _, dir := range candidateAuthDirs(session.AuthDir, r.Host.AuthDir) {
		if pending, err := loginSessionPath(dir, r.State); err == nil {
			_ = os.Remove(pending)
		}
	}
	stateMu.Lock()
	delete(sessions, r.State)
	stateMu.Unlock()
	candidates := session.Licenses
	if len(candidates) == 0 && session.License != "" {
		candidates = []string{session.License}
	}
	var refreshErr error
	if len(candidates) > 0 {
		for _, candidate := range candidates {
			s.LicenseID = candidate
			s.JWT = ""
			s, refreshErr = refresh(r.HostCallbackID, s)
			if refreshErr == nil {
				break
			}
		}
	} else {
		s.JWT = ""
		s, refreshErr = refresh(r.HostCallbackID, s)
	}
	if refreshErr != nil {
		if liteID, liteErr := obtainGrazieLite(r.HostCallbackID, s.IDToken); liteErr == nil && liteID != "" {
			s.LicenseID = liteID
			s.JWT = ""
			s, refreshErr = refresh(r.HostCallbackID, s)
		}
	}
	if refreshErr != nil {
		return nil, refreshErr
	}
	if err := host().SaveAuth(fileName(email), encode(s)); err != nil {
		return nil, fmt.Errorf("Cannot persist JetBrains sign-in")
	}
	return pluginapi.AuthLoginPollResponse{Status: "success", Auth: authData(s, "")}, nil
}

// Quota reads also serve the existing T3 usage bridge through api-call, with
// refreshing and token substitution kept inside the plugin.
func executorHTTP(raw []byte) (any, error) {
	var r struct {
		pluginapi.ExecutorHTTPRequest
		Callback string `json:"host_callback_id"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fmt.Errorf("Invalid HTTP request")
	}
	if r.URL != apiBase+"/user/v5/quota/get" || r.Method != "POST" {
		return nil, &statusError{400, "unsupported management HTTP request"}
	}
	s, err := liveStorage(r.Callback, r.AuthID, r.StorageJSON)
	if err != nil {
		return nil, err
	}
	response, err := httpCall(r.Callback, "POST", r.URL, apiHeaders(s.JWT), []byte("{}"))
	if err != nil {
		return nil, err
	}
	return pluginapi.ExecutorHTTPResponse{StatusCode: response.StatusCode, Headers: response.Headers, Body: response.Body}, nil
}
func quota(raw []byte) (any, error) {
	var r struct {
		pluginapi.QuotaFetchRequest
		Callback string `json:"host_callback_id"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fmt.Errorf("Invalid quota request")
	}
	storageJSON, err := storageForQuota(r.QuotaFetchRequest)
	if err != nil {
		return nil, err
	}
	s, err := liveStorage(r.Callback, r.AuthID, storageJSON)
	if err != nil {
		return nil, err
	}
	body, err := checked(r.Callback, "quota read", "POST", apiBase+"/user/v5/quota/get", apiHeaders(s.JWT), []byte("{}"))
	if err != nil {
		return nil, err
	}
	used, maximum, err := parseQuota(body)
	if err != nil {
		return nil, err
	}
	// The quota response's own "until" is the subscription end, so the
	// monthly reset comes from the refill metadata and is best-effort: an
	// unavailable refill must not hide the allowance itself.
	reset := ""
	if refill, refillErr := checked(r.Callback, "quota refill read", "POST", apiBase+"/user/v5/quota/metadata/refill", apiHeaders(s.JWT), []byte("{}")); refillErr == nil {
		reset = parseRefill(refill)
	}
	remaining := 0.0
	if maximum > 0 {
		remaining = 1 - used/maximum
		if remaining < 0 {
			remaining = 0
		}
		if remaining > 1 {
			remaining = 1
		}
	}
	// Keep the progress bucket for the dashboard's visual quota indicator, and
	// expose the raw allowance values as summary metrics so Quota Management can
	// show the actual usage and limit alongside the percentage.
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "JetBrains AI"},
		Summary: []pluginapi.QuotaMetric{
			{Key: "usage", Label: "Used", Value: used},
			{Key: "limit", Label: "Limit", Value: maximum},
		},
		Groups: []pluginapi.QuotaGroup{{DisplayName: "Included allowance", Buckets: []pluginapi.QuotaBucket{{Window: "monthly", RemainingFraction: remaining, ResetTime: reset, Description: "Included JetBrains AI allowance"}}}},
	}, nil
}

func storageForQuota(req pluginapi.QuotaFetchRequest) ([]byte, error) {
	if len(req.StorageJSON) != 0 {
		return req.StorageJSON, nil
	}
	if !strings.EqualFold(strings.TrimSpace(req.Provider), "jetbrains") {
		return nil, fmt.Errorf("Invalid JetBrains credential")
	}
	// Home workers hold the selected credential in metadata instead of a file.
	raw, err := json.Marshal(req.Metadata)
	if err != nil {
		return nil, fmt.Errorf("Invalid JetBrains credential")
	}
	if _, err := readStorage(raw); err != nil {
		return nil, err
	}
	return raw, nil
}
