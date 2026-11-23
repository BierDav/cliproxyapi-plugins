package framework

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type statusFailure struct{ status int }

func (e statusFailure) Error() string   { return fmt.Sprintf("upstream said %d", e.status) }
func (e statusFailure) StatusCode() int { return e.status }

func decodeEnvelope(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("envelope %s: %v", raw, err)
	}
	return envelope
}

func TestRegisterConfiguresAndDescribesThePlugin(t *testing.T) {
	var configured string
	p := &Plugin{
		Metadata:     pluginapi.Metadata{Name: "Test", Version: "1.0.0"},
		Capabilities: Capabilities{Executor: true, ExecutorModelScope: pluginapi.ExecutorModelScopeOAuth, ExecutorInputFormats: []string{"openai"}},
		Configure: func(configYAML []byte) error {
			configured = string(configYAML)
			return nil
		},
	}
	raw, failed := p.Call(pluginabi.MethodPluginRegister, []byte(`{"config_yaml":"a2V5OiB2YWx1ZQ=="}`))
	if failed || configured != "key: value" {
		t.Fatalf("register failed=%v configured=%q: %s", failed, configured, raw)
	}
	var registration struct {
		SchemaVersion uint32                     `json:"schema_version"`
		Metadata      pluginapi.Metadata         `json:"metadata"`
		Capabilities  map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(decodeEnvelope(t, raw).Result, &registration); err != nil {
		t.Fatal(err)
	}
	if registration.SchemaVersion != pluginabi.SchemaVersion || registration.Metadata.Name != "Test" ||
		string(registration.Capabilities["executor"]) != "true" || string(registration.Capabilities["executor_model_scope"]) != `"oauth"` {
		t.Fatalf("unexpected registration %+v", registration)
	}

	// Reconfigure without a request body clears the configuration.
	if _, err := p.Dispatch(pluginabi.MethodPluginReconfigure, nil); err != nil || configured != "" {
		t.Fatalf("reconfigure: %v, %q", err, configured)
	}
	p.Configure = func([]byte) error { return errors.New("bad config") }
	if _, err := p.Dispatch(pluginabi.MethodPluginReconfigure, []byte(`{}`)); err == nil || err.Error() != "bad config" {
		t.Fatalf("configuration error must fail the call, got %v", err)
	}
}

func TestHandlersAndBuiltInMethods(t *testing.T) {
	p := &Plugin{Identifier: "test"}
	p.Handle("custom.method", Decode(func(req struct{ Name string }) (any, error) { return "hello " + req.Name, nil }))
	if got, err := p.Dispatch("custom.method", []byte(`{"Name":"host"}`)); err != nil || got != "hello host" {
		t.Fatalf("custom handler: %v, %v", got, err)
	}
	if _, err := p.Dispatch("custom.method", []byte(`not json`)); err == nil {
		t.Fatal("Decode must reject a malformed request")
	}
	for _, method := range []string{pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier} {
		if got, err := p.Dispatch(method, nil); err != nil || got != (Identifier{Identifier: "test"}) {
			t.Fatalf("%s: %v, %v", method, got, err)
		}
	}
	// A handler overrides the built-in method of the same name.
	p.Handle(pluginabi.MethodAuthIdentifier, func([]byte) (any, error) { return "override", nil })
	if got, _ := p.Dispatch(pluginabi.MethodAuthIdentifier, nil); got != "override" {
		t.Fatalf("handler did not override the identifier: %v", got)
	}
}

func TestUnknownMethodsAndQuiesce(t *testing.T) {
	p := &Plugin{ErrorCode: "test_error", InternalErrorsRetryable: true}
	for _, method := range []string{"no.such.method", pluginabi.MethodPluginQuiesce, pluginabi.MethodAuthIdentifier} {
		raw, failed := p.Call(method, nil)
		envelope := decodeEnvelope(t, raw)
		// The host treats unknown_method from plugin.quiesce as "unsupported".
		if !failed || envelope.Error == nil || envelope.Error.Code != "unknown_method" || envelope.Error.HTTPStatus != http.StatusNotImplemented || envelope.Error.Retryable {
			t.Fatalf("%s: unexpected %s", method, raw)
		}
	}
	quiesced, shut := 0, 0
	p.Quiesce = func() { quiesced++ }
	p.Shutdown = func() { shut++ }
	if _, failed := p.Call(pluginabi.MethodPluginQuiesce, nil); failed || quiesced != 1 {
		t.Fatalf("quiesce failed=%v calls=%d", failed, quiesced)
	}
	if raw, failed := p.Call(pluginabi.MethodPluginShutdown, nil); failed || shut != 1 || string(decodeEnvelope(t, raw).Result) != "{}" {
		t.Fatalf("shutdown failed=%v calls=%d: %s", failed, shut, raw)
	}
}

func TestErrorEnvelopes(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		retryUnset bool
		want       pluginabi.Error
	}{
		{"coded error keeps its fields", fmt.Errorf("wrapped: %w", &Error{Code: "upstream_error", Message: "nope", HTTPStatus: 418, Retryable: true}), false,
			pluginabi.Error{Code: "upstream_error", Message: "nope", HTTPStatus: 418, Retryable: true}},
		{"coded error without code takes the plugin's", &Error{Message: "nope", HTTPStatus: 400}, false,
			pluginabi.Error{Code: "test_error", Message: "nope", HTTPStatus: 400}},
		{"status 429 is retryable", statusFailure{429}, false, pluginabi.Error{Code: "test_error", Message: "upstream said 429", HTTPStatus: 429, Retryable: true}},
		{"status 404 is final", fmt.Errorf("lookup: %w", statusFailure{404}), false, pluginabi.Error{Code: "test_error", Message: "lookup: upstream said 404", HTTPStatus: 404}},
		{"status 0 counts as none", statusFailure{0}, false, pluginabi.Error{Code: "test_error", Message: "upstream said 0", HTTPStatus: 500, Retryable: true}},
		{"plain error is retryable when configured", errors.New("broken"), false, pluginabi.Error{Code: "test_error", Message: "broken", HTTPStatus: 500, Retryable: true}},
		{"plain error is final otherwise", errors.New("broken"), true, pluginabi.Error{Code: "test_error", Message: "broken", HTTPStatus: 500}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{ErrorCode: "test_error", InternalErrorsRetryable: !tc.retryUnset}
			p.Handle("fail", func([]byte) (any, error) { return nil, tc.err })
			raw, failed := p.Call("fail", nil)
			envelope := decodeEnvelope(t, raw)
			if !failed || envelope.OK || envelope.Error == nil || *envelope.Error != tc.want {
				t.Fatalf("got %s, want %+v", raw, tc.want)
			}
		})
	}
}

func TestUnencodableResultFails(t *testing.T) {
	p := &Plugin{}
	p.Handle("bad", func([]byte) (any, error) { return func() {}, nil })
	raw, failed := p.Call("bad", nil)
	if envelope := decodeEnvelope(t, raw); !failed || envelope.Error == nil || envelope.Error.Code != "encoding_error" {
		t.Fatalf("unexpected %s", raw)
	}
}
