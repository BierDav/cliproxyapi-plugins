package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/BierDav/cliproxyapi-plugins/framework/abi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const providerKey = "make"
const pluginVersion = "0.1.0"

// hostCall reaches the host; tests replace it.
var hostCall framework.Caller = abi.Call

func host() framework.Host { return framework.Host{Call: hostCall} }

var plugin = newPlugin()

func init() { abi.Serve(plugin) }

func main() {}

const makeLogoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#6d00cc"/><path d="M14 46V18h7l11 15 11-15h7v28h-7V30L32 44 21 30v16z" fill="#fff"/></svg>`

// pluginConfig mirrors plugins.configs.cliproxyapi-make.
type pluginConfig struct {
	// PublicBaseURL is the externally reachable CLIProxyAPI origin. When set,
	// Make redirects the browser straight back to the management OAuth
	// callback so sign-in completes without pasting the callback URL.
	PublicBaseURL string
	// TeamID selects the Make team that hosts the gateway scenario.
	TeamID int64
	// ClaudeThinking exposes the model's reasoning summary to Claude Messages
	// clients as thinking blocks. Off by default: those blocks carry no
	// Anthropic signature, so a thread containing them cannot later be resumed
	// or forked on a real Claude model.
	ClaudeThinking bool
}

var configMu sync.Mutex
var config pluginConfig

func currentConfig() pluginConfig {
	configMu.Lock()
	defer configMu.Unlock()
	return config
}

func shutdown() {
	stateMu.Lock()
	clear(sessions)
	stateMu.Unlock()
	tokenMu.Lock()
	clear(tokenCache)
	tokenMu.Unlock()
	pausedMu.Lock()
	clear(paused)
	pausedMu.Unlock()
}

func encode(v any) []byte { b, _ := json.Marshal(v); return b }

// statusError carries an HTTP status to the host without exposing upstream
// bodies or credentials.
type statusError struct {
	status  int
	message string
}

func (e *statusError) Error() string { return e.message }

// StatusCode gives the error's HTTP status to the framework.
func (e *statusError) StatusCode() int { return e.status }

func newStatusError(status int, format string, args ...any) error {
	return &statusError{status: status, message: fmt.Sprintf(format, args...)}
}

func handleMethod(method string, raw []byte) ([]byte, bool) { return plugin.Call(method, raw) }

func dispatch(method string, raw []byte) (any, error) { return plugin.Dispatch(method, raw) }

func registration() framework.Registration { return plugin.Registration() }

type rpcRequest struct {
	AuthID         string
	FileName       string
	Provider       string
	BaseURL        string
	RawJSON        []byte
	StorageJSON    []byte
	Metadata       map[string]any
	Host           pluginapi.HostConfigSummary
	State          string
	HostCallbackID string `json:"host_callback_id"`
}

// parseConfig reads plugins.configs.cliproxyapi-make.
func parseConfig(configYAML []byte) (pluginConfig, error) {
	var doc struct {
		PublicBaseURL  string `yaml:"public_base_url"`
		TeamID         any    `yaml:"team_id"`
		ClaudeThinking bool   `yaml:"claude_thinking"`
	}
	if yaml.Unmarshal(configYAML, &doc) != nil {
		return pluginConfig{}, fmt.Errorf("Invalid Make configuration")
	}
	cfg := pluginConfig{PublicBaseURL: strings.TrimRight(strings.TrimSpace(doc.PublicBaseURL), "/"), ClaudeThinking: doc.ClaudeThinking}
	switch v := doc.TeamID.(type) {
	case int:
		cfg.TeamID = int64(v)
	case int64:
		cfg.TeamID = v
	case float64:
		cfg.TeamID = int64(v)
	case string:
		if strings.TrimSpace(v) != "" {
			id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil || id <= 0 {
				return pluginConfig{}, fmt.Errorf("Invalid Make team_id")
			}
			cfg.TeamID = id
		}
	}
	if cfg.PublicBaseURL != "" && !strings.HasPrefix(cfg.PublicBaseURL, "https://") && !strings.HasPrefix(cfg.PublicBaseURL, "http://") {
		return pluginConfig{}, fmt.Errorf("Invalid Make public_base_url")
	}
	return cfg, nil
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
		Identifier: providerKey,
		Metadata: pluginapi.Metadata{Name: "Make", Version: pluginVersion, Author: "Prox",
			GitHubRepository: "https://github.com/BierDav/cliproxyapi-plugins/tree/main/make",
			Logo:             "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(makeLogoSVG)),
			ConfigFields: []pluginapi.ConfigField{
				{Name: "public_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Public CLIProxyAPI origin (for example https://ai.example.com). Make then redirects sign-ins straight back to this host."},
				{Name: "team_id", Type: pluginapi.ConfigFieldTypeString, Description: "Optional Make team ID for the gateway scenario. Defaults to the first team of the signed-in organization."},
				{Name: "claude_thinking", Type: pluginapi.ConfigFieldTypeBoolean, Default: false, Description: "Show the model's reasoning to Claude Messages clients as thinking blocks. These blocks are unsigned, so such threads cannot later continue on a real Claude model."},
			}},
		Capabilities: framework.Capabilities{AuthProvider: true, ModelProvider: true, Executor: true, QuotaProvider: true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"claude", "openai", "openai-response"},
			ExecutorOutputFormats: []string{"claude", "openai", "openai-response"}},
		Configure:               configure,
		Quiesce:                 shutdown,
		Shutdown:                shutdown,
		ErrorCode:               "make_error",
		InternalErrorsRetryable: true,
	}
	p.Handle(pluginabi.MethodAuthParse, rpc(func(r rpcRequest) (any, error) {
		var s storage
		if json.Unmarshal(r.RawJSON, &s) != nil || s.Type != providerKey {
			return pluginapi.AuthParseResponse{Handled: false}, nil
		}
		if s.RefreshToken == "" || s.ClientID == "" {
			return nil, fmt.Errorf("Make credential needs a refresh token")
		}
		return pluginapi.AuthParseResponse{Handled: true, Auth: authData(s, r.FileName)}, nil
	}))
	p.Handle(pluginabi.MethodAuthRefresh, rpc(func(r rpcRequest) (any, error) {
		s, err := refreshCredential(r.HostCallbackID, r.AuthID, r.StorageJSON)
		if err != nil {
			return nil, err
		}
		a := authData(s, r.AuthID)
		return pluginapi.AuthRefreshResponse{Auth: a, NextRefreshAfter: a.NextRefreshAfter}, nil
	}))
	p.Handle(pluginabi.MethodAuthLoginStart, rpc(startLogin))
	p.Handle(pluginabi.MethodAuthLoginPoll, rpc(pollLogin))
	p.Handle(pluginabi.MethodModelStatic, rpc(func(rpcRequest) (any, error) {
		return pluginapi.ModelResponse{Provider: providerKey, Models: []pluginapi.ModelInfo{}}, nil
	}))
	p.Handle(pluginabi.MethodModelForAuth, rpc(func(rpcRequest) (any, error) {
		return pluginapi.ModelResponse{Provider: providerKey, Models: modelInfos()}, nil
	}))
	p.Handle(pluginabi.MethodExecutorExecute, func(raw []byte) (any, error) {
		return execute(pluginabi.MethodExecutorExecute, raw)
	})
	p.Handle(pluginabi.MethodExecutorExecuteStream, func(raw []byte) (any, error) {
		return execute(pluginabi.MethodExecutorExecuteStream, raw)
	})
	p.Handle(pluginabi.MethodExecutorCountTokens, func([]byte) (any, error) {
		return nil, newStatusError(501, "Make does not support token counting; use the response usage")
	})
	p.Handle(pluginabi.MethodQuotaDescribe, func([]byte) (any, error) {
		return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{providerKey}, DisplayName: "Make", SupportsReset: false}, nil
	})
	p.Handle(pluginabi.MethodQuotaFetch, quota)
	return p
}

func configure(configYAML []byte) error {
	cfg, err := parseConfig(configYAML)
	if err != nil {
		return err
	}
	configMu.Lock()
	config = cfg
	configMu.Unlock()
	return nil
}
