package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func report(label string, cost float64, since, published string) string {
	data, _ := json.Marshal(map[string]any{"budgetDisplayName": label, "costAmount": cost, "budgetAmount": 269.43,
		"costIntervalStart": since, "currencyCode": "EUR"})
	id := fmt.Sprintf("ack-%s-%s", strings.TrimPrefix(label, costLabel), published)
	return fmt.Sprintf(`{"ackId":%q,"message":{"data":%q,"publishTime":%q}}`, id, base64.StdEncoding.EncodeToString(data), published)
}

const fullPeriod = "2017-01-02T08:00:00Z"

// creditFake answers the Google Cloud calls of a quota read for an account
// whose free trial started at trial and whose billing account is open or not.
type creditFake struct {
	trial                     time.Time
	open                      bool
	pulls                     []string
	patched, created, enables int
	acked, kept               []string
}

func (f *creditFake) provider(t *testing.T) *providerPlugin {
	const project = "trial-project-1"
	const account = "billingAccounts/01B363-881557-904916"
	sub := pubsubAPI + "projects/" + project + "/subscriptions/" + creditTopic
	topic := "projects/" + project + "/topics/" + creditTopic
	return newProvider(framework.Host{Call: func(name string, input any) (json.RawMessage, error) {
		r := input.(map[string]any)
		endpoint, h, body := r["url"].(string), r["headers"].(http.Header), r["body"].([]byte)
		status, output := 200, "{}"
		if endpoint != tokenEndpoint && h.Get("Authorization") != "Bearer vertex-access" {
			t.Fatalf("%s did not use the user's token", endpoint)
		}
		if (strings.HasPrefix(endpoint, pubsubAPI) || strings.HasPrefix(endpoint, budgetAPI) || strings.HasPrefix(endpoint, loggingAPI)) &&
			h.Get("X-Goog-User-Project") != project {
			t.Fatalf("%s did not bill its quota to the account's project", endpoint)
		}
		switch endpoint {
		case tokenEndpoint:
			output = `{"access_token":"vertex-access","expires_in":3599}`
		case serviceUsage + "projects/" + project + "/services/pubsub.googleapis.com:enable",
			serviceUsage + "projects/" + project + "/services/billingbudgets.googleapis.com:enable",
			serviceUsage + "projects/" + project + "/services/logging.googleapis.com:enable":
			f.enables++
			output = `{"name":"operations/noop.DONE_OPERATION","done":true}`
		case billingAPI + "projects/" + project + "/billingInfo":
			output = `{"billingAccountName":"` + account + `"}`
		case billingAPI + account:
			output = fmt.Sprintf(`{"name":%q,"open":%t}`, account, f.open)
		case pubsubAPI + topic:
			status = 409
		case sub:
			if !strings.Contains(string(body), `"expirationPolicy":{}`) {
				t.Fatal("credit subscription would expire")
			}
		case budgetAPI + account + "/budgets?pageSize=100":
			// A budget from an earlier version, which counted from the day
			// tracking started, and someone else's budget.
			output = `{"budgets":[{"name":"` + account + `/budgets/old","displayName":"` + costLabel + `",` +
				`"budgetFilter":{"creditTypesTreatment":"EXCLUDE_ALL_CREDITS","customPeriod":{"startDate":{"year":2026,"month":11,"day":22}}},` +
				`"notificationsRule":{"pubsubTopic":"` + topic + `"}},{"displayName":"` + netLabel + `","notificationsRule":{"pubsubTopic":"projects/other/topics/x"}}]}`
		case budgetAPI + account + "/budgets/old?updateMask=" + budgetMask:
			f.patched++
			if r["method"] != http.MethodPatch || string(body) != `{"budgetFilter":{"creditTypesTreatment":"EXCLUDE_ALL_CREDITS","customPeriod":{"startDate":{"day":2,"month":1,"year":2017}}}}` {
				t.Fatalf("unexpected budget update %s %s", r["method"], body)
			}
		case budgetAPI + account + "/budgets":
			f.created++
			if !strings.Contains(string(body), `"nanos":430000000,"units":"269"`) || !strings.Contains(string(body), `"pubsubTopic":"`+topic+`"`) ||
				!strings.Contains(string(body), `"displayName":"`+netLabel+`"`) ||
				!strings.Contains(string(body), `"creditTypes":["PROMOTION"],"creditTypesTreatment":"INCLUDE_SPECIFIED_CREDITS","customPeriod":{"startDate":{"day":2,"month":1,"year":2017}}`) {
				t.Fatalf("unexpected budget %s", body)
			}
		case billingAPI + account + "/projects?pageSize=50":
			output = `{"projectBillingInfo":[{"projectId":"` + project + `"},{"projectId":"ai-studio-1"}]}`
		case resourceManager + "projects/" + project:
			output = `{"createTime":"` + f.trial.Add(-5*time.Minute).Format(time.RFC3339) + `"}`
		case resourceManager + "projects/ai-studio-1":
			output = `{"createTime":"` + f.trial.Add(-9*24*time.Hour).Format(time.RFC3339) + `"}`
		case loggingAPI + "entries:list":
			var q struct {
				Names  []string `json:"resourceNames"`
				Filter string
				Token  string `json:"pageToken"`
			}
			_ = json.Unmarshal(body, &q)
			if strings.Join(q.Names, ",") != "projects/"+project+",projects/ai-studio-1" ||
				!strings.Contains(q.Filter, fmt.Sprintf("timestamp>=%q", f.trial.Add(-9*24*time.Hour-time.Minute).UTC().Format(time.RFC3339))) ||
				!strings.Contains(q.Filter, `protoPayload.authorizationInfo.resource="`+account+`"`) {
				t.Fatalf("unexpected log query %s", body)
			}
			output = `{"nextPageToken":"more"}` // Logging returns empty pages while it scans.
			if q.Token == "more" {
				output = `{"entries":[{"timestamp":"` + f.trial.Format(time.RFC3339Nano) + `"}]}`
			}
		case sub + ":pull":
			if !strings.Contains(string(body), `"returnImmediately":true`) {
				t.Fatal("credit pull would wait for a message")
			}
			output, f.pulls = f.pulls[0], f.pulls[1:]
		case sub + ":acknowledge":
			var ids struct{ AckIDs []string }
			_ = json.Unmarshal(body, &ids)
			f.acked = append(f.acked, ids.AckIDs...)
		case sub + ":modifyAckDeadline":
			var ids struct{ AckIDs []string }
			_ = json.Unmarshal(body, &ids)
			f.kept = append(f.kept, ids.AckIDs...)
		default:
			t.Fatalf("unexpected Google request %s", endpoint)
		}
		return json.Marshal(framework.HTTPResponse{StatusCode: status, Body: []byte(output)})
	}})
}

func readQuota(t *testing.T, p *providerPlugin) pluginapi.QuotaFetchResponse {
	t.Helper()
	c := credential{Type: provider, Project: "cliproxy-gf-0000000000000000", Email: "owner@example.test", Key: fakeKey, Vertex: "trial-project-1", Refresh: "refresh"}
	payload, _ := json.Marshal(map[string]any{"auth_index": "1", "provider": provider, "storage_json": c.auth().StorageJSON})
	result, err := p.plugin().Dispatch(pluginabi.MethodQuotaFetch, payload)
	if err != nil {
		t.Fatal(err)
	}
	return result.(pluginapi.QuotaFetchResponse)
}

func TestCreditBudgetsReportWhatPromotionsPaid(t *testing.T) {
	t.Setenv("CLIPROXY_GEMINI_FREE_OAUTH_CLIENT_SECRET", "oauth-client-secret")
	trial := time.Now().Add(-10 * 24 * time.Hour).UTC().Truncate(time.Second)
	// 31.50 of cost, of which promotions paid 20.25; the rest is paid usage
	// such as the Gemini API, which the free trial does not cover.
	f := &creditFake{trial: trial, open: true, pulls: []string{
		`{"receivedMessages":[` + report(costLabel, 12.5, fullPeriod, "2026-11-22T18:00:00Z") + `,` +
			report(costLabel, 31.5, fullPeriod, "2026-11-22T20:00:00Z") + `,` +
			report(costLabel, 40, "2026-11-22T08:00:00Z", "2026-11-22T21:00:00Z") + `,` +
			report(netLabel, 11.25, fullPeriod, "2026-11-22T19:00:00Z") + `]}`,
		`{"receivedMessages":[` + report(costLabel, 31.5, fullPeriod, "2026-11-22T20:00:00Z") + `,` +
			report(netLabel, 11.25, fullPeriod, "2026-11-22T19:00:00Z") + `]}`,
	}}
	p := f.provider(t)
	if err := p.configure([]byte("oauth_client_id: " + fakeClient + "\nvertex_models: gemini-4-neon\nvertex_credit: 269.43\n")); err != nil {
		t.Fatal(err)
	}
	ends := trial.Add(90 * 24 * time.Hour)
	for i := range 2 {
		quota := readQuota(t, p)
		bucket := quota.Groups[0].Buckets[0]
		if want := 1 - 20.25/269.43; fmt.Sprintf("%.6f", bucket.RemainingFraction) != fmt.Sprintf("%.6f", want) ||
			!strings.Contains(bucket.Description, "€249.18 of €269.43 left, reported 22 Nov 19:00 UTC; ends "+ends.Format("2 Jan 2006")) ||
			bucket.ResetTime != ends.Format(time.RFC3339) || quota.Summary[0].Value != 269.43-20.25 || quota.Summary[1].Value != 20.25 ||
			quota.Summary[3].Key != "days_left" || quota.Summary[3].Value != 80 {
			t.Fatalf("read %d: unexpected credit %+v %+v", i, bucket, quota.Summary)
		}
	}
	if f.patched != 1 || f.created != 1 || f.enables != 3 {
		t.Fatalf("expected one setup, got %d updates, %d new budgets and %d enables", f.patched, f.created, f.enables)
	}
	if strings.Join(f.acked, ",") != "ack--2026-11-22T18:00:00Z,ack--2026-11-22T21:00:00Z" ||
		strings.Join(f.kept, ",") != "ack- after promotions-2026-11-22T19:00:00Z,ack--2026-11-22T20:00:00Z,ack- after promotions-2026-11-22T19:00:00Z,ack--2026-11-22T20:00:00Z" {
		t.Fatalf("expected the newest report of each budget kept and the rest acknowledged, acked %v kept %v", f.acked, f.kept)
	}
}

func TestCreditEndsWithTheTrial(t *testing.T) {
	t.Setenv("CLIPROXY_GEMINI_FREE_OAUTH_CLIENT_SECRET", "oauth-client-secret")
	trial := time.Now().Add(-100 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := &creditFake{trial: trial, open: true, pulls: []string{
		`{"receivedMessages":[` + report(costLabel, 30, fullPeriod, "2026-11-22T20:00:00Z") + `,` + report(netLabel, 0, fullPeriod, "2026-11-22T20:00:00Z") + `]}`,
		`{}`,
	}}
	p := f.provider(t)
	if err := p.configure([]byte("oauth_client_id: " + fakeClient + "\nvertex_models: gemini-4-neon\nvertex_credit: 269.43\n")); err != nil {
		t.Fatal(err)
	}
	bucket := readQuota(t, p).Groups[0].Buckets[0]
	if bucket.RemainingFraction != 0 || !strings.HasSuffix(bucket.Description, "; ended "+trial.Add(90*24*time.Hour).Format("2 Jan 2006")) {
		t.Fatalf("trial past 90 days still has credit: %+v", bucket)
	}
	f.open = false
	f.trial = time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	p.credits = map[string]*creditState{}
	bucket = readQuota(t, p).Groups[0].Buckets[0]
	if bucket.RemainingFraction != 0 || !strings.HasSuffix(bucket.Description, "; the billing account is closed") {
		t.Fatalf("closed billing account still has credit: %+v", bucket)
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
