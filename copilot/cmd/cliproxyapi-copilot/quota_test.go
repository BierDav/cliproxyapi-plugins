package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaRequiresExactAccountAndNeverReturnsToken(t *testing.T) {
	calls := 0
	call := func(method string, payload any) (json.RawMessage, error) {
		calls++
		raw, _ := json.Marshal(payload)
		switch method {
		case pluginabi.MethodHostAuthGet:
			if !strings.Contains(string(raw), `"auth_index":"chosen"`) {
				t.Fatal(string(raw))
			}
			return json.RawMessage(`{"json":{"type":"copilot","github_access_token":"fixture-secret"}}`), nil
		case pluginabi.MethodHostHTTPDo:
			if !strings.Contains(string(raw), "https://api.github.com/copilot_internal/user") {
				t.Fatal(string(raw))
			}
			// The host replies with pluginapi.HTTPResponse, which has no JSON tags.
			body, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"quota_snapshots":{}}`)})
			return body, nil
		default:
			t.Fatalf("unexpected callback %s", method)
			return nil, nil
		}
	}
	bad, err := copilotQuota([]byte(`{"Method":"GET","Path":"/v0/management/copilot-quota","Query":{}}`), framework.Host{Call: call})
	if err != nil || bad.StatusCode != 400 || calls != 0 {
		t.Fatal("missing index must not read any credential")
	}
	good, err := copilotQuota([]byte(`{"Method":"GET","Path":"/v0/management/copilot-quota","Query":{"auth_index":["chosen"]}}`), framework.Host{Call: call})
	if err != nil || good.StatusCode != 200 || calls != 2 || strings.Contains(string(good.Body), "fixture-secret") {
		t.Fatalf("quota failed: %v", err)
	}
}

func TestQuotaUsesSelectedFilelessCredential(t *testing.T) {
	req := pluginapi.QuotaFetchRequest{Provider: "copilot", Metadata: map[string]any{
		"type": "copilot", "github_access_token": "selected-fixture-token",
	}}
	got, err := quotaStorageFromRequest(req)
	if err != nil || got.Token != "selected-fixture-token" {
		t.Fatalf("fileless credential was not resolved: %v", err)
	}
	req.Provider = "other"
	if _, err := quotaStorageFromRequest(req); err == nil {
		t.Fatal("must reject metadata belonging to another provider")
	}
	req.Provider = "copilot"
	req.StorageJSON = []byte(`{"type":"other"}`)
	if _, err := quotaStorageFromRequest(req); err == nil {
		t.Fatal("invalid explicit storage must not fall back to metadata")
	}
}

func TestNormalizeCopilotQuota(t *testing.T) {
	got, err := normalizeCopilotQuota([]byte(`{"quota_reset_date_utc":"2026-12-01T00:00:00Z","quota_snapshots":{"premium_interactions":{"percent_remaining":73}}}`))
	if err != nil || len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 1 {
		t.Fatalf("unexpected normalized quota: %#v, %v", got, err)
	}
	bucket := got.Groups[0].Buckets[0]
	if bucket.RemainingFraction != 0.73 || bucket.ResetTime != "2026-12-01T00:00:00Z" {
		t.Fatalf("unexpected quota bucket: %#v", bucket)
	}
}

func TestNormalizeCopilotQuotaRejectsMissingAllowance(t *testing.T) {
	if _, err := normalizeCopilotQuota([]byte(`{"quota_snapshots":{}}`)); err == nil {
		t.Fatal("missing allowance should fail")
	}
}

func TestQuotaCapabilityIsAdvertised(t *testing.T) {
	raw, err := json.Marshal(pluginRegistration())
	if err != nil || !strings.Contains(string(raw), `"quota_provider":true`) {
		t.Fatalf("quota capability missing from registration: %s, %v", raw, err)
	}
}

func TestNormalizeCopilotQuotaCounts(t *testing.T) {
	got, err := normalizeCopilotQuota([]byte(`{"quota_snapshots":{"premium_interactions":{"entitlement":300,"quota_remaining":225}}}`))
	if err != nil || len(got.Summary) != 2 || got.Summary[0].Value != 300 || got.Summary[1].Value != 225 || got.Groups[0].Buckets[0].RemainingFraction != 0.75 {
		t.Fatalf("unexpected quota counts: %#v, %v", got, err)
	}
	if _, err := normalizeCopilotQuota([]byte(`{"quota_snapshots":{"premium_interactions":{}}}`)); err == nil {
		t.Fatal("unknown allowance must not appear exhausted")
	}
}

func TestNormalizeCopilotQuotaNumericReset(t *testing.T) {
	for _, reset := range []string{"0", "1796083200", "1796083200000", `"2026-12-01T00:00:00Z"`} {
		got, err := normalizeCopilotQuota([]byte(`{"quota_reset_date_utc":"2026-12-01T00:00:00.000Z","quota_snapshots":{"premium_interactions":{"entitlement":200,"remaining":200,"percent_remaining":100,"quota_reset_at":` + reset + `}}}`))
		if err != nil || len(got.Groups) != 1 || got.Groups[0].Buckets[0].ResetTime != "2026-12-01T00:00:00Z" {
			t.Fatalf("reset %s: %#v, %v", reset, got, err)
		}
	}
}
