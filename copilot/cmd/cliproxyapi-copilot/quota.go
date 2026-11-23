package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type rpcQuotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type quotaHTTPClient struct {
	host       framework.Host
	callbackID string
}

func (c quotaHTTPClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	resp, err := c.host.Do(ctx, c.callbackID, framework.HTTPRequest{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body})
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

func (c quotaHTTPClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("streaming is not supported for quota requests")
}

func copilotQuotaDescribe(pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{"copilot"}, DisplayName: "Copilot", SupportsReset: false}, nil
}

func copilotQuotaFetch(ctx context.Context, req rpcQuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	storage, err := quotaStorageFromRequest(req.QuotaFetchRequest)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	if req.HTTPClient == nil {
		req.HTTPClient = quotaHTTPClient{host: host, callbackID: req.HostCallbackID}
	}
	resp, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://api.github.com/copilot_internal/user", Headers: http.Header{
		"Authorization": {"Bearer " + storage.Token}, "Accept": {"application/json"}, "User-Agent": {"GitHubCopilotChat/0.35.0"},
		"Editor-Version": {"vscode/1.107.0"}, "Editor-Plugin-Version": {"copilot-chat/0.35.0"}, "X-GitHub-Api-Version": {"2025-04-01"},
	}})
	if err != nil || resp.StatusCode != http.StatusOK {
		return pluginapi.QuotaFetchResponse{}, errors.New("Copilot quota request failed")
	}
	return normalizeCopilotQuota(resp.Body)
}

func quotaStorageFromRequest(req pluginapi.QuotaFetchRequest) (quotaStorage, error) {
	if len(req.StorageJSON) != 0 {
		return parseQuotaStorage(req.StorageJSON)
	}
	// Fileless workers receive the selected credential's persisted fields in metadata.
	if !strings.EqualFold(strings.TrimSpace(req.Provider), "copilot") {
		return quotaStorage{}, errors.New("Copilot credential unavailable")
	}
	raw, err := json.Marshal(req.Metadata)
	if err != nil {
		return quotaStorage{}, errors.New("Copilot credential unavailable")
	}
	return parseQuotaStorage(raw)
}

type quotaStorage struct {
	Type  string `json:"type"`
	Token string `json:"github_access_token"`
}

func parseQuotaStorage(raw []byte) (quotaStorage, error) {
	var s quotaStorage
	if json.Unmarshal(raw, &s) != nil || !strings.EqualFold(s.Type, "copilot") || strings.TrimSpace(s.Token) == "" {
		return s, errors.New("Copilot credential unavailable")
	}
	s.Token = strings.TrimSpace(s.Token)
	return s, nil
}

func normalizeCopilotQuota(raw []byte) (pluginapi.QuotaFetchResponse, error) {
	var doc struct {
		QuotaSnapshots map[string]struct {
			PercentRemaining *float64        `json:"percent_remaining"`
			Entitlement      *float64        `json:"entitlement"`
			Remaining        *float64        `json:"remaining"`
			QuotaRemaining   *float64        `json:"quota_remaining"`
			Unlimited        bool            `json:"unlimited"`
			Reset            json.RawMessage `json:"quota_reset_at"`
		} `json:"quota_snapshots"`
		Reset     string `json:"quota_reset_date_utc"`
		ResetDate string `json:"quota_reset_date"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return pluginapi.QuotaFetchResponse{}, errors.New("Copilot returned invalid quota data")
	}
	q, ok := doc.QuotaSnapshots["premium_interactions"]
	if !ok {
		q, ok = doc.QuotaSnapshots["chat"]
	}
	if !ok {
		return pluginapi.QuotaFetchResponse{}, errors.New("Copilot did not return an allowance")
	}
	if q.Remaining == nil {
		q.Remaining = q.QuotaRemaining
	}
	remaining := 0.0
	if q.Unlimited {
		remaining = 1
	} else if q.PercentRemaining != nil {
		remaining = *q.PercentRemaining / 100
	} else if q.Entitlement != nil && *q.Entitlement > 0 && q.Remaining != nil {
		remaining = *q.Remaining / *q.Entitlement
	} else {
		return pluginapi.QuotaFetchResponse{}, errors.New("Copilot did not return a measurable allowance")
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	reset := quotaResetTime(q.Reset)
	if reset == "" {
		reset = doc.Reset
	}
	if reset == "" {
		reset = doc.ResetDate
	}
	if reset != "" {
		if t, err := time.Parse(time.RFC3339, reset); err == nil {
			reset = t.UTC().Format(time.RFC3339)
		} else if t, err := time.Parse("2006-01-02", reset); err == nil {
			reset = t.UTC().Format(time.RFC3339)
		}
	}
	var summary []pluginapi.QuotaMetric
	if !q.Unlimited && q.Entitlement != nil && *q.Entitlement >= 0 {
		summary = append(summary, pluginapi.QuotaMetric{Key: "limit", Label: "Monthly limit", Value: *q.Entitlement})
		if q.Remaining != nil && *q.Remaining >= 0 {
			summary = append(summary, pluginapi.QuotaMetric{Key: "remaining", Label: "Remaining", Value: *q.Remaining})
		}
	}
	return pluginapi.QuotaFetchResponse{Summary: summary, Groups: []pluginapi.QuotaGroup{{DisplayName: "Copilot", Buckets: []pluginapi.QuotaBucket{{Window: "monthly", RemainingFraction: remaining, ResetTime: reset, Description: "Premium interactions"}}}}}, nil
}

// GitHub returns either a timestamp string or a Unix value; zero means unset.
func quotaResetTime(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	seconds, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || seconds <= 0 {
		return ""
	}
	if seconds >= 1_000_000_000_000 {
		return time.UnixMilli(seconds).UTC().Format(time.RFC3339)
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}

// Copilot quota is a GitHub read, distinct from the inference token exchange.
// Resolve the exact selected credential; never silently default to another one.
func copilotQuota(raw []byte, host framework.Host) (pluginapi.ManagementResponse, error) {
	var req struct {
		pluginapi.ManagementRequest
		Callback string `json:"host_callback_id"`
	}
	if json.Unmarshal(raw, &req) != nil || req.Method != "GET" || req.Path != "/v0/management/copilot-quota" || req.Query.Get("auth_index") == "" {
		return pluginapi.ManagementResponse{StatusCode: 400, Body: []byte(`{"error":"auth_index is required"}`)}, nil
	}
	auth, err := host.Call(pluginabi.MethodHostAuthGet, map[string]string{"host_callback_id": req.Callback, "auth_index": req.Query.Get("auth_index")})
	if err != nil {
		return pluginapi.ManagementResponse{}, errors.New("Copilot credential unavailable")
	}
	var doc struct {
		JSON struct {
			Type     string `json:"type"`
			Token    string `json:"github_access_token"`
			Disabled bool   `json:"disabled"`
		} `json:"json"`
	}
	if json.Unmarshal(auth, &doc) != nil || doc.JSON.Type != "copilot" || doc.JSON.Token == "" || doc.JSON.Disabled {
		return pluginapi.ManagementResponse{}, errors.New("Copilot credential unavailable")
	}
	upstream, err := host.Do(context.Background(), req.Callback, framework.HTTPRequest{Method: http.MethodGet, URL: "https://api.github.com/copilot_internal/user", Headers: http.Header{
		"Authorization": {"Bearer " + doc.JSON.Token}, "Accept": {"application/json"}, "User-Agent": {"GitHubCopilotChat/0.35.0"}, "Editor-Version": {"vscode/1.107.0"}, "Editor-Plugin-Version": {"copilot-chat/0.35.0"}, "X-GitHub-Api-Version": {"2025-04-01"},
	}})
	if err != nil || upstream.StatusCode != http.StatusOK || !json.Valid(upstream.Body) {
		return pluginapi.ManagementResponse{}, errors.New("Copilot quota request failed")
	}
	return pluginapi.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: upstream.Body}, nil
}
