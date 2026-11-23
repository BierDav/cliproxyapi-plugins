package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/BierDav/cliproxyapi-plugins/framework/abi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	provider = "gemini-free"
	// probeModel checks a new key during enrollment. It is the Free tier model
	// with the most headroom, independent of the models the gateway serves.
	probeModel    = "gemini-3.1-flash-lite"
	defaultPool   = "gcp"
	defaultModels = probeModel
)

var (
	projectPattern = regexp.MustCompile("^cliproxy-gf-[a-f0-9]{16}$")
	numberPattern  = regexp.MustCompile("^[0-9]+$")
	statePattern   = regexp.MustCompile("^[a-f0-9]{32}$")
	namePattern    = regexp.MustCompile("^[a-z0-9][a-z0-9._-]*$")
	// vertexPattern matches Google Cloud project IDs.
	vertexPattern = regexp.MustCompile("^[a-z][a-z0-9-]{4,28}[a-z0-9]$")
)

func init() { abi.Serve(newProvider(framework.Host{Call: abi.Call}).plugin()) }

func main() {}

type configuration struct {
	ClientID     string `yaml:"oauth_client_id"`
	PublicOrigin string `yaml:"public_base_url"`
	Pool         string `yaml:"pool"`
	Models       string `yaml:"models"`
	VertexModels string `yaml:"vertex_models"`
	VertexCredit string `yaml:"vertex_credit"`
}

// catalog is the set of models every enrolled account serves, published as
// <pool>/<model> so that all accounts share one pool per model. Free models
// use the account's restricted Gemini key; Vertex models call Vertex AI as
// the signed-in user, in the account's billed project.
type catalog struct {
	pool   string
	models []string
	vertex []string
}

func (c catalog) id(model string) string { return c.pool + "/" + model }

// upstream returns the Gemini model behind a pooled model ID and whether
// Vertex AI serves it.
func (c catalog) upstream(id string) (name string, vertex, ok bool) {
	name, ok = strings.CutPrefix(strings.TrimPrefix(id, "models/"), c.pool+"/")
	switch {
	case ok && slices.Contains(c.models, name):
		return name, false, true
	case ok && slices.Contains(c.vertex, name):
		return name, true, true
	}
	return "", false, false
}

func parseModels(list string) ([]string, error) {
	var out []string
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimPrefix(strings.TrimSpace(name), "models/")
		if !namePattern.MatchString(name) {
			return nil, errors.New("Gemini Free models must be a comma-separated list of Gemini model names")
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out, nil
}

func parseCatalog(cfg configuration) (catalog, error) {
	c := catalog{pool: strings.TrimSpace(cfg.Pool)}
	if c.pool == "" {
		c.pool = defaultPool
	}
	if !namePattern.MatchString(c.pool) {
		return catalog{}, errors.New("Gemini Free pool must be a lowercase model prefix such as gcp")
	}
	list := cfg.Models
	if strings.TrimSpace(list) == "" {
		list = defaultModels
	}
	var err error
	if c.models, err = parseModels(list); err != nil {
		return catalog{}, err
	}
	if strings.TrimSpace(cfg.VertexModels) != "" {
		if c.vertex, err = parseModels(cfg.VertexModels); err != nil {
			return catalog{}, err
		}
	}
	for _, name := range c.vertex {
		if slices.Contains(c.models, name) {
			return catalog{}, errors.New("Gemini Free cannot serve " + name + " from both the Free tier and Vertex AI")
		}
	}
	return c, nil
}

// displayName turns gemini-4-hydrogen into Gemini 4 Hydrogen.
func displayName(model string) string {
	words := strings.FieldsFunc(model, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

type providerPlugin struct {
	host    framework.Host
	mu      sync.Mutex
	config  configuration
	catalog catalog
	pending map[string]*enrollment
	// tokens caches Vertex AI access tokens by the SHA-256 of their refresh token.
	tokens map[string]accessToken
	// creditAmount is each account's Google Cloud credit; credits tracks the
	// credit budget of each Vertex AI project.
	creditAmount float64
	credits      map[string]*creditState
}

type accessToken struct {
	value   string
	expires time.Time
}

func newProvider(host framework.Host) *providerPlugin {
	c, _ := parseCatalog(configuration{})
	return &providerPlugin{host: host, catalog: c, pending: make(map[string]*enrollment),
		tokens: make(map[string]accessToken), credits: make(map[string]*creditState)}
}

func (p *providerPlugin) models() catalog {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.catalog
}

func (p *providerPlugin) clearPending() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.pending)
	clear(p.tokens)
}

func (p *providerPlugin) configure(raw []byte) error {
	var cfg configuration
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return errors.New("Invalid Gemini Free plugin configuration")
	}
	if cfg.PublicOrigin != "" {
		u, err := url.Parse(cfg.PublicOrigin)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return errors.New("Gemini Free public_base_url must be an HTTPS origin")
		}
		cfg.PublicOrigin = strings.TrimSuffix(cfg.PublicOrigin, "/")
	}
	if cfg.ClientID != "" && !strings.HasSuffix(cfg.ClientID, ".apps.googleusercontent.com") {
		return errors.New("Gemini Free oauth_client_id must be a Google OAuth client ID")
	}
	models, err := parseCatalog(cfg)
	if err != nil {
		return err
	}
	amount, err := parseCredit(cfg.VertexCredit)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.config.ClientID != cfg.ClientID || p.config.PublicOrigin != cfg.PublicOrigin {
		clear(p.pending)
		clear(p.tokens)
	}
	if p.creditAmount != amount {
		clear(p.credits) // look for the budget again before creating one with the new amount
	}
	p.config, p.catalog, p.creditAmount = cfg, models, amount
	return nil
}

func clientSecret() string {
	return strings.TrimSpace(os.Getenv("CLIPROXY_GEMINI_FREE_OAUTH_CLIENT_SECRET"))
}

type credential struct {
	Type    string `json:"type"`
	Project string `json:"project_id"`
	Email   string `json:"email"`
	Key     string `json:"api_key"`
	// Vertex is the account's billed project for Vertex AI models, and
	// Refresh the Google OAuth refresh token that calls Vertex AI there.
	// Both are empty when the account has no billed project.
	Vertex  string `json:"vertex_project,omitempty"`
	Refresh string `json:"refresh_token,omitempty"`
}

func (c credential) valid() bool {
	return c.Type == provider && projectPattern.MatchString(c.Project) && c.Email != "" && c.Key != "" &&
		(c.Vertex == "") == (c.Refresh == "") && (c.Vertex == "" || vertexPattern.MatchString(c.Vertex))
}

func (c credential) auth() pluginapi.AuthData {
	raw, _ := json.Marshal(c)
	filename := c.Project + ".json"
	label, metadata := "Gemini Free · "+c.Email, map[string]any{"type": provider, "email": c.Email, "project_id": c.Project}
	if c.Vertex != "" {
		label = "Gemini Free + Vertex · " + c.Email
		metadata["vertex_project"] = c.Vertex
	}
	return pluginapi.AuthData{
		Provider: provider, ID: filename, FileName: filename, Label: label,
		StorageJSON: raw, Metadata: metadata,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
}

type rpc struct {
	pluginapi.ExecutorRequest
	Host           pluginapi.HostConfigSummary
	HostCallbackID string `json:"host_callback_id"`
	StreamID       string `json:"stream_id"`
	State          string
	RawJSON        []byte
}

func (p *providerPlugin) plugin() *framework.Plugin {
	plugin := &framework.Plugin{
		Identifier: provider,
		Metadata: pluginapi.Metadata{
			Name: "Gemini Free", Version: "0.4.0", Author: "Prox",
			GitHubRepository: "https://github.com/BierDav/cliproxyapi-plugins/tree/main/gemini-free",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "oauth_client_id", Type: pluginapi.ConfigFieldTypeString, Description: "Google OAuth web client ID. The matching secret is supplied in the gateway environment."},
				{Name: "public_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "HTTPS gateway origin reachable by the browser for the Google OAuth redirect."},
				{Name: "pool", Type: pluginapi.ConfigFieldTypeString, Description: "Model prefix shared by all enrolled accounts. Defaults to gcp."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeString, Description: "Comma-separated Gemini models every account serves as <pool>/<model>. Defaults to " + defaultModels + "."},
				{Name: "vertex_models", Type: pluginapi.ConfigFieldTypeString, Description: "Comma-separated models served through Vertex AI in each account's billed project, for example gemini-4-neon. Empty disables Vertex AI."},
				{Name: "vertex_credit", Type: pluginapi.ConfigFieldTypeString, Description: "Google Cloud credit per account in its billing currency, for example 269.43. Enables a Cloud Billing budget that reports the remaining credit as quota."},
			},
		},
		Capabilities: framework.Capabilities{
			AuthProvider: true, ModelProvider: true, Executor: true, QuotaProvider: true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"gemini", "openai", "openai-response"},
			ExecutorOutputFormats: []string{"gemini", "openai", "openai-response"},
		},
		Configure: p.configure, Quiesce: p.clearPending, Shutdown: p.clearPending, ErrorCode: "gemini_free_error",
	}
	plugin.Handle(pluginabi.MethodAuthLoginStart, framework.Decode(p.start))
	plugin.Handle(pluginabi.MethodAuthLoginPoll, framework.Decode(p.poll))
	plugin.Handle(pluginabi.MethodAuthParse, framework.Decode(func(r rpc) (any, error) {
		var c credential
		if json.Unmarshal(r.RawJSON, &c) != nil || c.Type != provider {
			return pluginapi.AuthParseResponse{}, nil
		}
		if !c.valid() {
			return nil, failure(http.StatusBadRequest, "Invalid Gemini Free credential")
		}
		return pluginapi.AuthParseResponse{Handled: true, Auth: c.auth()}, nil
	}))
	// The saved credential is a restricted API key, plus a Google OAuth
	// refresh token when the account serves Vertex AI models. Access tokens
	// for Vertex AI are refreshed on demand and kept in memory.
	plugin.Handle(pluginabi.MethodAuthRefresh, framework.Decode(func(r rpc) (any, error) {
		var c credential
		if json.Unmarshal(r.StorageJSON, &c) != nil || !c.valid() {
			return nil, failure(http.StatusUnauthorized, "Invalid Gemini Free credential; enroll again")
		}
		return pluginapi.AuthRefreshResponse{Auth: c.auth()}, nil
	}))
	plugin.Handle(pluginabi.MethodQuotaDescribe, func([]byte) (any, error) {
		return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{provider}, DisplayName: "Google Cloud credits"}, nil
	})
	plugin.Handle(pluginabi.MethodQuotaFetch, framework.Decode(p.quota))
	plugin.Handle(pluginabi.MethodModelStatic, func([]byte) (any, error) {
		return pluginapi.ModelResponse{Provider: provider, Models: []pluginapi.ModelInfo{}}, nil
	})
	plugin.Handle(pluginabi.MethodModelForAuth, framework.Decode(func(r rpc) (any, error) {
		result := pluginapi.ModelResponse{Provider: provider, Models: []pluginapi.ModelInfo{}}
		var c credential
		if json.Unmarshal(r.StorageJSON, &c) != nil || !c.valid() {
			return result, nil
		}
		models := p.models()
		add := func(name, tier string) {
			id := models.id(name)
			result.Models = append(result.Models, pluginapi.ModelInfo{
				ID: id, Name: "models/" + id, Object: "model", OwnedBy: "google", Type: "gemini",
				DisplayName:                displayName(name) + " · " + tier,
				SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent", "countTokens"},
				InputTokenLimit:            1048576, OutputTokenLimit: 65536, ContextLength: 1048576, MaxCompletionTokens: 65536,
			})
		}
		for _, name := range models.models {
			add(name, "Free")
		}
		if c.Vertex != "" {
			for _, name := range models.vertex {
				add(name, "Vertex")
			}
		}
		return result, nil
	}))
	for _, method := range []string{pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream, pluginabi.MethodExecutorCountTokens} {
		plugin.Handle(method, framework.Decode(func(r rpc) (any, error) { return p.execute(method, r) }))
	}
	return plugin
}

func failure(status int, message string) error {
	return &framework.Error{HTTPStatus: status, Message: message, Retryable: framework.RetryableStatus(status)}
}
