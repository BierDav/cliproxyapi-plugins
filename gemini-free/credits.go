package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pubsubAPI   = "https://pubsub.googleapis.com/v1/"
	budgetAPI   = "https://billingbudgets.googleapis.com/v1/"
	creditTopic = "cliproxy-credit"
	budgetLabel = "CLIProxy credits"
)

var (
	creditPattern  = regexp.MustCompile(`^[0-9]{1,7}(\.[0-9]{1,2})?$`)
	accountPattern = regexp.MustCompile(`^billingAccounts/[A-Za-z0-9-]+$`)
)

// parseCredit reads vertex_credit, the Google Cloud credit of each account
// in its billing currency, for example 269.43.
func parseCredit(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if !creditPattern.MatchString(raw) || err != nil || value <= 0 {
		return 0, errors.New("Gemini Free vertex_credit must be a positive amount such as 269.43")
	}
	return value, nil
}

// creditReport is the latest Cloud Billing budget notification: the cost
// since the budget's start before credits, and the budget, which equals the
// account's credit.
type creditReport struct {
	Budget    float64   `json:"budgetAmount"`
	Cost      float64   `json:"costAmount"`
	Currency  string    `json:"currencyCode"`
	published time.Time `json:"-"`
}

// creditState tracks one Vertex AI project's credit budget. Pub/Sub keeps the
// newest notification unacknowledged, so every gateway replica and restart
// can read it again.
type creditState struct {
	ready  bool
	latest *creditReport
}

type pubsubPull struct {
	ReceivedMessages []struct {
		AckID   string `json:"ackId"`
		Message struct {
			Data        string `json:"data"`
			PublishTime string `json:"publishTime"`
		} `json:"message"`
	} `json:"receivedMessages"`
}

// cloud calls a Google Cloud API as the signed-in user. Pub/Sub and Budget
// calls bill their quota to the account's own project, not the OAuth client's.
func (p *providerPlugin) cloud(cb, token, quotaProject, step, method, endpoint string, input, result any) error {
	h := http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/json"}}
	if quotaProject != "" {
		h.Set("X-Goog-User-Project", quotaProject)
	}
	var body []byte
	if input != nil {
		h.Set("Content-Type", "application/json")
		body, _ = json.Marshal(input)
	}
	return p.call(cb, step, framework.HTTPRequest{Method: method, URL: endpoint, Headers: h, Body: body}, result)
}

func (p *providerPlugin) credit(project string) *creditState {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.credits[project]
	if state == nil {
		state = &creditState{}
		p.credits[project] = state
	}
	return state
}

// ensureCreditBudget sets up, once per project, a Cloud Billing budget for
// the account's credit that publishes its spend to a Pub/Sub topic in the
// account's Vertex AI project. Every step is idempotent, so an interrupted
// setup continues on the next quota read.
func (p *providerPlugin) ensureCreditBudget(cb, token string, c credential, amount float64) error {
	for _, service := range []string{"pubsub.googleapis.com", "billingbudgets.googleapis.com"} {
		var op operation
		if err := p.cloud(cb, token, "", "enable credit tracking", http.MethodPost,
			serviceUsage+"projects/"+c.Vertex+"/services/"+service+":enable", struct{}{}, &op); err != nil {
			return err
		}
		if err := operationError("enable credit tracking", op); err != nil {
			return err
		}
		if !op.Done {
			return failure(http.StatusServiceUnavailable, "Google Cloud is enabling credit tracking for "+c.Email+"; try again in a minute")
		}
	}
	var billing struct {
		Account string `json:"billingAccountName"`
	}
	if err := p.cloud(cb, token, "", "read the billing account", http.MethodGet, billingAPI+"projects/"+c.Vertex+"/billingInfo", nil, &billing); err != nil {
		return err
	}
	if !accountPattern.MatchString(billing.Account) {
		return failure(http.StatusConflict, "The Vertex AI project of "+c.Email+" has no billing account")
	}
	topic := "projects/" + c.Vertex + "/topics/" + creditTopic
	subscription := "projects/" + c.Vertex + "/subscriptions/" + creditTopic
	if err := p.cloud(cb, token, c.Vertex, "create the credit topic", http.MethodPut, pubsubAPI+topic, struct{}{}, nil); err != nil && !statusIs(err, http.StatusConflict) {
		return err
	}
	// Keep notifications for a week and never expire the subscription, so a
	// reading survives quiet periods and gateway restarts.
	if err := p.cloud(cb, token, c.Vertex, "create the credit subscription", http.MethodPut, pubsubAPI+subscription, map[string]any{
		"topic": topic, "ackDeadlineSeconds": 60, "messageRetentionDuration": "604800s", "expirationPolicy": map[string]any{},
	}, nil); err != nil && !statusIs(err, http.StatusConflict) {
		return err
	}
	var budgets struct {
		Budgets []struct {
			NotificationsRule struct {
				Topic string `json:"pubsubTopic"`
			} `json:"notificationsRule"`
		} `json:"budgets"`
	}
	if err := p.cloud(cb, token, c.Vertex, "list budgets", http.MethodGet, budgetAPI+billing.Account+"/budgets?pageSize=100", nil, &budgets); err != nil {
		return err
	}
	for _, budget := range budgets.Budgets {
		if budget.NotificationsRule.Topic == topic {
			return nil
		}
	}
	if amount == 0 {
		return failure(http.StatusServiceUnavailable, "Set vertex_credit in the Gemini Free plugin to track Google Cloud credits")
	}
	units, fraction := math.Modf(amount)
	today := time.Now().UTC()
	// Credits pay the cost before credits, so the budget counts that cost from
	// the day tracking starts. The amount stays editable in Google Cloud.
	return p.cloud(cb, token, c.Vertex, "create the credit budget", http.MethodPost, budgetAPI+billing.Account+"/budgets", map[string]any{
		"displayName": budgetLabel,
		"budgetFilter": map[string]any{"creditTypesTreatment": "EXCLUDE_ALL_CREDITS",
			"customPeriod": map[string]any{"startDate": map[string]int{"year": today.Year(), "month": int(today.Month()), "day": today.Day()}}},
		"amount":            map[string]any{"specifiedAmount": map[string]any{"units": strconv.FormatInt(int64(units), 10), "nanos": int64(math.Round(fraction*100)) * 10_000_000}},
		"thresholdRules":    []map[string]float64{{"thresholdPercent": 0.5}, {"thresholdPercent": 0.9}, {"thresholdPercent": 1}},
		"notificationsRule": map[string]string{"pubsubTopic": topic, "schemaVersion": "1.0"},
	}, nil)
}

// readCredit pulls new budget notifications and keeps the newest one. It
// acknowledges the older ones and returns the newest to the subscription.
func (p *providerPlugin) readCredit(cb, token string, c credential, state *creditState) error {
	subscription := pubsubAPI + "projects/" + c.Vertex + "/subscriptions/" + creditTopic
	var pulled pubsubPull
	// Without returnImmediately, a pull on an empty subscription waits for a
	// message, which outlasts the usage reader's timeout.
	if err := p.cloud(cb, token, c.Vertex, "read the credit budget", http.MethodPost, subscription+":pull",
		map[string]any{"maxMessages": 100, "returnImmediately": true}, &pulled); err != nil {
		return err
	}
	p.mu.Lock()
	latest := state.latest
	p.mu.Unlock()
	newest := ""
	var stale []string
	for _, received := range pulled.ReceivedMessages {
		var report creditReport
		data, err := base64.StdEncoding.DecodeString(received.Message.Data)
		published, timeErr := time.Parse(time.RFC3339Nano, received.Message.PublishTime)
		if err != nil || timeErr != nil || json.Unmarshal(data, &report) != nil || report.Budget <= 0 || report.Currency == "" {
			stale = append(stale, received.AckID)
			continue
		}
		report.published = published
		if latest == nil || !published.Before(latest.published) {
			if newest != "" {
				stale = append(stale, newest)
			}
			latest, newest = &report, received.AckID
		} else {
			stale = append(stale, received.AckID)
		}
	}
	if len(stale) > 0 {
		_ = p.cloud(cb, token, c.Vertex, "acknowledge credit reports", http.MethodPost, subscription+":acknowledge", map[string][]string{"ackIds": stale}, nil)
	}
	if newest != "" {
		_ = p.cloud(cb, token, c.Vertex, "keep the latest credit report", http.MethodPost, subscription+":modifyAckDeadline",
			map[string]any{"ackIds": []string{newest}, "ackDeadlineSeconds": 0}, nil)
	}
	p.mu.Lock()
	state.latest = latest
	p.mu.Unlock()
	return nil
}

func money(value float64, currency string) string {
	symbol := map[string]string{"EUR": "€", "USD": "$", "GBP": "£", "CHF": "CHF "}[currency]
	if symbol == "" {
		symbol = currency + " "
	}
	return symbol + strconv.FormatFloat(value, 'f', 2, 64)
}

// quotaRequest is the host's quota.fetch payload, whose storage field is
// snake_case unlike the executor's.
type quotaRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id"`
}

func (p *providerPlugin) quota(r quotaRequest) (any, error) {
	var c credential
	if json.Unmarshal(r.StorageJSON, &c) != nil || !c.valid() {
		return nil, failure(http.StatusUnauthorized, "Invalid Gemini Free credential; enroll again")
	}
	if c.Vertex == "" {
		return nil, failure(http.StatusNotFound, "This Gemini Free account has no Google Cloud credit")
	}
	p.mu.Lock()
	amount := p.creditAmount
	p.mu.Unlock()
	token, err := p.vertexAccessToken(r.HostCallbackID, c)
	if err != nil {
		return nil, err
	}
	state := p.credit(c.Vertex)
	p.mu.Lock()
	ready := state.ready
	p.mu.Unlock()
	if !ready {
		if err := p.ensureCreditBudget(r.HostCallbackID, token, c, amount); err != nil {
			return nil, err
		}
		p.mu.Lock()
		state.ready = true
		p.mu.Unlock()
	}
	if err := p.readCredit(r.HostCallbackID, token, c, state); err != nil {
		return nil, err
	}
	p.mu.Lock()
	latest := state.latest
	p.mu.Unlock()
	response := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "Google Cloud credits", TierName: c.Vertex}}
	if latest == nil {
		// Cloud Billing reports a new budget a few times a day.
		response.Groups = []pluginapi.QuotaGroup{{DisplayName: "Google Cloud credits", Buckets: []pluginapi.QuotaBucket{{
			Window: "credit", RemainingFraction: 1, Description: "Waiting for Google Cloud's first spend report"}}}}
		return response, nil
	}
	left := math.Max(0, latest.Budget-latest.Cost)
	response.Summary = []pluginapi.QuotaMetric{
		{Key: "remaining", Label: "Credit left", Value: left, Format: "currency", Currency: latest.Currency},
		{Key: "spent", Label: "Spent", Value: latest.Cost, Format: "currency", Currency: latest.Currency},
		{Key: "credit", Label: "Credit", Value: latest.Budget, Format: "currency", Currency: latest.Currency},
	}
	response.Groups = []pluginapi.QuotaGroup{{DisplayName: "Google Cloud credits", Buckets: []pluginapi.QuotaBucket{{
		Window: "credit", RemainingFraction: left / latest.Budget,
		Description: fmt.Sprintf("%s of %s left, reported %s", money(left, latest.Currency), money(latest.Budget, latest.Currency),
			latest.published.UTC().Format("2 Jan 15:04 UTC")),
	}}}}
	return response, nil
}
