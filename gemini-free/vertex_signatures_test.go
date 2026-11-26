package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const invalidVertexSignature = `{"error":{"status":"INVALID_ARGUMENT","message":"Invalid thought signature."}}`

func TestRepairVertexSignaturesPreservesConversation(t *testing.T) {
	native := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	wrapped := base64.StdEncoding.EncodeToString(append([]byte{0x12, 10, 0x0a, 8}, native...))
	body := fmt.Sprintf(`{
		"contents": [
			{"role":"user","parts":[{"text":"Find the status"}]},
			{"role":"model","parts":[
				{"functionCall":{"name":"sample","args":{"query":"test"}},"thoughtSignature":%q},
				{"functionCall":{"name":"sample2","args":{}}}
			]},
			{"role":"user","parts":[{"functionResponse":{"name":"sample","response":{"ok":true}}},
				{"functionResponse":{"name":"sample2","response":{"ok":true}}}]},
			{"role":"model","parts":[{"text":"I found it","thoughtSignature":%q}]},
			{"role":"user","parts":[{"text":"Continue"}]},
			{"role":"model","parts":[{"functionCall":{"name":"sample","args":{}},"thoughtSignature":%q}]},
			{"role":"user","parts":[{"functionResponse":{"name":"sample","response":{"ok":true}}}]}
		], "generationConfig":{"maxOutputTokens":64}
	}`, wrapped, wrapped, base64.StdEncoding.EncodeToString(native))
	want := strings.Replace(body, wrapped, vertexMigratedSignature, 1)
	want = strings.Replace(want, `,"thoughtSignature":"`+wrapped+`"`, "", 1)
	got := repairVertexSignatures([]byte(body), []byte(invalidVertexSignature))
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal([]byte(want), &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("repaired the wrong parts of the conversation: %s", got)
	}
	if repairVertexSignatures(got, []byte(invalidVertexSignature)) != nil {
		t.Fatal("already migrated and native signatures must stay intact")
	}
}

func TestVertexRetriesOnlySignatureRejections(t *testing.T) {
	const original = `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"sample","args":{}},"thoughtSignature":"EgIKAA=="},{"functionCall":{"name":"parallel","args":{}}}]},{"role":"user","parts":[{"text":"Continue"}]}]}`
	native := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	payload := strings.Replace(original, "EgIKAA==", base64.StdEncoding.EncodeToString(append([]byte{0x12, 10, 0x0a, 8}, native...)), 1)
	repaired := strings.Replace(original, "EgIKAA==", vertexMigratedSignature, 1)
	const unrelated = `{"error":{"status":"INVALID_ARGUMENT","message":"Thinking budget is not supported for this model."}}`
	const output = `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]}}]}`
	for _, tc := range []struct {
		name, rejection        string
		vertex, retry, success bool
	}{
		{"migrated history", invalidVertexSignature, true, true, true},
		{"unrelated request error", unrelated, true, false, false},
		{"Developer API", invalidVertexSignature, false, false, false},
		{"at most one retry", invalidVertexSignature, true, true, false},
	} {
		for _, method := range []string{pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				attempts, rejectionChunks := 0, 0
				closed := make(chan string, 2)
				done := make(chan string, 1)
				emitted := make(chan string, 1)
				host := framework.Host{Call: func(name string, input any) (json.RawMessage, error) {
					switch name {
					case pluginabi.MethodHostHTTPDo, pluginabi.MethodHostHTTPDoStream:
						req := input.(map[string]any)
						attempts++
						status, response, want := http.StatusBadRequest, tc.rejection, payload
						if attempts > 1 {
							want = repaired
							if tc.success {
								status, response = http.StatusOK, output
							}
						}
						if string(req["body"].([]byte)) != want {
							t.Fatalf("unexpected generation payload for attempt %d: %s", attempts, req["body"])
						}
						if name == pluginabi.MethodHostHTTPDo {
							return json.Marshal(framework.HTTPResponse{StatusCode: status, Body: []byte(response)})
						}
						return json.Marshal(map[string]any{"status_code": status, "stream_id": fmt.Sprint(attempts)})
					case pluginabi.MethodHostHTTPStreamRead:
						if input.(map[string]string)["stream_id"] == "1" {
							rejectionChunks++
							if rejectionChunks == 1 {
								return json.Marshal(framework.StreamChunk{Payload: []byte(tc.rejection[:10])})
							}
							return json.Marshal(framework.StreamChunk{Payload: []byte(tc.rejection[10:]), Done: true})
						}
						return json.Marshal(framework.StreamChunk{Payload: []byte("data: " + output + "\n\n"), Done: true})
					case pluginabi.MethodHostHTTPStreamClose:
						closed <- input.(map[string]string)["stream_id"]
					case pluginabi.MethodHostStreamEmit:
						emitted <- string(input.(map[string]any)["payload"].([]byte))
					case pluginabi.MethodHostStreamClose:
						done <- input.(map[string]string)["error"]
					default:
						return nil, fmt.Errorf("unexpected callback %s", name)
					}
					return json.RawMessage(`{}`), nil
				}}
				p := newProvider(host)
				c := credential{Type: provider, Project: "cliproxy-gf-0000000000000000", Email: "owner@example.test", Key: fakeKey}
				model := probeModel
				if tc.vertex {
					if err := p.configure([]byte("vertex_models: " + vertexModel)); err != nil {
						t.Fatal(err)
					}
					c.Vertex, c.Refresh = trialProj, "test-refresh"
					id := sha256.Sum256([]byte(c.Refresh))
					p.tokens[hex.EncodeToString(id[:])] = accessToken{value: "vertex-access", expires: time.Now().Add(time.Hour)}
					model = vertexModel
				}
				_, err := p.execute(method, rpc{StreamID: "destination", ExecutorRequest: pluginapi.ExecutorRequest{
					Model: defaultPool + "/" + model, StorageJSON: c.auth().StorageJSON, SourceFormat: "gemini", Payload: []byte(payload),
				}})
				if (err == nil) != tc.success {
					t.Fatalf("generation success = %t, want %t: %v", err == nil, tc.success, err)
				}
				wantAttempts := 1
				if tc.retry {
					wantAttempts++
				}
				if attempts != wantAttempts {
					t.Fatalf("got %d upstream attempts, want %d", attempts, wantAttempts)
				}
				if method == pluginabi.MethodExecutorExecuteStream {
					for i := 1; i <= wantAttempts; i++ {
						select {
						case id := <-closed:
							if id != fmt.Sprint(i) {
								t.Fatalf("closed stream %s out of order", id)
							}
						case <-time.After(5 * time.Second):
							t.Fatalf("upstream stream %d was not closed", i)
						}
					}
					if tc.success && (len(emitted) != 1 || <-emitted != output || len(done) != 1 || <-done != "") {
						t.Fatal("recovered stream did not return the upstream output and close cleanly")
					}
				}
			})
		}
	}
}
