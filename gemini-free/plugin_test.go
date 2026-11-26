package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	fakeClient = "test.apps.googleusercontent.com"
	fakeSub    = "stable-google-subject"
	fakeKey    = "gemini-auth-key"
	projNumber = "123456"
	// pooledModel is served besides the enrollment probe model in tests.
	pooledModel = "gemini-4-hydrogen"
	vertexModel = "gemini-4-neon"
	trialProj   = "trial-project-1"
)

type scenario struct {
	projectCreated, serviceAccountCreated, keyCreated bool
	billing, needsVerification                        bool
	project, state, challenge                         string
	requests, generate                                int
	geminiEnableAttempts                              int
	transientEnableError, permanentEnableError        bool
	// vertex configures Vertex AI models; trialBilling is the billing state
	// of the account's billed project ("" for none).
	vertex                    bool
	trialBilling              string
	refreshes, vertexRequests int
}

func TestStreamResponsesThroughTerminalEvent(t *testing.T) {
	c := credential{Type: provider, Project: "cliproxy-gf-0000000000000000", Email: "owner@example.test", Key: fakeKey}
	var frames strings.Builder
	finished := make(chan string, 1)
	reads := 0
	response := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"billing"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}` + "\n\n"
	p := newProvider(framework.Host{Call: func(name string, input any) (json.RawMessage, error) {
		switch name {
		case pluginabi.MethodHostHTTPDoStream:
			if input.(map[string]any)["url"] != geminiAPI+"models/"+probeModel+":streamGenerateContent?alt=sse" {
				return nil, fmt.Errorf("incorrect upstream streaming URL")
			}
			return json.RawMessage(`{"status_code":200,"stream_id":"source"}`), nil
		case pluginabi.MethodHostHTTPStreamRead:
			reads++
			if reads == 1 {
				return json.Marshal(framework.StreamChunk{Payload: []byte(response[:44])})
			}
			return json.Marshal(framework.StreamChunk{Payload: []byte(response[44:]), Done: true})
		case pluginabi.MethodHostStreamEmit:
			frames.Write(input.(map[string]any)["payload"].([]byte))
		case pluginabi.MethodHostStreamClose:
			finished <- input.(map[string]string)["error"]
		case pluginabi.MethodHostHTTPStreamClose:
		default:
			return nil, fmt.Errorf("unexpected stream callback %s", name)
		}
		return json.RawMessage(`{}`), nil
	}})
	payload := []byte(`{"input":"Classify this sample"}`)
	_, err := p.execute(pluginabi.MethodExecutorExecuteStream, rpc{StreamID: "destination", ExecutorRequest: pluginapi.ExecutorRequest{
		Model: defaultPool + "/" + probeModel, StorageJSON: c.auth().StorageJSON, SourceFormat: "openai-response", OriginalRequest: payload, Payload: payload,
	}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-finished:
		if msg != "" || !strings.Contains(frames.String(), `"delta":"billing"`) ||
			!strings.Contains(frames.String(), "event: response.completed") {
			t.Fatalf("Responses stream was incomplete: %v\n%s", msg, frames.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Responses stream did not finish")
	}
}

func (s *scenario) call(t *testing.T, name string, input any) (json.RawMessage, error) {
	t.Helper()
	if name != pluginabi.MethodHostHTTPDo {
		t.Fatalf("unexpected host method %s", name)
	}
	s.requests++
	r := input.(map[string]any)
	endpoint := r["url"].(string)
	method := r["method"].(string)
	h := r["headers"].(http.Header)
	body := r["body"].([]byte)
	status, output := 200, "{}"
	project := "projects/" + s.project
	accounts := project + "/serviceAccounts"
	email := serviceAccount + "@" + s.project + ".iam.gserviceaccount.com"
	parent := "projects/" + projNumber + "/locations/global"
	switch endpoint {
	case tokenEndpoint:
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		if form.Get("grant_type") == "refresh_token" {
			s.refreshes++
			if form.Get("refresh_token") != "long-lived-refresh" || form.Get("client_secret") != "oauth-client-secret" {
				t.Fatal("Vertex AI refresh did not use the enrolled refresh token")
			}
			output = `{"access_token":"vertex-access","expires_in":3599}`
			break
		}
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if form.Get("code") != "browser-code" || form.Get("client_secret") != "oauth-client-secret" ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != s.challenge || form.Get("redirect_uri") != "https://gateway.test/v0/management/oauth-callback" {
			t.Fatal("OAuth exchange did not bind the code to its session")
		}
		output = `{"access_token":"temporary-google-token","refresh_token":"must-never-be-saved"}`
		if s.vertex {
			output = `{"access_token":"temporary-google-token","refresh_token":"long-lived-refresh"}`
		}
	case userEndpoint:
		output = `{"sub":"` + fakeSub + `","email":"owner@example.test","email_verified":true}`
	case resourceManager + project:
		if !s.projectCreated {
			status = 403
		} else {
			output = fmt.Sprintf(`{"name":"projects/%s","projectId":%q,"state":"ACTIVE","labels":{"%s":%q}}`,
				projNumber, s.project, ownerLabel, strings.TrimPrefix(s.project, "cliproxy-gf-"))
		}
	case resourceManager + "projects":
		if method != "POST" || !strings.Contains(string(body), s.project) {
			t.Fatalf("invalid project creation: %s %s", method, body)
		}
		output = `{"name":"operations/project"}`
	case resourceManager + "operations/project":
		s.projectCreated = true
		output = `{"done":true}`
	case billingAPI + project + "/billingInfo":
		if method != "GET" {
			t.Fatal("plugin attempted to change billing")
		}
		output = fmt.Sprintf(`{"billingEnabled":%t}`, s.billing)
	case serviceUsage + "projects/" + projNumber + "/services/" + geminiService + ":enable":
		s.geminiEnableAttempts++
		if s.transientEnableError && s.geminiEnableAttempts == 1 {
			status, output = 400, `{"error":{"status":"FAILED_PRECONDITION","message":"not visible yet"}}`
		} else {
			output = `{"name":"operations/enable-gemini"}`
		}
	case serviceUsage + "operations/enable-gemini",
		serviceUsage + "projects/" + projNumber + "/services/iam.googleapis.com:enable":
		output = `{"done":true}`
	case serviceUsage + "projects/" + projNumber + "/services/apikeys.googleapis.com:enable":
		if s.permanentEnableError {
			output = `{"name":"operations/enable-keys"}`
		} else {
			output = `{"done":true}`
		}
	case serviceUsage + "operations/enable-keys":
		output = `{"done":true,"error":{"code":9,"message":"DO_NOT_EXPOSE","details":[{"reason":"BILLING_NOT_ENABLED"}]}}`
	case iamAPI + accounts + "/" + email:
		if !s.serviceAccountCreated {
			status = 404
		}
	case iamAPI + accounts:
		s.serviceAccountCreated = true
	case keyAPI + parent + "/keys/" + keyID:
		if !s.keyCreated {
			status = 404
		} else {
			output = fmt.Sprintf(`{"serviceAccountEmail":%q,"restrictions":{"apiTargets":[{"service":%q}]}}`, email, geminiService)
		}
	case keyAPI + parent + "/keys?keyId=" + keyID:
		if !strings.Contains(string(body), email) || !strings.Contains(string(body), geminiService) {
			t.Fatal("plugin proposed an unbound or unrestricted key")
		}
		output = fmt.Sprintf(`{"name":%q}`, parent+"/operations/key")
	case keyAPI + parent + "/operations/key":
		if !s.keyCreated {
			s.keyCreated = true
			output = `{"done":false}` // a poll need not repeat the operation name
		} else {
			output = `{"done":true}`
		}
	case keyAPI + parent + "/keys/" + keyID + "/keyString":
		output = `{"keyString":"` + fakeKey + `"}`
	case geminiAPI + "models/" + probeModel + ":generateContent", geminiAPI + "models/" + pooledModel + ":generateContent":
		s.generate++
		if h.Get("X-Goog-Api-Key") != fakeKey || h.Get("Authorization") != "" {
			t.Fatal("Gemini should get the restricted key, never the Cloud OAuth token")
		}
		if strings.Contains(string(body), "billing") {
			t.Fatal("unexpected generation payload")
		}
		if s.needsVerification {
			status, output = 403, `{"error":{"details":[{"reason":"FREE_TIER_VERIFICATION_REQUIRED"}],"message":"key=DO_NOT_EXPOSE"}}`
		} else {
			output = `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`
		}
	case billingAPI + "billingAccounts?pageSize=50":
		// The closed account is listed first; open accounts must still win.
		output = `{"billingAccounts":[{"name":"billingAccounts/closed","open":false},{"name":"billingAccounts/hidden","open":true},
			{"name":"billingAccounts/trial","open":true}]}`
	case billingAPI + "billingAccounts/hidden/projects?pageSize=100":
		status = 403
	case billingAPI + "billingAccounts/closed/projects?pageSize=100":
		output = `{"projectBillingInfo":[{"projectId":"old-project","billingEnabled":false}]}`
	case billingAPI + "billingAccounts/trial/projects?pageSize=100":
		switch s.trialBilling {
		case "enabled":
			output = fmt.Sprintf(`{"projectBillingInfo":[{"projectId":"other-project","billingEnabled":true},{"projectId":"shared-project","billingEnabled":true},
				{"projectId":%q,"billingEnabled":true},{"projectId":%q,"billingEnabled":true}]}`, s.project, trialProj)
		case "unpaid":
			output = fmt.Sprintf(`{"projectBillingInfo":[{"projectId":%q,"billingEnabled":false}]}`, trialProj)
		default:
			output = `{"projectBillingInfo":[]}`
		}
	case resourceManager + "projects/other-project":
		output = `{"projectId":"other-project","state":"ACTIVE","createTime":"2026-03-01T00:00:00Z"}`
	case resourceManager + "projects/old-project":
		output = `{"projectId":"old-project","state":"ACTIVE","createTime":"2026-01-01T00:00:00Z"}`
	case resourceManager + "projects/shared-project":
		status = 403
	case resourceManager + "projects/" + trialProj:
		output = fmt.Sprintf(`{"projectId":%q,"state":"ACTIVE","createTime":"2026-06-01T00:00:00Z"}`, trialProj)
	case serviceUsage + "projects/" + trialProj + "/services/" + vertexService + ":enable":
		output = `{"name":"operations/enable-vertex"}`
	case serviceUsage + "projects/old-project/services/" + vertexService + ":enable":
		// Google refuses Vertex AI on a project whose billing account is closed.
		status, output = 400, `{"error":{"status":"FAILED_PRECONDITION","details":[{"reason":"BILLING_DISABLED"}]}}`
	case serviceUsage + "operations/enable-vertex":
		output = `{"done":true}`
	case vertexAPI + "projects/" + trialProj + "/locations/global/publishers/google/models/" + vertexModel + ":generateContent",
		vertexAPI + "projects/" + trialProj + "/locations/global/publishers/google/models/" + vertexModel + ":countTokens":
		s.vertexRequests++
		if h.Get("Authorization") != "Bearer vertex-access" || h.Get("X-Goog-Api-Key") != "" {
			t.Fatal("Vertex AI should get the refreshed user token, never the Gemini key")
		}
		if !strings.Contains(string(body), `"role":"user"`) ||
			(strings.HasSuffix(endpoint, ":countTokens") && strings.Contains(string(body), "safetySettings")) {
			t.Fatalf("Vertex AI request was not adapted: %s", body)
		}
		output = `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`
		if strings.HasSuffix(endpoint, ":countTokens") {
			output = `{"totalTokens":5}`
		}
	default:
		t.Fatalf("unexpected Google request %s %s", method, endpoint)
	}
	raw, err := json.Marshal(framework.HTTPResponse{StatusCode: status, Body: []byte(output)})
	return raw, err
}

func (s *scenario) start(t *testing.T) (*providerPlugin, rpc) {
	t.Helper()
	t.Setenv("CLIPROXY_GEMINI_FREE_OAUTH_CLIENT_SECRET", "oauth-client-secret")
	p := newProvider(framework.Host{Call: func(name string, input any) (json.RawMessage, error) { return s.call(t, name, input) }})
	config := "oauth_client_id: " + fakeClient + "\npublic_base_url: https://gateway.test/\nmodels: " + pooledModel + ", " + probeModel + "\n"
	access := "online"
	if s.vertex {
		config += "vertex_models: " + vertexModel + "\n"
		access = "offline"
	}
	if err := p.configure([]byte(config)); err != nil {
		t.Fatal(err)
	}
	req := rpc{Host: pluginapi.HostConfigSummary{AuthDir: t.TempDir()}}
	res, err := p.start(req)
	if err != nil {
		t.Fatal(err)
	}
	start := res.(pluginapi.AuthLoginStartResponse)
	u, _ := url.Parse(start.URL)
	if u.Hostname() != "accounts.google.com" || u.Query().Get("state") != start.State ||
		u.Query().Get("access_type") != access || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected Google OAuth redirect %s", u)
	}
	s.state, s.challenge = start.State, u.Query().Get("code_challenge")
	digest := sha256.Sum256([]byte(fakeClient + "\x00" + fakeSub))
	s.project = fmt.Sprintf("cliproxy-gf-%x", digest[:8])
	req.State = start.State
	res, err = p.poll(req)
	if err != nil || res.(pluginapi.AuthLoginPollResponse).Status != "pending" || s.requests != 0 {
		t.Fatalf("setup ran before the user signed in: %v", err)
	}
	data, _ := json.Marshal(map[string]string{"state": s.state, "code": "browser-code"})
	if err := os.WriteFile(callbackFile(req.Host.AuthDir, s.state), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, req
}

func finish(t *testing.T, p *providerPlugin, r rpc) (pluginapi.AuthData, error) {
	t.Helper()
	for i := 0; i < 25; i++ {
		got, err := p.poll(r)
		if err != nil {
			return pluginapi.AuthData{}, err
		}
		poll := got.(pluginapi.AuthLoginPollResponse)
		if poll.Status == pluginapi.AuthLoginStatusSuccess {
			return poll.Auth, nil
		}
	}
	t.Fatal("Google enrollment did not finish")
	return pluginapi.AuthData{}, nil
}

func TestEnrollThenRunClassification(t *testing.T) {
	s := &scenario{}
	p, req := s.start(t)
	auth, err := finish(t, p, req)
	if err != nil {
		t.Fatal(err)
	}
	if !s.projectCreated || !s.serviceAccountCreated || !s.keyCreated || s.generate != 1 || auth.FileName != s.project+".json" {
		t.Fatal("expected a verified project with a restricted key")
	}
	if data := string(auth.StorageJSON); strings.Contains(data, "temporary-google-token") || strings.Contains(data, "must-never-be-saved") {
		t.Fatal("Cloud OAuth tokens were persisted")
	}
	_, err = os.Stat(callbackFile(req.Host.AuthDir, req.State))
	if !os.IsNotExist(err) {
		t.Fatal("OAuth callback file was retained")
	}
	plugin := p.plugin()
	payload, _ := json.Marshal(rpc{ExecutorRequest: pluginapi.ExecutorRequest{StorageJSON: auth.StorageJSON}})
	result, err := plugin.Dispatch(pluginabi.MethodModelForAuth, payload)
	if err != nil {
		t.Fatal(err)
	}
	models := result.(pluginapi.ModelResponse).Models
	if len(models) != 2 || models[0].ID != "gcp/"+pooledModel || models[1].ID != "gcp/"+probeModel ||
		models[0].DisplayName != "Gemini 4 Hydrogen · Free" {
		t.Fatalf("expected the configured models in the gcp pool, got %+v", models)
	}
	req = rpc{ExecutorRequest: pluginapi.ExecutorRequest{Model: models[0].ID, StorageJSON: auth.StorageJSON,
		SourceFormat: "openai", Payload: []byte(`{"messages":[{"role":"user","content":"Classify this sample"}]}`)}}
	output, err := p.execute(pluginabi.MethodExecutorExecute, req)
	if err != nil || !strings.Contains(string(output.(pluginapi.ExecutorResponse).Payload), "chat.completion") {
		t.Fatalf("OpenAI-compatible generation did not complete: %v", err)
	}
	for _, other := range []string{"gcp/gemini-4-neon", pooledModel, "gemini-free-0000000000000000/" + probeModel} {
		req.Model = other
		if _, err := p.execute(pluginabi.MethodExecutorExecute, req); err == nil {
			t.Fatalf("Gemini Free accepted the unconfigured model %s", other)
		}
	}
}

func TestRefuseBillingAndReportGoogleVerification(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    scenario
		want string
	}{
		{"billing", scenario{projectCreated: true, billing: true}, "billing is linked"},
		{"verification", scenario{projectCreated: true, serviceAccountCreated: true, keyCreated: true, needsVerification: true}, "https://aistudio.google.com/verify?project=" + projNumber},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, req := tc.s.start(t)
			auth, err := finish(t, p, req)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "DO_NOT_EXPOSE") || len(auth.StorageJSON) != 0 {
				t.Fatalf("expected a sanitized, actionable enrollment error; got %v", err)
			}
			if tc.s.billing && (tc.s.generate != 0 || tc.s.serviceAccountCreated || tc.s.keyCreated) {
				t.Fatal("billing-linked project reached API key setup or generation")
			}
		})
	}
}

func TestRetryServiceEnableAfterProjectCreation(t *testing.T) {
	s := &scenario{transientEnableError: true}
	p, req := s.start(t)
	auth, err := finish(t, p, req)
	if err != nil || s.geminiEnableAttempts != 2 || len(auth.StorageJSON) == 0 {
		t.Fatalf("transient Service Usage rejection did not recover: %v", err)
	}
}

func TestServiceOperationErrorIdentifiesTheBlockedService(t *testing.T) {
	s := &scenario{projectCreated: true, permanentEnableError: true}
	p, req := s.start(t)
	auth, err := finish(t, p, req)
	if err == nil || !strings.Contains(err.Error(), "BILLING_NOT_ENABLED") ||
		!strings.Contains(err.Error(), "https://console.cloud.google.com/apis/library/apikeys.googleapis.com?project="+s.project) ||
		strings.Contains(err.Error(), "DO_NOT_EXPOSE") || len(auth.StorageJSON) != 0 || s.serviceAccountCreated {
		t.Fatalf("expected a safe diagnostic before key setup; got %v", err)
	}
}

func TestConfigureModelPool(t *testing.T) {
	p := newProvider(framework.Host{})
	if c := p.models(); c.pool != "gcp" || !slices.Equal(c.models, []string{probeModel}) {
		t.Fatalf("unexpected default catalog %+v", c)
	}
	if err := p.configure([]byte("pool: free\nmodels: models/gemini-4-hydrogen, gemini-4-hydrogen ,gemini-3.6-flash\n")); err != nil {
		t.Fatal(err)
	}
	c := p.models()
	if c.pool != "free" || !slices.Equal(c.models, []string{"gemini-4-hydrogen", "gemini-3.6-flash"}) {
		t.Fatalf("unexpected catalog %+v", c)
	}
	if name, vertex, ok := c.upstream("models/free/gemini-3.6-flash"); !ok || vertex || name != "gemini-3.6-flash" {
		t.Fatal("pooled model did not resolve to its Gemini model")
	}
	for _, bad := range []string{"pool: GCP\n", "pool: a/b\n", "models: gemini-4-hydrogen,,x\n", "models: ../secret\n",
		"models: gemini-4-neon\nvertex_models: gemini-4-neon\n", "vertex_models: a b\n"} {
		if err := p.configure([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid configuration %q", bad)
		}
	}
	if c := p.models(); c.pool != "free" {
		t.Fatal("a rejected configuration replaced the catalog")
	}
}

func TestEnrollVertexThenServeBothTiers(t *testing.T) {
	s := &scenario{vertex: true, trialBilling: "enabled"}
	p, req := s.start(t)
	auth, err := finish(t, p, req)
	if err != nil {
		t.Fatal(err)
	}
	var c credential
	if json.Unmarshal(auth.StorageJSON, &c) != nil || c.Vertex != trialProj || c.Refresh != "long-lived-refresh" || c.Key != fakeKey ||
		strings.Contains(string(auth.StorageJSON), "temporary-google-token") || auth.Label != "Gemini Free + Vertex · owner@example.test" {
		t.Fatalf("expected the billed project and refresh token in the credential, got %s", auth.StorageJSON)
	}
	payload, _ := json.Marshal(rpc{ExecutorRequest: pluginapi.ExecutorRequest{StorageJSON: auth.StorageJSON}})
	result, err := p.plugin().Dispatch(pluginabi.MethodModelForAuth, payload)
	if err != nil {
		t.Fatal(err)
	}
	models := result.(pluginapi.ModelResponse).Models
	if len(models) != 3 || models[2].ID != "gcp/"+vertexModel || models[2].DisplayName != "Gemini 4 Neon · Vertex" {
		t.Fatalf("expected the Vertex model beside the Free models, got %+v", models)
	}
	for _, m := range models {
		if m.Thinking == nil || len(m.Thinking.Levels) == 0 {
			t.Fatalf("expected %s to declare thinking support, or the host strips thinkingConfig", m.ID)
		}
	}
	for _, r := range []struct {
		method, format string
		payload        string
	}{
		{pluginabi.MethodExecutorExecute, "openai", `{"messages":[{"role":"user","content":"Classify this sample"}]}`},
		{pluginabi.MethodExecutorExecute, "gemini", `{"contents":[{"parts":[{"text":"Classify this sample"}]}]}`},
		{pluginabi.MethodExecutorCountTokens, "gemini", `{"generateContentRequest":{"model":"models/x","contents":[{"parts":[{"text":"Count"}]}],"safetySettings":[]}}`},
	} {
		_, err := p.execute(r.method, rpc{ExecutorRequest: pluginapi.ExecutorRequest{Model: "gcp/" + vertexModel,
			StorageJSON: auth.StorageJSON, SourceFormat: r.format, Payload: []byte(r.payload)}})
		if err != nil {
			t.Fatalf("Vertex AI %s from %s failed: %v", r.method, r.format, err)
		}
	}
	if s.vertexRequests != 3 || s.refreshes != 1 {
		t.Fatalf("expected 3 Vertex AI calls on one cached access token, got %d calls and %d refreshes", s.vertexRequests, s.refreshes)
	}
	free := c
	free.Vertex, free.Refresh = "", ""
	free.Project = "cliproxy-gf-1111111111111111"
	_, err = p.execute(pluginabi.MethodExecutorExecute, rpc{ExecutorRequest: pluginapi.ExecutorRequest{Model: "gcp/" + vertexModel,
		StorageJSON: free.auth().StorageJSON, SourceFormat: "gemini", Payload: []byte(`{"contents":[]}`)}})
	if err == nil {
		t.Fatal("an account without a billed project served a Vertex AI model")
	}
}

func TestEnrollVertexBillingStates(t *testing.T) {
	for _, tc := range []struct{ billing, want string }{{"unpaid", trialProj}, {"", "old-project"}} {
		t.Run(tc.billing, func(t *testing.T) {
			s := &scenario{vertex: true, trialBilling: tc.billing}
			p, req := s.start(t)
			auth, err := finish(t, p, req)
			var c credential
			if err != nil || json.Unmarshal(auth.StorageJSON, &c) != nil || c.Vertex != tc.want {
				t.Fatalf("expected Vertex AI in %s, got %v %s", tc.want, err, auth.StorageJSON)
			}
		})
	}
}
