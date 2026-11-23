package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quota reports the organization's monthly Make credit allowance. Make bills
// AI Provider tokens and scenario operations from the same credit pool.
func quota(raw []byte) (any, error) {
	var r struct {
		pluginapi.QuotaFetchRequest
		Callback string `json:"host_callback_id"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fmt.Errorf("Invalid quota request")
	}
	storageJSON := r.StorageJSON
	if len(storageJSON) == 0 {
		if !strings.EqualFold(strings.TrimSpace(r.Provider), providerKey) {
			return nil, newStatusError(401, "Invalid Make credential")
		}
		storageJSON = encode(r.Metadata)
	}
	s, _, err := liveStorage(r.Callback, r.AuthID, storageJSON, false)
	if err != nil {
		return nil, err
	}
	result, err := callTool(r.Callback, s.AccessToken, "organization_get", map[string]any{"organizationId": s.OrganizationID})
	if err != nil {
		return nil, err
	}
	usage, err := parseOrganizationUsage(result)
	if err != nil {
		return nil, err
	}
	remaining := 0.0
	if usage.Limit > 0 {
		remaining = (usage.Limit - usage.Used) / usage.Limit
		remaining = max(0, min(1, remaining))
	}
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "Make " + usage.Plan},
		Summary: []pluginapi.QuotaMetric{
			{Key: "usage", Label: "Credits used", Value: usage.Used},
			{Key: "limit", Label: "Credits", Value: usage.Limit},
		},
		Groups: []pluginapi.QuotaGroup{{DisplayName: "Monthly credits", Buckets: []pluginapi.QuotaBucket{{Window: "monthly",
			RemainingFraction: remaining, ResetTime: usage.Reset, Description: "Make credits (AI Provider tokens and operations)"}}}},
	}, nil
}

type organizationUsage struct {
	Plan        string
	Used, Limit float64
	Reset       string
}

func parseOrganizationUsage(raw []byte) (organizationUsage, error) {
	var doc struct {
		Organization *json.RawMessage `json:"organization"`
	}
	body := raw
	if json.Unmarshal(raw, &doc) == nil && doc.Organization != nil {
		body = *doc.Organization
	}
	var org struct {
		ProductName          string          `json:"productName"`
		CenticreditsConsumed json.RawMessage `json:"centicreditsConsumed"`
		CenticreditsExtra    json.RawMessage `json:"centicreditsExtra"`
		NextReset            string          `json:"nextReset"`
		License              struct {
			Operations json.RawMessage `json:"operations"`
		} `json:"license"`
	}
	if json.Unmarshal(body, &org) != nil {
		return organizationUsage{}, newStatusError(502, "Invalid Make organization")
	}
	consumed, err1 := number(org.CenticreditsConsumed)
	allowance, err2 := number(org.License.Operations)
	if err1 != nil || err2 != nil || allowance <= 0 {
		return organizationUsage{}, newStatusError(502, "Make did not return a measurable credit allowance")
	}
	extra, _ := number(org.CenticreditsExtra)
	return organizationUsage{Plan: org.ProductName, Used: consumed / 100, Limit: allowance + extra/100, Reset: org.NextReset}, nil
}

// number accepts JSON numbers and Make's numeric strings.
func number(raw json.RawMessage) (float64, error) {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" || value == "null" {
		return 0, fmt.Errorf("missing number")
	}
	return strconv.ParseFloat(value, 64)
}
