package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework/sse"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func testToken(email string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString(encode(map[string]any{"email": email, "exp": time.Now().Add(time.Hour).Unix()})) + ".signature"
}

func testJWT(email, licenseType string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString(encode(map[string]any{"email": email, "license_type": licenseType, "exp": time.Now().Add(time.Hour).Unix()})) + ".signature"
}
func setup(t *testing.T) {
	old := hostCall
	t.Cleanup(func() { hostCall = old })
	clear(tokenCache)
	clear(catalogs)
	clear(sessions)
	stateMu.Lock()
	clear(upgradeAttempts)
	configuredLicenses = nil
	stateMu.Unlock()
}
func credential() storage {
	return storage{Type: "jetbrains", Email: "test@example.com", LicenseID: "test-license", RefreshToken: "private-refresh", IDToken: "private-id", IDExpires: time.Now().Add(time.Hour).UnixMilli(), JWT: "private-jwt", JWTExpires: time.Now().Add(time.Hour).UnixMilli()}
}

func TestRegistrationBrandAndProviderIdentity(t *testing.T) {
	var registered struct {
		Metadata pluginapi.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(encode(registration()), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.Metadata.Name != "JetBrains" {
		t.Fatalf("name = %q", registered.Metadata.Name)
	}
	logo, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(registered.Metadata.Logo, "data:image/svg+xml;base64,"))
	if err != nil || !strings.HasPrefix(string(logo), "<svg") {
		t.Fatal("logo must be an SVG image data URL")
	}
	if authData(credential(), "test.json").Provider != "jetbrains" {
		t.Fatal("provider identifier must remain lowercase")
	}
}

func TestQuotaUsesFilelessSelectedCredential(t *testing.T) {
	var metadata map[string]any
	if err := json.Unmarshal(encode(credential()), &metadata); err != nil {
		t.Fatal(err)
	}
	req := pluginapi.QuotaFetchRequest{Provider: "jetbrains", Metadata: metadata}
	raw, err := storageForQuota(req)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := readStorage(raw)
	if err != nil || stored.Email != "test@example.com" {
		t.Fatal("selected worker credential was not preserved")
	}
	req.Provider = "other"
	if _, err := storageForQuota(req); err == nil {
		t.Fatal("must reject credentials belonging to another provider")
	}
}

func TestConfiguredLicenseAllowsDashboardSignIn(t *testing.T) {
	setup(t)
	config := encode(map[string]any{"config_yaml": []byte("license_id: fixture-license\n")})
	if _, err := dispatch(pluginabi.MethodPluginReconfigure, config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = dispatch(pluginabi.MethodPluginReconfigure, encode(map[string]any{"config_yaml": []byte("{}")}))
	})
	result, err := startLogin(rpcRequest{Host: pluginapi.HostConfigSummary{AuthDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	flow := result.(pluginapi.AuthLoginStartResponse)
	stateMu.Lock()
	session := sessions[flow.State]
	delete(sessions, flow.State)
	stateMu.Unlock()
	if session.License != "fixture-license" {
		t.Fatal("dashboard sign-in ignored configured license")
	}
}

func TestSignInWithoutConfiguredLicenseAndEmptyAuthDirFallsBackToGrazieLite(t *testing.T) {
	setup(t)
	result, err := startLogin(rpcRequest{})
	if err != nil {
		t.Fatalf("expected sign-in start to succeed without license or auth dir, got %v", err)
	}
	flow := result.(pluginapi.AuthLoginStartResponse)
	if flow.URL == "" || flow.State == "" {
		t.Fatalf("expected valid authorization URL and state, got %#v", flow)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".oauth-jetbrains-"+flow.State+".oauth"), encode(map[string]string{"state": flow.State, "code": "private-code"}), 0600); err != nil {
		t.Fatal(err)
	}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		if method == pluginabi.MethodHostAuthSave {
			var s storage
			if err := json.Unmarshal(req["json"].(json.RawMessage), &s); err != nil {
				t.Fatal(err)
			}
			parsed, err := dispatch(pluginabi.MethodAuthParse, encode(rpcRequest{RawJSON: req["json"].(json.RawMessage), FileName: req["name"].(string)}))
			if err != nil || !parsed.(pluginapi.AuthParseResponse).Handled {
				t.Fatalf("expected saved credential to parse cleanly, got %v %v", parsed, err)
			}
			return encode(map[string]any{}), nil
		}
		urlStr, _ := req["url"].(string)
		if urlStr == tokenURL {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{"id_token": testToken("free@example.com"), "refresh_token": "private-refresh"})}), nil
		}
		if strings.Contains(urlStr, "/license/obtain/grazie-lite") {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{
				"license": map[string]string{"licenseId": "lite-free-id", "type": "grazie.individual.lite"},
			})}), nil
		}
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			var body struct {
				LicenseID string `json:"licenseId"`
			}
			_ = json.Unmarshal(req["body"].([]byte), &body)
			if body.LicenseID == "lite-free-id" {
				return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testToken("free@example.com")})}), nil
			}
			return encode(upstreamResponse{StatusCode: 403, Body: []byte(`Forbidden`)}), nil
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unknown endpoint`)}), nil
	}
	pollRes, err := pollLogin(rpcRequest{State: flow.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}})
	if err != nil {
		t.Fatalf("expected pollLogin without configured license to succeed via grazie-lite, got %v", err)
	}
	resp := pollRes.(pluginapi.AuthLoginPollResponse)
	if resp.Status != "success" {
		t.Fatalf("expected status success, got %s", resp.Status)
	}
	var auth storage
	if err := json.Unmarshal(resp.Auth.StorageJSON, &auth); err != nil || auth.LicenseID != "lite-free-id" {
		t.Fatalf("expected lite-free-id in saved auth, got %#v (err=%v)", auth, err)
	}
}

func TestStartLoginCreatesMissingAndTildeAuthDir(t *testing.T) {
	setup(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	result, err := startLogin(rpcRequest{Host: pluginapi.HostConfigSummary{AuthDir: "~/.cli-proxy-api/nested"}})
	if err != nil {
		t.Fatalf("expected startLogin with tilde path to succeed, got %v", err)
	}
	flow := result.(pluginapi.AuthLoginStartResponse)
	expectedPath := filepath.Join(home, ".cli-proxy-api", "nested", ".jetbrains-oauth-"+flow.State+".pending")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected pending session file at %s, got %v", expectedPath, err)
	}
}

func TestNativeClaudeKeepsThinkingAndUsesAccount(t *testing.T) {
	setup(t)
	var sent map[string]any
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("unexpected callback %s", method)
		}
		address := req["url"].(string)
		body := []byte(`{"profiles":[{"id":"anthropic-claude-4-6-sonnet"}]}`)
		if strings.Contains(address, "/anthropic/v1/messages") {
			sent = map[string]any{}
			_ = json.Unmarshal(req["body"].([]byte), &sent)
			body = []byte(`{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"signed"},{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`)
		}
		return encode(upstreamResponse{StatusCode: 200, Body: body}), nil
	}
	raw := []byte(`{"model":"jetbrains/claude-sonnet-4-6","messages":[{"role":"user","content":"test"}],"max_tokens":32,"thinking":{"type":"adaptive"}}`)
	response, err := execute(pluginabi.MethodExecutorExecute, encode(execution{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: fileName("test@example.com"), Model: "jetbrains/claude-sonnet-4-6", SourceFormat: "claude", Payload: raw, OriginalRequest: raw, StorageJSON: encode(credential())}}))
	if err != nil {
		t.Fatal(err)
	}
	result := response.(pluginapi.ExecutorResponse)
	if !strings.Contains(string(result.Payload), `"signature":"signed"`) {
		t.Fatal("lost native thinking signature")
	}
	if sent["model"] != "claude-sonnet-4-6" || sent["thinking"] == nil {
		t.Fatal("wrong native model or missing thinking")
	}
}
func TestQuotaRejectsMissingAndNonfiniteData(t *testing.T) {
	good := []byte(`{"current":{"tariffQuota":{"current":{"amount":"125"},"maximum":{"amount":"500"}},"until":1796083200000}}`)
	used, maximum, err := parseQuota(good)
	if err != nil || used != 125 || maximum != 500 {
		t.Fatalf("wrong quota: %v %v %v", used, maximum, err)
	}
	for _, raw := range []string{`{}`, `{"current":{"current":{"amount":"NaN"},"maximum":{"amount":100}}}`, `{"current":{"current":{"amount":-1},"maximum":{"amount":100}}}`, `{"current":{"current":{"amount":0},"maximum":{"amount":-1}}}`} {
		if _, _, err := parseQuota([]byte(raw)); err == nil {
			t.Fatal("accepted unknown quota as allowance")
		}
	}
	zero := []byte(`{"current":{"current":{"amount":0},"maximum":{"amount":0}}}`)
	zUsed, zLimit, err := parseQuota(zero)
	if err != nil || zUsed != 0 || zLimit != 0 {
		t.Fatalf("failed to parse zero quota: %v %v %v", zUsed, zLimit, err)
	}
}

func TestParseRefillUsesMonthlyNextNotSubscriptionUntil(t *testing.T) {
	refill := []byte(`{"current":{"tariff":{"amount":{"amount":"500"},"period":{"millis":2592000000}},"next":1796083200000,"last":1793404800000}}`)
	if reset := parseRefill(refill); reset != "2026-12-01T00:00:00Z" {
		t.Fatalf("wrong refill reset: %q", reset)
	}
	if reset := parseRefill([]byte(`{"current":{"next":"2026-12-01T00:00:00Z"}}`)); reset != "2026-12-01T00:00:00Z" {
		t.Fatalf("wrong string refill reset: %q", reset)
	}
	derived := []byte(`{"current":{"last":1793491200000,"tariff":{"period":{"millis":2592000000}}}}`)
	if reset := parseRefill(derived); reset != "2026-12-01T00:00:00Z" {
		t.Fatalf("wrong derived refill reset: %q", reset)
	}
	for _, raw := range []string{`{}`, `{"current":{"next":null}}`, `{"current":{"next":-1}}`, `{"current":{"last":0,"tariff":{"period":{"millis":0}}}}`} {
		if reset := parseRefill([]byte(raw)); reset != "" {
			t.Fatalf("accepted unknown refill as reset: %q", reset)
		}
	}
}

func TestQuotaFetchReadsResetFromRefillMetadata(t *testing.T) {
	setup(t)
	refilled := false
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("unexpected callback %s", method)
		}
		address := payload.(map[string]any)["url"].(string)
		if address == apiBase+"/user/v5/quota/metadata/refill" {
			refilled = true
			return encode(upstreamResponse{StatusCode: 200, Body: []byte(`{"current":{"next":1796083200000}}`)}), nil
		}
		if address != apiBase+"/user/v5/quota/get" {
			t.Fatalf("unexpected quota URL %s", address)
		}
		return encode(upstreamResponse{StatusCode: 200, Body: []byte(`{"current":{"tariffQuota":{"current":{"amount":"125"},"maximum":{"amount":"500"}},"until":1811019600000}}`)}), nil
	}
	result, err := quota(encode(pluginapi.QuotaFetchRequest{AuthID: fileName("test@example.com"), StorageJSON: encode(credential())}))
	if err != nil {
		t.Fatal(err)
	}
	if !refilled {
		t.Fatal("quota fetch did not read refill metadata")
	}
	response := result.(pluginapi.QuotaFetchResponse)
	if reset := response.Groups[0].Buckets[0].ResetTime; reset != "2026-12-01T00:00:00Z" {
		t.Fatalf("reset time used the subscription end instead of the monthly refill: %q", reset)
	}
	if response.Groups[0].Buckets[0].Window != "monthly" {
		t.Fatalf("unexpected window: %q", response.Groups[0].Buckets[0].Window)
	}
}

func TestQuotaFetchZeroLimitAllowance(t *testing.T) {
	setup(t)
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("unexpected callback %s", method)
		}
		return encode(upstreamResponse{StatusCode: 200, Body: []byte(`{"current":{"current":{"amount":"0.0"},"maximum":{"amount":"0.0"},"tariffQuota":{"current":{"amount":"0.0"},"maximum":{"amount":"0.0"}}}}`)}), nil
	}
	result, err := quota(encode(pluginapi.QuotaFetchRequest{AuthID: fileName("test@example.com"), StorageJSON: encode(credential())}))
	if err != nil {
		t.Fatal(err)
	}
	response := result.(pluginapi.QuotaFetchResponse)
	if len(response.Summary) != 2 || response.Summary[0].Value != 0 || response.Summary[1].Value != 0 {
		t.Fatalf("unexpected summary for zero quota: %#v", response.Summary)
	}
	if response.Groups[0].Buckets[0].RemainingFraction != 0 {
		t.Fatalf("unexpected remaining fraction for zero quota: %v", response.Groups[0].Buckets[0].RemainingFraction)
	}
}

func TestQuotaFetchIncludesUsageAndLimitMetrics(t *testing.T) {
	setup(t)
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("unexpected callback %s", method)
		}
		return encode(upstreamResponse{StatusCode: 200, Body: []byte(`{"current":{"tariffQuota":{"current":{"amount":"125"},"maximum":{"amount":"500"}}}}`)}), nil
	}
	result, err := quota(encode(pluginapi.QuotaFetchRequest{AuthID: fileName("test@example.com"), StorageJSON: encode(credential())}))
	if err != nil {
		t.Fatal(err)
	}
	response := result.(pluginapi.QuotaFetchResponse)
	if len(response.Summary) != 2 || response.Summary[0].Key != "usage" || response.Summary[0].Value != 125 || response.Summary[1].Key != "limit" || response.Summary[1].Value != 500 {
		t.Fatalf("unexpected quota summary: %#v", response.Summary)
	}
	if response.Groups[0].Buckets[0].RemainingFraction != 0.75 {
		t.Fatalf("unexpected remaining fraction: %v", response.Groups[0].Buckets[0].RemainingFraction)
	}
}

func TestOAuthSavesAuthorizedTokenBeforeLicenseFailure(t *testing.T) {
	setup(t)
	directory := t.TempDir()
	state := "test-state"
	sessions[state] = loginSession{Verifier: "private-pkce", License: "incorrect-license", AuthDir: directory, Expires: time.Now().Add(time.Minute)}
	if err := os.WriteFile(filepath.Join(directory, ".oauth-jetbrains-"+state+".oauth"), []byte(`{"state":"test-state","code":"private-code"}`), 0600); err != nil {
		t.Fatal(err)
	}
	saved := false
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		if method == pluginabi.MethodHostAuthSave {
			saved = true
			return encode(map[string]any{}), nil
		}
		if req["url"] == tokenURL {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{"id_token": testToken("test@example.com"), "refresh_token": "private-refresh"})}), nil
		}
		if !saved {
			t.Fatal("attempted activation before saving authorized token")
		}
		return encode(upstreamResponse{StatusCode: 403, Body: []byte(`{"secret":"must not leak"}`)}), nil
	}
	_, err := pollLogin(rpcRequest{State: state})
	if err == nil || !saved {
		t.Fatal("expected saved sign-in and activation failure")
	}
	if strings.Contains(err.Error(), "must not leak") {
		t.Fatal("leaked provider error body")
	}
}

func TestPollLoginFallsBackToGrazieLiteWhenConfiguredLicenseFails(t *testing.T) {
	setup(t)
	directory := t.TempDir()
	state := "test-state-lite"
	sessions[state] = loginSession{
		Verifier: "private-pkce",
		License:  "untrusted-license",
		Licenses: []string{"untrusted-license"},
		AuthDir:  directory,
		Expires:  time.Now().Add(time.Minute),
	}
	if err := os.WriteFile(filepath.Join(directory, ".oauth-jetbrains-"+state+".oauth"), []byte(`{"state":"test-state-lite","code":"private-code"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var savedStorages []storage
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		if method == pluginabi.MethodHostAuthSave {
			var s storage
			if json.Unmarshal(req["json"].(json.RawMessage), &s) == nil {
				savedStorages = append(savedStorages, s)
			}
			return encode(map[string]any{}), nil
		}
		urlStr, _ := req["url"].(string)
		if urlStr == tokenURL {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{"id_token": testToken("test@example.com"), "refresh_token": "private-refresh"})}), nil
		}
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			var body struct {
				LicenseID string `json:"licenseId"`
			}
			_ = json.Unmarshal(req["body"].([]byte), &body)
			if body.LicenseID == "untrusted-license" {
				return encode(upstreamResponse{StatusCode: 492, Body: []byte(`Untrusted license`)}), nil
			}
			if body.LicenseID == "lite-license-id" {
				return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testToken("test@example.com")})}), nil
			}
			return encode(upstreamResponse{StatusCode: 403, Body: []byte(`Forbidden`)}), nil
		}
		if strings.Contains(urlStr, "/license/obtain/grazie-lite") {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{
				"license": map[string]string{"licenseId": "lite-license-id", "type": "grazie.individual.lite"},
			})}), nil
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unknown endpoint`)}), nil
	}
	result, err := pollLogin(rpcRequest{State: state})
	if err != nil {
		t.Fatalf("expected successful sign-in with fallback, got %v", err)
	}
	resp := result.(pluginapi.AuthLoginPollResponse)
	if resp.Status != "success" {
		t.Fatalf("expected status success, got %s", resp.Status)
	}
	var auth storage
	if err := json.Unmarshal(resp.Auth.StorageJSON, &auth); err != nil || auth.LicenseID != "lite-license-id" {
		t.Fatalf("expected Auth.StorageJSON to use lite-license-id, got %#v", auth)
	}
	if len(savedStorages) < 2 || savedStorages[len(savedStorages)-1].LicenseID != "lite-license-id" {
		t.Fatalf("expected final saved storage with lite-license-id, got %#v", savedStorages)
	}
}

func TestRefreshFallsBackToGrazieLiteWhenSavedLicenseFails(t *testing.T) {
	setup(t)
	s := storage{
		Type:         "jetbrains",
		Email:        "test@example.com",
		LicenseID:    "untrusted-license",
		RefreshToken: "test-refresh",
		IDToken:      testToken("test@example.com"),
		IDExpires:    time.Now().Add(time.Hour).UnixMilli(),
	}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		urlStr, _ := req["url"].(string)
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			var body struct {
				LicenseID string `json:"licenseId"`
			}
			_ = json.Unmarshal(req["body"].([]byte), &body)
			if body.LicenseID == "untrusted-license" {
				return encode(upstreamResponse{StatusCode: 492, Body: []byte(`Untrusted license`)}), nil
			}
			if body.LicenseID == "lite-license-id" {
				return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testToken("test@example.com")})}), nil
			}
			return encode(upstreamResponse{StatusCode: 403, Body: []byte(`Forbidden`)}), nil
		}
		if strings.Contains(urlStr, "/license/obtain/grazie-lite") {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{
				"license": map[string]string{"licenseId": "lite-license-id", "type": "grazie.individual.lite"},
			})}), nil
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unknown endpoint`)}), nil
	}
	refreshed, err := refresh("cb-1", s)
	if err != nil {
		t.Fatalf("expected successful refresh with fallback, got %v", err)
	}
	if refreshed.LicenseID != "lite-license-id" || refreshed.JWT == "" {
		t.Fatalf("expected refreshed storage to have lite-license-id and JWT, got %#v", refreshed)
	}
}

func TestRefreshUpgradesFromFallbackToConfiguredLicense(t *testing.T) {
	setup(t)
	stateMu.Lock()
	configuredLicenses = []string{"valid-configured-license", "other-license"}
	stateMu.Unlock()
	s := storage{
		Type:         "jetbrains",
		Email:        "upgrade@example.com",
		LicenseID:    "lite-license-id",
		RefreshToken: "test-refresh",
		IDToken:      testToken("upgrade@example.com"),
		IDExpires:    time.Now().Add(time.Hour).UnixMilli(),
		JWT:          "old-fallback-jwt",
		JWTExpires:   time.Now().Add(time.Hour).UnixMilli(),
	}
	var activatedLicenses []string
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		urlStr, _ := req["url"].(string)
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			var body struct {
				LicenseID string `json:"licenseId"`
			}
			_ = json.Unmarshal(req["body"].([]byte), &body)
			activatedLicenses = append(activatedLicenses, body.LicenseID)
			if body.LicenseID == "valid-configured-license" {
				return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testToken("upgrade@example.com")})}), nil
			}
			return encode(upstreamResponse{StatusCode: 403, Body: []byte(`Forbidden`)}), nil
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unknown endpoint`)}), nil
	}
	refreshed, err := refresh("cb-1", s)
	if err != nil {
		t.Fatalf("expected successful upgrade, got %v", err)
	}
	if refreshed.LicenseID != "valid-configured-license" {
		t.Fatalf("expected upgraded license valid-configured-license, got %s", refreshed.LicenseID)
	}
	if len(activatedLicenses) == 0 || activatedLicenses[0] != "valid-configured-license" {
		t.Fatalf("expected valid-configured-license tried first, got %#v", activatedLicenses)
	}
}

func TestRefreshPrioritizesConfiguredLicenseWhenAlreadyAssigned(t *testing.T) {
	setup(t)
	stateMu.Lock()
	configuredLicenses = []string{"first-license", "second-license"}
	stateMu.Unlock()
	s := storage{
		Type:         "jetbrains",
		Email:        "user@example.com",
		LicenseID:    "second-license",
		RefreshToken: "test-refresh",
		IDToken:      testToken("user@example.com"),
		IDExpires:    time.Now().Add(time.Hour).UnixMilli(),
		JWT:          "second-jwt",
		JWTExpires:   time.Now().Add(time.Hour).UnixMilli(),
	}
	var activatedLicenses []string
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		urlStr, _ := req["url"].(string)
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			var body struct {
				LicenseID string `json:"licenseId"`
			}
			_ = json.Unmarshal(req["body"].([]byte), &body)
			activatedLicenses = append(activatedLicenses, body.LicenseID)
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testToken("user@example.com")})}), nil
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unknown endpoint`)}), nil
	}
	// Force activation by expiring JWT
	s.JWTExpires = 0
	refreshed, err := refresh("cb-1", s)
	if err != nil {
		t.Fatalf("expected successful refresh, got %v", err)
	}
	if refreshed.LicenseID != "second-license" {
		t.Fatalf("expected second-license retained, got %s", refreshed.LicenseID)
	}
	if len(activatedLicenses) != 1 || activatedLicenses[0] != "second-license" {
		t.Fatalf("expected only second-license tried, got %#v", activatedLicenses)
	}
}

func TestMethodAuthRefreshForcesFreshToken(t *testing.T) {
	setup(t)
	s := credential()
	tokenMu.Lock()
	tokenCache[s.Email] = s
	tokenMu.Unlock()

	calls := 0
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		urlStr, _ := req["url"].(string)
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			calls++
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testToken("test@example.com")})}), nil
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unknown endpoint`)}), nil
	}
	res, err := dispatch(pluginabi.MethodAuthRefresh, encode(rpcRequest{StorageJSON: encode(s)}))
	if err != nil {
		t.Fatal(err)
	}
	authResp := res.(pluginapi.AuthRefreshResponse)
	if calls != 1 {
		t.Fatalf("expected license activation called once on manual refresh, called %d times", calls)
	}
	if authResp.Auth.Provider != "jetbrains" {
		t.Fatalf("expected provider jetbrains, got %s", authResp.Auth.Provider)
	}
}

func TestOAuthPKCEAndPrivatePendingSessionRecovery(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	request := rpcRequest{Host: pluginapi.HostConfigSummary{AuthDir: dir}, Metadata: map[string]any{"license_id": "test-license"}}
	result, err := startLogin(request)
	if err != nil {
		t.Fatal(err)
	}
	flow := result.(pluginapi.AuthLoginStartResponse)
	address, _ := url.Parse(flow.URL)
	challenge := address.Query().Get("code_challenge")
	path, _ := loginSessionPath(dir, flow.State)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("pending verifier must be private")
	}
	clear(sessions)
	callback := encode(map[string]string{"state": flow.State, "code": "private-code"})
	if err := os.WriteFile(filepath.Join(dir, ".oauth-jetbrains-"+flow.State+".oauth"), callback, 0600); err != nil {
		t.Fatal(err)
	}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		if method == pluginabi.MethodHostAuthSave {
			return encode(map[string]any{}), nil
		}
		if req["url"] == tokenURL {
			form, _ := url.ParseQuery(string(req["body"].([]byte)))
			hash := sha256.Sum256([]byte(form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(hash[:]) != challenge || form.Get("code") != "private-code" || form.Get("redirect_uri") != redirectURI {
				t.Fatal("OAuth token request does not match its authorization")
			}
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"id_token": testToken("test@example.com"), "refresh_token": "private-refresh"})}), nil
		}
		return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testToken("test@example.com")})}), nil
	}
	request.State = flow.State
	if _, err := pollLogin(request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("consumed PKCE verifier must be removed")
	}
}
func TestAggregateToolFragmentsAndImages(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"Look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]}`)
	raw, err := aggregateRequest(input, "google-chat-gemini-flash")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"mediaType":"image/png"`) || !strings.Contains(string(raw), "llm.parameters.tools") {
		t.Fatal("missing image or tools")
	}
	events := `data: {"type":"ToolCall","parallelToolIndex":0,"id":"call-1","name":"read"}

data: {"type":"ToolCall","parallelToolIndex":0,"content":"{\"path\":\"x\"}"}

data: {"type":"FinishMetadata","reason":"tool_call"}

data: end

`
	result, err := aggregateResponse([]byte(events), "model")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if json.Unmarshal(result, &doc) != nil {
		t.Fatal("invalid result")
	}
	if !strings.Contains(string(result), `"finish_reason":"tool_calls"`) || !strings.Contains(string(result), "call-1") {
		t.Fatal("lost tool identity or finish reason")
	}
	if _, err := aggregateResponse([]byte("data: {\"type\":\"Content\",\"content\":\"partial\"}\n\n"), "model"); err == nil {
		t.Fatal("accepted interrupted response")
	}
}
func TestProviderErrorsKeepQuotaStatusAndHideBodies(t *testing.T) {
	setup(t)
	hostCall = func(string, any) (json.RawMessage, error) {
		return encode(upstreamResponse{StatusCode: 477, Body: []byte("private-provider-body")}), nil
	}
	_, err := checked("", "model request", "POST", apiBase, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "private-provider-body") {
		t.Fatal(fmt.Sprint(err))
	}
}

func TestOAuthErrorIncludesOnlyStandardCode(t *testing.T) {
	setup(t)
	for _, code := range []string{"invalid_grant", "private-upstream-text"} {
		hostCall = func(string, any) (json.RawMessage, error) {
			return encode(upstreamResponse{StatusCode: 400, Body: encode(map[string]string{"error": code, "error_description": "private-provider-body"})}), nil
		}
		_, err := checked("", "OAuth exchange", "POST", tokenURL, nil, nil)
		if err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatal("leaked OAuth error body")
		}
		if code == "invalid_grant" && !strings.Contains(err.Error(), code) {
			t.Fatal("missing standard OAuth error code")
		}
	}
}

func TestChatCompletionsTranslateToNativeResponses(t *testing.T) {
	input := []byte(`{"model":"jetbrains/gpt-5-nano","messages":[{"role":"user","content":"OK"}],"max_tokens":64}`)
	target, _ := formatRoute(modelRoute{Family: "openai", Native: "gpt-5-codex"}, translator.FormatOpenAI)
	output, err := translateRequest(translator.FormatOpenAI, target, "gpt-5-codex", input, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `"input"`) {
		t.Fatal("missing Responses input")
	}
}

func TestChatStreamPayloadAvoidsDoubleSSEFraming(t *testing.T) {
	for _, frame := range []string{"data: {\"choices\":[]}\n\n", "data: {\"choices\":[]}\r\n\r\n", "{\"choices\":[]}"} {
		if got := string(clientStreamPayload(translator.FormatOpenAI, []byte(frame))); got != `{"choices":[]}` {
			t.Fatalf("invalid Chat Completions stream payload %q", got)
		}
	}
	if got := clientStreamPayload(translator.FormatOpenAI, []byte("data: [DONE]\n\n")); len(got) != 0 {
		t.Fatal("the host must emit the terminal marker")
	}
	frame := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	if got := string(clientStreamPayload(translator.FormatClaude, []byte(frame))); got != frame {
		t.Fatal("lost native Messages framing")
	}
}

func TestNativeChatStreamRequiresUsage(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":"OK"}],"stream_options":{"include_usage":false}}`)
	out, err := translateRequest(translator.FormatOpenAI, translator.FormatOpenAI, "gpt-4o-mini", input, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"include_usage":true`) {
		t.Fatal("JetBrains requires usage on streaming Chat Completions")
	}
}

func TestNativeClaudeEventsTranslateToChatStream(t *testing.T) {
	frame := []byte("event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\r\n\r\n")
	var state any
	out := registry.TranslateStream(context.Background(), translator.FormatClaude, translator.FormatOpenAI, "model", []byte(`{"stream":true}`), nil, sse.DataFrame(frame), &state)
	if len(out) == 0 || !strings.Contains(string(out[0]), `"content":"OK"`) {
		t.Fatal("lost Claude text while translating native SSE to Chat Completions")
	}
}

func TestSignInWithoutConfiguredLicenseDiscoversActiveLicense(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	result, err := startLogin(rpcRequest{Host: pluginapi.HostConfigSummary{AuthDir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	flow := result.(pluginapi.AuthLoginStartResponse)
	if err := os.WriteFile(filepath.Join(dir, ".oauth-jetbrains-"+flow.State+".oauth"), encode(map[string]string{"state": flow.State, "code": "private-code"}), 0600); err != nil {
		t.Fatal(err)
	}
	obtainedLite := false
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		if method == pluginabi.MethodHostAuthSave {
			return encode(map[string]any{}), nil
		}
		urlStr, _ := req["url"].(string)
		if urlStr == tokenURL {
			form, _ := url.ParseQuery(string(req["body"].([]byte)))
			if form.Get("grant_type") == "switch_audience" && form.Get("audience") == jcpUserManagementAudience {
				return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{"access_token": "jcp-mgmt-token", "refresh_token": "private-refresh"})}), nil
			}
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{"id_token": testToken("user@example.com"), "refresh_token": "private-refresh"})}), nil
		}
		if strings.Contains(urlStr, "/user-management/api/ai-access-options") {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{
				"aiAccessOptions": []map[string]any{
					{"type": "workspace", "workspaceId": "ws-1", "aiAccessEnabled": false},
					{"type": "license", "name": "EDUP", "code": "EDUP", "fullName": "JetBrains AI Free", "licenseId": "EDULICENSE1", "aiAccessEnabled": true},
					{"type": "license", "name": "AIP", "code": "AIP", "fullName": "JetBrains AI Pro", "licenseId": "PROLICENSE1", "aiAccessEnabled": true},
				},
			})}), nil
		}
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			var body struct {
				LicenseID string `json:"licenseId"`
			}
			_ = json.Unmarshal(req["body"].([]byte), &body)
			if body.LicenseID == "PROLICENSE1" {
				return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testJWT("user@example.com", "jetbrains-ai.individual.pro")})}), nil
			}
			return encode(upstreamResponse{StatusCode: 403, Body: []byte(`Forbidden`)}), nil
		}
		if strings.Contains(urlStr, "/license/obtain/grazie-lite") {
			obtainedLite = true
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unexpected endpoint`)}), nil
	}
	pollRes, err := pollLogin(rpcRequest{State: flow.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}})
	if err != nil {
		t.Fatalf("expected pollLogin to succeed with discovered license, got %v", err)
	}
	if obtainedLite {
		t.Fatal("should not fall back to grazie-lite when an active AI license was discovered")
	}
	var auth storage
	if err := json.Unmarshal(pollRes.(pluginapi.AuthLoginPollResponse).Auth.StorageJSON, &auth); err != nil || auth.LicenseID != "PROLICENSE1" {
		t.Fatalf("expected discovered PROLICENSE1 prioritized over free, got %#v", auth)
	}
}

func TestRefreshUpgradesFromGrazieLiteToDiscoveredLicenseWithoutConfig(t *testing.T) {
	setup(t)
	s := storage{
		Type:         "jetbrains",
		Email:        "github3@example.com",
		LicenseID:    "6E95FASO1S",
		RefreshToken: "test-refresh",
		IDToken:      testToken("github3@example.com"),
		IDExpires:    time.Now().Add(time.Hour).UnixMilli(),
		JWT:          testJWT("github3@example.com", "grazie.individual.lite"),
		JWTExpires:   time.Now().Add(time.Hour).UnixMilli(),
	}
	saved := false
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		if method == pluginabi.MethodHostAuthSave {
			var persisted storage
			if json.Unmarshal(req["json"].(json.RawMessage), &persisted) == nil && persisted.LicenseID == "EDULICENSE1" {
				saved = true
			}
			return encode(map[string]any{}), nil
		}
		urlStr, _ := req["url"].(string)
		if urlStr == tokenURL {
			form, _ := url.ParseQuery(string(req["body"].([]byte)))
			if form.Get("grant_type") == "switch_audience" && form.Get("audience") == jcpUserManagementAudience {
				return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{"access_token": "jcp-mgmt-token", "refresh_token": "test-refresh"})}), nil
			}
		}
		if strings.Contains(urlStr, "/user-management/api/ai-access-options") {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{
				"aiAccessOptions": []map[string]any{
					{"type": "license", "name": "EDUP", "code": "EDUP", "fullName": "JetBrains AI Free", "licenseId": "EDULICENSE1", "aiAccessEnabled": true},
				},
			})}), nil
		}
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			var body struct {
				LicenseID string `json:"licenseId"`
			}
			_ = json.Unmarshal(req["body"].([]byte), &body)
			if body.LicenseID == "EDULICENSE1" {
				return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testJWT("github3@example.com", "jetbrains-ai.individual.free-tier")})}), nil
			}
			return encode(upstreamResponse{StatusCode: 403, Body: []byte(`Forbidden`)}), nil
		}
		if urlStr == apiBase+"/user/v5/quota/get" {
			return encode(upstreamResponse{StatusCode: 200, Body: []byte(`{"current":{"tariffQuota":{"current":{"amount":"769.015"},"maximum":{"amount":"300000.0"}}}}`)}), nil
		}
		if urlStr == apiBase+"/user/v5/quota/metadata/refill" {
			return encode(upstreamResponse{StatusCode: 200, Body: []byte(`{"current":{"next":1797097623540}}`)}), nil
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unexpected endpoint`)}), nil
	}
	res, err := quota(encode(pluginapi.QuotaFetchRequest{AuthID: fileName("github3@example.com"), StorageJSON: encode(s)}))
	if err != nil {
		t.Fatalf("expected quota fetch after upgrade to succeed, got %v", err)
	}
	if !saved {
		t.Fatal("expected upgraded license EDULICENSE1 to be persisted")
	}
	resp := res.(pluginapi.QuotaFetchResponse)
	if len(resp.Summary) != 2 || resp.Summary[1].Value != 300000 {
		t.Fatalf("expected upgraded quota limit 300000, got %#v", resp.Summary)
	}

	// Verify MethodAuthRefresh also upgrades a grazie.individual.lite JWT even after clearing s.JWT
	setup(t)
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		urlStr, _ := req["url"].(string)
		if urlStr == tokenURL {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{"access_token": "jcp-mgmt-token", "refresh_token": "test-refresh"})}), nil
		}
		if strings.Contains(urlStr, "/user-management/api/ai-access-options") {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]any{
				"aiAccessOptions": []map[string]any{
					{"type": "license", "name": "EDUP", "code": "EDUP", "fullName": "JetBrains AI Free", "licenseId": "EDULICENSE1", "aiAccessEnabled": true},
				},
			})}), nil
		}
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			var body struct {
				LicenseID string `json:"licenseId"`
			}
			_ = json.Unmarshal(req["body"].([]byte), &body)
			if body.LicenseID == "EDULICENSE1" {
				return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testJWT("github3@example.com", "jetbrains-ai.individual.free-tier")})}), nil
			}
			return encode(upstreamResponse{StatusCode: 403, Body: []byte(`Forbidden`)}), nil
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unexpected endpoint`)}), nil
	}
	refRes, err := dispatch(pluginabi.MethodAuthRefresh, encode(rpcRequest{StorageJSON: encode(s)}))
	if err != nil {
		t.Fatalf("MethodAuthRefresh failed: %v", err)
	}
	var refreshed storage
	if err := json.Unmarshal(refRes.(pluginapi.AuthRefreshResponse).Auth.StorageJSON, &refreshed); err != nil || refreshed.LicenseID != "EDULICENSE1" {
		t.Fatalf("expected MethodAuthRefresh to upgrade to EDULICENSE1, got %#v", refreshed)
	}
}

func TestRefreshKeepsGrazieLiteWhenDiscoveryUnavailable(t *testing.T) {
	setup(t)
	s := storage{
		Type:         "jetbrains",
		Email:        "lite@example.com",
		LicenseID:    "6E95FASO1S",
		RefreshToken: "test-refresh",
		IDToken:      testToken("lite@example.com"),
		IDExpires:    time.Now().Add(time.Hour).UnixMilli(),
		JWT:          testJWT("lite@example.com", "grazie.individual.lite"),
		JWTExpires:   time.Now().Add(time.Hour).UnixMilli(),
	}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(map[string]any)
		urlStr, _ := req["url"].(string)
		if urlStr == tokenURL {
			return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Discovery unavailable`)}), nil
		}
		if strings.Contains(urlStr, "/provide-access/license/v2") {
			return encode(upstreamResponse{StatusCode: 200, Body: encode(map[string]string{"token": testJWT("lite@example.com", "grazie.individual.lite")})}), nil
		}
		return encode(upstreamResponse{StatusCode: 500, Body: []byte(`Unexpected endpoint`)}), nil
	}
	res, err := refresh("cb-1", s)
	if err != nil {
		t.Fatalf("expected refresh to keep working when discovery fails, got %v", err)
	}
	if res.LicenseID != "6E95FASO1S" {
		t.Fatalf("expected 6E95FASO1S retained, got %s", res.LicenseID)
	}
}
