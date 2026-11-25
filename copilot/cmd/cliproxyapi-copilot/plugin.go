// Command cliproxyapi-copilot is the GitHub Copilot provider plugin, built
// with -buildmode=c-shared.
package main

import (
	"context"

	"github.com/BierDav/cliproxyapi-plugins/copilot/internal/provider"
	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/BierDav/cliproxyapi-plugins/framework/abi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const pluginVersion = "0.1.0"

var host = abi.Host()
var pluginService = provider.New(host)

func init() { abi.Serve(newPlugin()) }

func main() {}

type rpcAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthRefreshRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func newPlugin() *framework.Plugin {
	ctx := context.Background()
	p := &framework.Plugin{
		Identifier:   provider.ProviderID,
		Metadata:     pluginMetadata(),
		Capabilities: pluginCapabilities(),
		Configure:    pluginService.Configure,
		Shutdown:     pluginService.Shutdown,
		ErrorCode:    "plugin_error",
	}
	p.Handle(pluginabi.MethodQuotaDescribe, framework.Decode(func(req pluginapi.QuotaDescribeRequest) (any, error) {
		return copilotQuotaDescribe(req)
	}))
	p.Handle(pluginabi.MethodQuotaFetch, framework.Decode(func(req rpcQuotaFetchRequest) (any, error) {
		return copilotQuotaFetch(ctx, req)
	}))
	p.Handle(pluginabi.MethodQuotaReset, func([]byte) (any, error) {
		return pluginapi.QuotaResetResponse{Success: false, Message: "GitHub Copilot quota cannot be reset by the provider"}, nil
	})
	p.Handle(pluginabi.MethodManagementRegister, func([]byte) (any, error) {
		return pluginapi.ManagementRegistrationResponse{Routes: []pluginapi.ManagementRoute{{Method: "GET", Path: "/copilot-quota"}}}, nil
	})
	p.Handle(pluginabi.MethodManagementHandle, func(request []byte) (any, error) {
		return copilotQuota(request, host)
	})
	p.Handle(pluginabi.MethodModelStatic, func([]byte) (any, error) {
		return pluginService.StaticModels(), nil
	})
	p.Handle(pluginabi.MethodModelForAuth, framework.Decode(func(req rpcAuthModelRequest) (any, error) {
		return pluginService.ModelsForAuth(ctx, req.HostCallbackID, req.AuthModelRequest)
	}))
	p.Handle(pluginabi.MethodAuthParse, framework.Decode(func(req pluginapi.AuthParseRequest) (any, error) {
		return pluginService.ParseAuth(req)
	}))
	p.Handle(pluginabi.MethodAuthLoginStart, framework.Decode(func(req rpcAuthLoginStartRequest) (any, error) {
		return pluginService.StartLogin(ctx, req.HostCallbackID)
	}))
	p.Handle(pluginabi.MethodAuthLoginPoll, framework.Decode(func(req rpcAuthLoginPollRequest) (any, error) {
		return pluginService.PollLogin(ctx, req.HostCallbackID, req.State)
	}))
	p.Handle(pluginabi.MethodAuthRefresh, framework.Decode(func(req rpcAuthRefreshRequest) (any, error) {
		return pluginService.RefreshAuth(ctx, req.HostCallbackID, req.AuthRefreshRequest)
	}))
	p.Handle(pluginabi.MethodExecutorExecute, framework.Decode(func(req provider.ExecuteRequest) (any, error) {
		return pluginService.Execute(ctx, req)
	}))
	p.Handle(pluginabi.MethodExecutorExecuteStream, framework.Decode(func(req provider.ExecuteRequest) (any, error) {
		headers, err := pluginService.ExecuteStream(ctx, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"headers": headers}, nil
	}))
	p.Handle(pluginabi.MethodExecutorCountTokens, framework.Decode(func(req provider.ExecuteRequest) (any, error) {
		return pluginService.CountTokens(req)
	}))
	p.Handle(pluginabi.MethodExecutorHTTPRequest, framework.Decode(func(req provider.HTTPRequest) (any, error) {
		return pluginService.HTTP(ctx, req)
	}))
	return p
}

// pluginRegistration is what plugin.register returns.
func pluginRegistration() framework.Registration {
	return (&framework.Plugin{Metadata: pluginMetadata(), Capabilities: pluginCapabilities()}).Registration()
}

func pluginMetadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             "Copilot",
		Version:          pluginVersion,
		Author:           "Prox",
		GitHubRepository: "https://github.com/BierDav/cliproxyapi-plugins/tree/main/copilot",
		Logo:             "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHNoYXBlLXJlbmRlcmluZz0iZ2VvbWV0cmljUHJlY2lzaW9uIiB0ZXh0LXJlbmRlcmluZz0iZ2VvbWV0cmljUHJlY2lzaW9uIiBpbWFnZS1yZW5kZXJpbmc9Im9wdGltaXplUXVhbGl0eSIgZmlsbC1ydWxlPSJldmVub2RkIiBjbGlwLXJ1bGU9ImV2ZW5vZGQiIHZpZXdCb3g9IjAgMCA1MTIgNDE1Ljk5NyI+PHBhdGggZmlsbD0iI2ZmZiIgZmlsbC1ydWxlPSJub256ZXJvIiBkPSJNMTgxLjMzIDI2Ni4xNDFjMC0xMS40OTYgOS4zMi0yMC44MTcgMjAuODE4LTIwLjgxNyAxMS40OTggMCAyMC44MTkgOS4zMjEgMjAuODE5IDIwLjgxN3YzOC4zNzNjMCAxMS40OTctOS4zMjEgMjAuODE4LTIwLjgxOSAyMC44MTgtMTEuNDk3IDAtMjAuODE4LTkuMzItMjAuODE4LTIwLjgxOHYtMzguMzczek01MTIgMjQ2LjM5MXY1Ny4zODRjLS4wMiA3LjQxMS0zLjY5NiAxNC42MzgtOS42NyAxOS4wMTEtNzAuNTY1IDUxLjY1NS0xNTcuNjM2IDkzLjIxMS0yNDYuMzMgOTMuMjExLTk4LjEzOCAwLTE5Ni4zNzgtNTYuNTQyLTI0Ni4zMjktOTMuMjA5LTUuOTc1LTQuMzc0LTkuNjUtMTEuNi05LjY3MS0xOS4wMTJ2LTU3LjM4NGwxNS41ODMtMjEuMDg1YzguMzM2LTExLjMxMiAyMC43NTctMTQuMzEgMzMuOTgtMTQuMzEgNC45ODctNTYuOTUyIDE2Ljc5My05Ny42MDMgNDUuMDIzLTEyNy4zNTNDMTU1LjE5NCA1Ljc3IDIyNi41NiAwIDI1NiAwYzI5LjQ0IDAgMTAwLjgwNiA1Ljc3IDE1NC41NTUgNjIuNzIyIDI4LjE5IDI5Ljc1IDQwLjAzNiA3MC40IDQ1LjAyNSAxMjcuMzUzIDEzLjI2MyAwIDI1LjYwMiAyLjkzNiAzMy45NTggMTQuMzFsMTUuNTgzIDIxLjEyN2M0LjQ3NiA2LjA3NyA2Ljg3OCAxMy4zNDUgNi44NzggMjAuODhsLjAwMS0uMDAxem0tOTcuNjY1LTI2LjA3NGMtLjY3Ny0xMy4wNTgtMTEuMjkzLTE4LjE5LTIyLjMzOS0yMS44MjQtMTEuNjQgNy4zMDgtMjUuODQ4IDEwLjE4My0zOS40NTkgMTAuMTgzLTE0LjQ1NCAwLTQxLjQzMi0zLjQ3LTYzLjg3Mi0yNS44NjktNS42NjctNS42MjUtOS41MjctMTQuNDU0LTEyLjE1NS0yNC4yNDdsLS41NDQtLjAwMWgtLjU0NmMtNi4wOTggMC0xMy4wOTguMzQ5LTIwLjU1IDEuMDg5LTIuNjI4IDkuNzkzLTYuNTEgMTguNjIyLTEyLjE1NSAyNC4yNDctMjIuNCAyMi40LTQ5LjQxOCAyNS44Ny02My44NzIgMjUuODctMTMuNjEyIDAtMjcuODYtMi44NTYtMzkuNTAxLTEwLjE4NC0xMS4wMDUgMy42MTItMjEuNTU4IDguODI3LTIyLjI3NyAyMS44MjQtMS4xNyAyNC41NTQtMS4yNzIgNDkuMTA5LTEuMzc1IDczLjY0NC0uMDQgMTIuMzE4LS4wODIgMjQuNjU4LS4yODggMzYuOTc1LjA2MiA3LjE2NiA0LjM3NCAxMy44MTggMTAuODgyIDE2Ljc3NCA1Mi45NyAyNC4xMjQgMTAzLjA0NSAzNi4yNzggMTQ5LjEzNiAzNi4yNzggNDYuMDEgMCA5Ni4wODUtMTIuMTU0IDE0OS4wMTMtMzYuMjc4IDYuNTA4LTIuOTU2IDEwLjg0LTkuNjA4IDEwLjg4MS0xNi43NzQuNjM3LTM2LjgzMS4xMjQtNzMuODA4LTEuNjQyLTExMC42MTlsLjA0My4wMDEtMTkuMzgtMS4wODl6bS0zMDYuODE0LTUxLjM0OGM4LjY0MyA4LjYyMyAyNC45NjYgMTQuMzkyIDQyLjU2IDE0LjM5MiAxMy40NDggMCAzOS4wMy0yLjg3NCA2MC4xNTYtMjQuMzI5IDkuMjgtOC45NTEgMTUuMDUtMzEuMzUgMTQuNDEzLTU0LjA3OC0uNjU3LTE4LjIzMS01Ljc2OS0zMy4yOC0xMy40NDgtMzkuNjY1LTguMzE1LTcuMzcxLTI3LjIwMy0xMC41NzQtNDguMzMtOC42NDQtMjIuMzk5IDIuMjM3LTQxLjI2NyA5LjU4Ny01MC44NzUgMTkuODMzLTIwLjc5NyAyMi43MjgtMTYuMzIzIDgwLjMxNi00LjQ3NiA5Mi40OTF6bTEzMC41NTYtNTYuMDA4Yy42MzcgMy41MS45NjUgNy4zNSAxLjI3MyAxMS41MTcgMCAyLjg3NSAwIDUuNzctLjMwOCA4Ljk1MiA2LjQwNi0uNjM2IDExLjg0Ny0uNjM2IDE2Ljk1OS0uNjM2czEwLjU1MiAwIDE2Ljk1OC42MzZjLS4zMjgtMy4xODItLjMyOC02LjA3Ny0uMzI4LTguOTUyLjMyOC00LjE2Ny42NTYtOC4wMDcgMS4yOTQtMTEuNTE3LTYuNzM1LS42MzctMTIuODEyLS45NjUtMTcuOTI0LS45NjVzLTExLjIxLjMyOC0xNy45MjQuOTY1em00OS4yNzQtOC4wMDhjLS42MzcgMjIuNzI4IDUuMTMzIDQ1LjEyOCAxNC40MTMgNTQuMDggMjEuMTA1IDIxLjQ1NCA0Ni43MDggMjQuMzI4IDYwLjE1NCAyNC4zMjggMTcuNTk2IDAgMzMuOTE4LTUuNzY5IDQyLjU2MS0xNC4zOTIgMTEuODQ3LTEyLjE3NSAxNi4zMjItNjkuNzY0LTQuNDc2LTkyLjQ5MS05LjYwOC0xMC4yNDYtMjguNDc2LTE3LjU5NS01MC44NzUtMTkuODMzLTIxLjEyNy0xLjkzLTQwLjAxNSAxLjI3My00OC4zMyA4LjY0NC03LjY3OCA2LjM4NS0xMi43OSAyMS40MzQtMTMuNDQ3IDM5LjY2NXYtLjAwMXptMjEuNDU1IDE0MC4zNzFjLTExLjQ3NyAwLTIwLjc5OCA5LjMyMS0yMC43OTggMjAuODE3djM4LjM3M2MwIDExLjQ5NyA5LjMyIDIwLjgxOCAyMC43OTggMjAuODE4IDExLjQ5NyAwIDIwLjgxOC05LjMyIDIwLjgxOC0yMC44MTh2LTM4LjM3M2MwLTExLjQ5Ni05LjMyLTIwLjgxNy0yMC44MTgtMjAuODE3eiIvPjwvc3ZnPg==",
		ConfigFields: []pluginapi.ConfigField{
			{Name: "github_client_id", Type: pluginapi.ConfigFieldTypeString, Description: "Public GitHub OAuth application client identifier used for device flow."},
			{Name: "github_scope", Type: pluginapi.ConfigFieldTypeString, Description: "Space-delimited GitHub OAuth scopes; defaults to the least-privilege read:user scope."},
			{Name: "github_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "GitHub web OAuth base URL."},
			{Name: "github_api_url", Type: pluginapi.ConfigFieldTypeString, Description: "GitHub REST API base URL used for identity and Copilot token exchange."},
			{Name: "copilot_api_url", Type: pluginapi.ConfigFieldTypeString, Description: "Fallback Copilot API base URL when the token response has no API endpoint."},
			{Name: "oauth_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum device-code lifetime accepted by the plugin."},
			{Name: "model_cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "In-memory Copilot model catalog cache lifetime."},
			{Name: "token_expiry_buffer_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Refresh Copilot API tokens this long before expiration."},
			{Name: "excluded_model_prefixes", Type: pluginapi.ConfigFieldTypeArray, Description: "Case-insensitive model ID prefixes omitted from Copilot discovery to prevent collisions with native providers."},
		},
	}
}

func pluginCapabilities() framework.Capabilities {
	return framework.Capabilities{
		ManagementAPI:         true,
		ModelProvider:         true,
		AuthProvider:          true,
		QuotaProvider:         true,
		Executor:              true,
		ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
		ExecutorInputFormats:  []string{"openai-response", "claude", "openai"},
		ExecutorOutputFormats: []string{"openai-response", "claude", "openai"},
	}
}
