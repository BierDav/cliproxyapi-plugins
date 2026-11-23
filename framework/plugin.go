// Package framework is the runtime the provider plugins in this repository
// share: method routing, the JSON envelope, error mapping, registration and
// the plugin lifecycle. Host callbacks go through Host, and package
// framework/abi binds a Plugin to the host's native C ABI.
//
// A plugin describes itself with a Plugin, registers one Handler per RPC
// method it serves and hands the Plugin to abi.Serve from an init function:
//
//	func init() { abi.Serve(newPlugin()) }
package framework

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Handler serves one RPC method. It receives the raw JSON request and returns
// a value that is encoded as the envelope's result.
type Handler func(request []byte) (any, error)

// Plugin is one provider plugin as the host sees it.
type Plugin struct {
	// Identifier answers auth.identifier, executor.identifier and
	// quota.identifier, for example "copilot".
	Identifier string
	// Metadata and Capabilities are returned by plugin.register and
	// plugin.reconfigure.
	Metadata     pluginapi.Metadata
	Capabilities Capabilities
	// Configure receives plugins.configs.<plugin> as YAML on plugin.register
	// and plugin.reconfigure. An error fails the call.
	Configure func(configYAML []byte) error
	// Quiesce, when set, serves plugin.quiesce. Without it the host is told
	// the method is unsupported, which it accepts.
	Quiesce func()
	// Shutdown releases the plugin's state when the host unloads it.
	Shutdown func()
	// ErrorCode is the envelope code of errors that are not an *Error, for
	// example "jetbrains_error".
	ErrorCode string
	// InternalErrorsRetryable marks errors without an HTTP status (reported
	// as 500) as retryable.
	InternalErrorsRetryable bool

	handlers map[string]Handler
}

// Capabilities are the host integrations a provider plugin declares. Their
// JSON is the host's registration schema (pluginhost rpcCapabilities).
type Capabilities struct {
	ManagementAPI         bool                         `json:"management_api"`
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	QuotaProvider         bool                         `json:"quota_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
}

// Registration is the result of plugin.register and plugin.reconfigure.
type Registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  Capabilities       `json:"capabilities"`
}

// Registration describes the plugin to the host.
func (p *Plugin) Registration() Registration {
	return Registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: p.Metadata, Capabilities: p.Capabilities}
}

// Handle serves method with h. A later registration replaces an earlier one.
func (p *Plugin) Handle(method string, h Handler) {
	if p.handlers == nil {
		p.handlers = map[string]Handler{}
	}
	p.handlers[method] = h
}

// Decode adapts a function taking a typed request into a Handler. A request
// that is not valid JSON for T fails with the decoder's error.
func Decode[T any](fn func(T) (any, error)) Handler {
	return func(request []byte) (any, error) {
		var req T
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		return fn(req)
	}
}

// Dispatch runs method and returns its result. Registered handlers take
// precedence over the lifecycle and identifier methods the framework serves
// itself.
func (p *Plugin) Dispatch(method string, request []byte) (any, error) {
	if h, ok := p.handlers[method]; ok {
		return h(request)
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lifecycle struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(request) > 0 {
			if err := json.Unmarshal(request, &lifecycle); err != nil {
				return nil, fmt.Errorf("invalid %s request: %w", method, err)
			}
		}
		if p.Configure != nil {
			if err := p.Configure(lifecycle.ConfigYAML); err != nil {
				return nil, err
			}
		}
		return p.Registration(), nil
	case pluginabi.MethodPluginQuiesce:
		if p.Quiesce != nil {
			p.Quiesce()
			return struct{}{}, nil
		}
	case pluginabi.MethodPluginShutdown:
		if p.Shutdown != nil {
			p.Shutdown()
		}
		return struct{}{}, nil
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		if p.Identifier != "" {
			return Identifier{Identifier: p.Identifier}, nil
		}
	}
	return nil, &Error{Code: "unknown_method", Message: "unknown plugin method: " + method, HTTPStatus: http.StatusNotImplemented}
}

// Identifier is the result of the *.identifier methods.
type Identifier struct {
	Identifier string `json:"identifier"`
}

// Call runs method and encodes its envelope. failed reports an error
// envelope, which the C ABI returns as a non-zero status.
func (p *Plugin) Call(method string, request []byte) (envelope []byte, failed bool) {
	result, err := p.Dispatch(method, request)
	if err != nil {
		return ErrorEnvelope(p.envelopeError(err)), true
	}
	envelope, err = OKEnvelope(result)
	if err != nil {
		return ErrorEnvelope(&pluginabi.Error{Code: "encoding_error", Message: "Cannot encode plugin response", HTTPStatus: http.StatusInternalServerError}), true
	}
	return envelope, false
}

func (p *Plugin) envelopeError(err error) *pluginabi.Error {
	var coded *Error
	if errors.As(err, &coded) {
		code := coded.Code
		if code == "" {
			code = p.ErrorCode
		}
		return &pluginabi.Error{Code: code, Message: coded.Message, HTTPStatus: coded.HTTPStatus, Retryable: coded.Retryable}
	}
	var status StatusCoder
	if errors.As(err, &status) && status.StatusCode() > 0 {
		code := status.StatusCode()
		return &pluginabi.Error{Code: p.ErrorCode, Message: err.Error(), HTTPStatus: code, Retryable: RetryableStatus(code)}
	}
	return &pluginabi.Error{Code: p.ErrorCode, Message: err.Error(), HTTPStatus: http.StatusInternalServerError, Retryable: p.InternalErrorsRetryable}
}

// Error is a failure with every envelope field chosen by the plugin. An empty
// Code takes the plugin's ErrorCode.
type Error struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// StatusCoder is an error that carries an HTTP status for the host. Its
// envelope uses the plugin's ErrorCode and the error text, and is retryable
// for 429 and 5xx. A status of 0 counts as no status.
type StatusCoder interface {
	error
	StatusCode() int
}

// RetryableStatus reports whether the host may retry a request that failed
// with status.
func RetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// OKEnvelope encodes a successful result.
func OKEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

// ErrorEnvelope encodes a failure.
func ErrorEnvelope(failure *pluginabi.Error) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: failure})
	return raw
}
