package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func report(cost float64, published string) string {
	data, _ := json.Marshal(map[string]any{"budgetDisplayName": budgetLabel, "costAmount": cost, "budgetAmount": 269.43, "currencyCode": "EUR"})
	return fmt.Sprintf(`{"ackId":%q,"message":{"data":%q,"publishTime":%q}}`, "ack-"+published, base64.StdEncoding.EncodeToString(data), published)
}

func TestCreditBudgetReportsTheNewestSpend(t *testing.T) {
	t.Setenv("CLIPROXY_GEMINI_FREE_OAUTH_CLIENT_SECRET", "oauth-client-secret")
	const project = "trial-project-1"
	sub := pubsubAPI + "projects/" + project + "/subscriptions/" + creditTopic
	topic := "projects/" + project + "/topics/" + creditTopic
	var budgetCreated, setupCalls int
	var acked, kept []string
	pulls := []string{
		`{"receivedMessages":[` + report(12.5, "2026-11-22T18:00:00Z") + `,` + report(20.25, "2026-11-22T20:00:00Z") + `,` + report(5, "2026-11-22T12:00:00Z") + `]}`,
		`{"receivedMessages":[` + report(20.25, "2026-11-22T20:00:00Z") + `]}`,
	}
	p := newProvider(framework.Host{Call: func(name string, input any) (json.RawMessage, error) {
		r := input.(map[string]any)
		endpoint, h, body := r["url"].(string), r["headers"].(http.Header), r["body"].([]byte)
		status, output := 200, "{}"
		if endpoint != tokenEndpoint && h.Get("Authorization") != "Bearer vertex-access" {
			t.Fatalf("%s did not use the user's token", endpoint)
		}
		if (strings.HasPrefix(endpoint, pubsubAPI) || strings.HasPrefix(endpoint, budgetAPI)) && h.Get("X-Goog-User-Project") != project {
			t.Fatalf("%s did not bill its quota to the account's project", endpoint)
		}
		switch endpoint {
		case tokenEndpoint:
			output = `{"access_token":"vertex-access","expires_in":3599}`
		case serviceUsage + "projects/" + project + "/services/pubsub.googleapis.com:enable",
			serviceUsage + "projects/" + project + "/services/billingbudgets.googleapis.com:enable":
			setupCalls++
			output = `{"name":"operations/noop.DONE_OPERATION","done":true}`
		case billingAPI + "projects/" + project + "/billingInfo":
			output = `{"billingAccountName":"billingAccounts/01B363-881557-904916"}`
		case pubsubAPI + topic:
			status = 409
		case sub:
			if !strings.Contains(string(body), `"expirationPolicy":{}`) {
				t.Fatal("credit subscription would expire")
			}
		case budgetAPI + "billingAccounts/01B363-881557-904916/budgets?pageSize=100":
			output = `{"budgets":[{"notificationsRule":{"pubsubTopic":"projects/other/topics/x"}}]}`
		case budgetAPI + "billingAccounts/01B363-881557-904916/budgets":
			budgetCreated++
			if !strings.Contains(string(body), `"nanos":430000000,"units":"269"`) || !strings.Contains(string(body), `"pubsubTopic":"`+topic+`"`) ||
				!strings.Contains(string(body), "EXCLUDE_ALL_CREDITS") {
				t.Fatalf("unexpected budget %s", body)
			}
		case sub + ":pull":
			if !strings.Contains(string(body), `"returnImmediately":true`) {
				t.Fatal("credit pull would wait for a message")
			}
			output, pulls = pulls[0], pulls[1:]
		case sub + ":acknowledge":
			var ids struct{ AckIDs []string }
			_ = json.Unmarshal(body, &ids)
			acked = append(acked, ids.AckIDs...)
		case sub + ":modifyAckDeadline":
			var ids struct{ AckIDs []string }
			_ = json.Unmarshal(body, &ids)
			kept = append(kept, ids.AckIDs...)
		default:
			t.Fatalf("unexpected Google request %s", endpoint)
		}
		return json.Marshal(framework.HTTPResponse{StatusCode: status, Body: []byte(output)})
	}})
	if err := p.configure([]byte("oauth_client_id: " + fakeClient + "\nvertex_models: gemini-4-neon\nvertex_credit: 269.43\n")); err != nil {
		t.Fatal(err)
	}
	c := credential{Type: provider, Project: "cliproxy-gf-0000000000000000", Email: "owner@example.test", Key: fakeKey, Vertex: project, Refresh: "refresh"}
	payload, _ := json.Marshal(map[string]any{"auth_index": "1", "provider": provider, "storage_json": c.auth().StorageJSON})
	for i := range 2 {
		result, err := p.plugin().Dispatch(pluginabi.MethodQuotaFetch, payload)
		if err != nil {
			t.Fatal(err)
		}
		quota := result.(pluginapi.QuotaFetchResponse)
		bucket := quota.Groups[0].Buckets[0]
		if want := 1 - 20.25/269.43; fmt.Sprintf("%.6f", bucket.RemainingFraction) != fmt.Sprintf("%.6f", want) ||
			!strings.Contains(bucket.Description, "€249.18 of €269.43 left") || quota.Summary[0].Value != 269.43-20.25 {
			t.Fatalf("read %d: unexpected credit %+v %+v", i, bucket, quota.Summary)
		}
	}
	if budgetCreated != 1 || setupCalls != 2 {
		t.Fatalf("expected one setup, got %d budgets and %d enables", budgetCreated, setupCalls)
	}
	if strings.Join(acked, ",") != "ack-2026-11-22T18:00:00Z,ack-2026-11-22T12:00:00Z" ||
		strings.Join(kept, ",") != "ack-2026-11-22T20:00:00Z,ack-2026-11-22T20:00:00Z" {
		t.Fatalf("expected the newest report kept and the rest acknowledged, acked %v kept %v", acked, kept)
	}
}

func TestCreditConfiguration(t *testing.T) {
	for _, good := range []string{"", "300", "269.43"} {
		if _, err := parseCredit(good); err != nil {
			t.Fatalf("rejected %q: %v", good, err)
		}
	}
	for _, bad := range []string{"0", "-5", "1.234", "1e3", "€300"} {
		if _, err := parseCredit(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
