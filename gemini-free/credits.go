package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pubsubAPI   = "https://pubsub.googleapis.com/v1/"
	budgetAPI   = "https://billingbudgets.googleapis.com/v1/"
	loggingAPI  = "https://logging.googleapis.com/v2/"
	creditTopic = "cliproxy-credit"
	// costLabel names the budget that counts cost before any credit, and
	// netLabel the one that counts cost after promotional credits. Their
	// difference is what promotional credits, such as the free trial, paid.
	costLabel = "CLIProxy credits"
	netLabel  = "CLIProxy credits after promotions"
	// trialDays is how long the Google Cloud free trial credit lasts.
	trialDays = 90
)

var (
	creditPattern  = regexp.MustCompile(`^[0-9]{1,7}(\.[0-9]{1,2})?$`)
	accountPattern = regexp.MustCompile(`^billingAccounts/[A-Za-z0-9-]+$`)
	// budgetStart is the earliest start date Cloud Billing accepts, so the
	// budgets count everything since the billing account opened.
	budgetStart = map[string]int{"year": 2017, "month": 1, "day": 2}
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

// creditReport is a Cloud Billing budget notification: the budget's cost
// since it started and its amount, which equals the account's credit.
type creditReport struct {
	Name      string    `json:"budgetDisplayName"`
	Budget    float64   `json:"budgetAmount"`
	Cost      float64   `json:"costAmount"`
	Since     string    `json:"costIntervalStart"`
	Currency  string    `json:"currencyCode"`
	published time.Time `json:"-"`
}

// creditState tracks one Vertex AI project's credit budgets. Pub/Sub keeps
// the newest notification of each budget unacknowledged, so every gateway
// replica and restart can read it again.
type creditState struct {
	ready   bool
	account string
	// trial is when the billing account was first linked to a project, which
	// starts the free trial; searched records that the audit logs were read.
	trial    time.Time
	searched bool
	reports  map[string]*creditReport
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

// cloud calls a Google Cloud API as the signed-in user. Pub/Sub, Budget and
// Logging calls bill their quota to the account's own project, not the OAuth
// client's.
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
		state = &creditState{reports: make(map[string]*creditReport)}
		p.credits[project] = state
	}
	return state
}

type budgetInfo struct {
	Name   string `json:"name"`
	Label  string `json:"displayName"`
	Filter struct {
		Treatment   string   `json:"creditTypesTreatment"`
		CreditTypes []string `json:"creditTypes"`
		Period      struct {
			Start map[string]int `json:"startDate"`
		} `json:"customPeriod"`
	} `json:"budgetFilter"`
	NotificationsRule struct {
		Topic string `json:"pubsubTopic"`
	} `json:"notificationsRule"`
}

// budgetMask updates a budget's filter. Cloud Billing rejects budgetFilter
// itself as an update mask.
const budgetMask = "budgetFilter.creditTypesTreatment,budgetFilter.creditTypes,budgetFilter.customPeriod"

// budgetFilter is the spend each credit budget counts since budgetStart:
// cost before credits, or cost after promotional credits.
func budgetFilter(label string) map[string]any {
	filter := map[string]any{"creditTypesTreatment": "EXCLUDE_ALL_CREDITS", "customPeriod": map[string]any{"startDate": budgetStart}}
	if label == netLabel {
		filter["creditTypesTreatment"] = "INCLUDE_SPECIFIED_CREDITS"
		filter["creditTypes"] = []string{"PROMOTION"}
	}
	return filter
}

func (b budgetInfo) current() bool {
	start := b.Filter.Period.Start
	if start["year"] != budgetStart["year"] || start["month"] != budgetStart["month"] || start["day"] != budgetStart["day"] {
		return false
	}
	if b.Label == netLabel {
		return b.Filter.Treatment == "INCLUDE_SPECIFIED_CREDITS" && len(b.Filter.CreditTypes) == 1 && b.Filter.CreditTypes[0] == "PROMOTION"
	}
	return b.Filter.Treatment == "EXCLUDE_ALL_CREDITS"
}

// ensureCreditBudget sets up, once per project, two Cloud Billing budgets for
// the account's credit that publish their spend to a Pub/Sub topic in the
// account's Vertex AI project. Every step is idempotent, so an interrupted
// setup continues on the next quota read. It returns the billing account.
func (p *providerPlugin) ensureCreditBudget(cb, token string, c credential, amount float64) (string, error) {
	for _, service := range []string{"pubsub.googleapis.com", "billingbudgets.googleapis.com", "logging.googleapis.com"} {
		var op operation
		if err := p.cloud(cb, token, "", "enable credit tracking", http.MethodPost,
			serviceUsage+"projects/"+c.Vertex+"/services/"+service+":enable", struct{}{}, &op); err != nil {
			return "", err
		}
		if err := operationError("enable credit tracking", op); err != nil {
			return "", err
		}
		if !op.Done {
			return "", failure(http.StatusServiceUnavailable, "Google Cloud is enabling credit tracking for "+c.Email+"; try again in a minute")
		}
	}
	var billing struct {
		Account string `json:"billingAccountName"`
	}
	if err := p.cloud(cb, token, "", "read the billing account", http.MethodGet, billingAPI+"projects/"+c.Vertex+"/billingInfo", nil, &billing); err != nil {
		return "", err
	}
	if !accountPattern.MatchString(billing.Account) {
		return "", failure(http.StatusConflict, "The Vertex AI project of "+c.Email+" has no billing account")
	}
	topic := "projects/" + c.Vertex + "/topics/" + creditTopic
	subscription := "projects/" + c.Vertex + "/subscriptions/" + creditTopic
	if err := p.cloud(cb, token, c.Vertex, "create the credit topic", http.MethodPut, pubsubAPI+topic, struct{}{}, nil); err != nil && !statusIs(err, http.StatusConflict) {
		return "", err
	}
	// Keep notifications for a week and never expire the subscription, so a
	// reading survives quiet periods and gateway restarts.
	if err := p.cloud(cb, token, c.Vertex, "create the credit subscription", http.MethodPut, pubsubAPI+subscription, map[string]any{
		"topic": topic, "ackDeadlineSeconds": 60, "messageRetentionDuration": "604800s", "expirationPolicy": map[string]any{},
	}, nil); err != nil && !statusIs(err, http.StatusConflict) {
		return "", err
	}
	var budgets struct {
		Budgets []budgetInfo `json:"budgets"`
	}
	if err := p.cloud(cb, token, c.Vertex, "list budgets", http.MethodGet, budgetAPI+billing.Account+"/budgets?pageSize=100", nil, &budgets); err != nil {
		return "", err
	}
	for _, label := range []string{costLabel, netLabel} {
		var found *budgetInfo
		for i, budget := range budgets.Budgets {
			if budget.NotificationsRule.Topic == topic && budget.Label == label {
				found = &budgets.Budgets[i]
				break
			}
		}
		switch {
		case found != nil && found.current():
			continue
		case found != nil:
			// Budgets from earlier versions counted cost from the day tracking
			// started; count everything since the account opened instead.
			if !strings.HasPrefix(found.Name, billing.Account+"/budgets/") || strings.ContainsAny(found.Name, "?#%\\") {
				return "", failure(http.StatusBadGateway, "Google Cloud returned an invalid budget")
			}
			if err := p.cloud(cb, token, c.Vertex, "update the credit budget", http.MethodPatch, budgetAPI+found.Name+"?updateMask="+budgetMask,
				map[string]any{"budgetFilter": budgetFilter(label)}, nil); err != nil {
				return "", err
			}
			continue
		}
		if amount == 0 {
			return "", failure(http.StatusServiceUnavailable, "Set vertex_credit in the Gemini Free plugin to track Google Cloud credits")
		}
		units, fraction := math.Modf(amount)
		// The amount stays editable in Google Cloud; reports use the cost
		// budget's amount as the credit.
		if err := p.cloud(cb, token, c.Vertex, "create the credit budget", http.MethodPost, budgetAPI+billing.Account+"/budgets", map[string]any{
			"displayName":       label,
			"budgetFilter":      budgetFilter(label),
			"amount":            map[string]any{"specifiedAmount": map[string]any{"units": strconv.FormatInt(int64(units), 10), "nanos": int64(math.Round(fraction*100)) * 10_000_000}},
			"thresholdRules":    []map[string]float64{{"thresholdPercent": 0.5}, {"thresholdPercent": 0.9}, {"thresholdPercent": 1}},
			"notificationsRule": map[string]string{"pubsubTopic": topic, "schemaVersion": "1.0"},
		}, nil); err != nil {
			return "", err
		}
	}
	return billing.Account, nil
}

// trialStart finds when the billing account was first linked to one of its
// projects, which is when Google Cloud's free trial starts. The audit logs
// of those projects record it for 400 days. No project is linked before it
// exists, so the search starts at the oldest project's creation.
func (p *providerPlugin) trialStart(cb, token, quotaProject, account string) (time.Time, error) {
	var linked struct {
		Projects []struct {
			ProjectID string `json:"projectId"`
		} `json:"projectBillingInfo"`
	}
	if err := p.cloud(cb, token, "", "list the billed projects", http.MethodGet, billingAPI+account+"/projects?pageSize=50", nil, &linked); err != nil {
		return time.Time{}, err
	}
	var names []string
	oldest := time.Now().Add(-400 * 24 * time.Hour)
	earliest := time.Now()
	for _, project := range linked.Projects {
		if !vertexPattern.MatchString(project.ProjectID) {
			continue
		}
		var info struct {
			CreateTime time.Time `json:"createTime"`
		}
		err := p.cloud(cb, token, "", "read a billed project", http.MethodGet, resourceManager+"projects/"+project.ProjectID, nil, &info)
		if statusIs(err, http.StatusForbidden) || statusIs(err, http.StatusNotFound) {
			continue // a project the account cannot see does not hold its logs either
		}
		if err != nil {
			return time.Time{}, err
		}
		names = append(names, "projects/"+project.ProjectID)
		if info.CreateTime.Before(earliest) {
			earliest = info.CreateTime
		}
	}
	if len(names) == 0 {
		return time.Time{}, nil
	}
	if earliest.Before(oldest) {
		earliest = oldest
	}
	request := map[string]any{
		"resourceNames": names, "orderBy": "timestamp asc", "pageSize": 1,
		"filter": fmt.Sprintf(`timestamp>=%q AND logName:"cloudaudit.googleapis.com%%2Factivity" AND `+
			`protoPayload.methodName="AssignResourceToBillingAccount" AND protoPayload.authorizationInfo.resource=%q`,
			earliest.Add(-time.Minute).UTC().Format(time.RFC3339), account),
	}
	// Logging may return empty pages while it scans.
	for range 20 {
		var page struct {
			Entries []struct {
				Timestamp time.Time `json:"timestamp"`
			} `json:"entries"`
			Next string `json:"nextPageToken"`
		}
		if err := p.cloud(cb, token, quotaProject, "read the billing history", http.MethodPost, loggingAPI+"entries:list", request, &page); err != nil {
			return time.Time{}, err
		}
		if len(page.Entries) > 0 {
			return page.Entries[0].Timestamp, nil
		}
		if page.Next == "" {
			return time.Time{}, nil
		}
		request["pageToken"] = page.Next
	}
	return time.Time{}, failure(http.StatusServiceUnavailable, "Google Cloud is still searching the billing history")
}

// readCredit pulls new budget notifications and keeps the newest one of each
// credit budget. It acknowledges the others and returns the newest ones to
// the subscription. Reports from before a budget counted from budgetStart
// are dropped.
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
	latest := maps.Clone(state.reports)
	p.mu.Unlock()
	if latest == nil {
		latest = map[string]*creditReport{}
	}
	newest := map[string]string{}
	var stale []string
	since := fmt.Sprintf("%04d-%02d-%02d", budgetStart["year"], budgetStart["month"], budgetStart["day"])
	for _, received := range pulled.ReceivedMessages {
		var report creditReport
		data, err := base64.StdEncoding.DecodeString(received.Message.Data)
		published, timeErr := time.Parse(time.RFC3339Nano, received.Message.PublishTime)
		if err != nil || timeErr != nil || json.Unmarshal(data, &report) != nil || report.Budget <= 0 || report.Currency == "" ||
			(report.Name != costLabel && report.Name != netLabel) || !strings.HasPrefix(report.Since, since) {
			stale = append(stale, received.AckID)
			continue
		}
		report.published = published
		if old := latest[report.Name]; old == nil || !published.Before(old.published) {
			if id := newest[report.Name]; id != "" {
				stale = append(stale, id)
			}
			latest[report.Name], newest[report.Name] = &report, received.AckID
		} else {
			stale = append(stale, received.AckID)
		}
	}
	if len(stale) > 0 {
		_ = p.cloud(cb, token, c.Vertex, "acknowledge credit reports", http.MethodPost, subscription+":acknowledge", map[string][]string{"ackIds": stale}, nil)
	}
	if len(newest) > 0 {
		_ = p.cloud(cb, token, c.Vertex, "keep the latest credit reports", http.MethodPost, subscription+":modifyAckDeadline",
			map[string]any{"ackIds": slices.Sorted(maps.Values(newest)), "ackDeadlineSeconds": 0}, nil)
	}
	p.mu.Lock()
	state.reports = latest
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
	ready, account, searched := state.ready, state.account, state.searched
	p.mu.Unlock()
	if !ready {
		if account, err = p.ensureCreditBudget(r.HostCallbackID, token, c, amount); err != nil {
			return nil, err
		}
		p.mu.Lock()
		state.ready, state.account = true, account
		p.mu.Unlock()
	}
	if !searched {
		// Without the trial's start the quota still shows the credit, just
		// without an end date; the next read looks again.
		if start, err := p.trialStart(r.HostCallbackID, token, c.Vertex, account); err == nil {
			p.mu.Lock()
			state.trial, state.searched = start, true
			p.mu.Unlock()
		}
	}
	var billing struct {
		Open bool `json:"open"`
	}
	// Google closes a free trial billing account when its credit ends.
	if err := p.cloud(r.HostCallbackID, token, "", "read the billing account", http.MethodGet, billingAPI+account, nil, &billing); err != nil {
		return nil, err
	}
	if err := p.readCredit(r.HostCallbackID, token, c, state); err != nil {
		return nil, err
	}
	p.mu.Lock()
	cost, net, trial := state.reports[costLabel], state.reports[netLabel], state.trial
	p.mu.Unlock()
	response := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "Google Cloud credits", TierName: c.Vertex}}
	bucket := pluginapi.QuotaBucket{Window: "credit"}
	var ends time.Time
	if !trial.IsZero() {
		ends = trial.Add(trialDays * 24 * time.Hour)
		bucket.ResetTime = ends.UTC().Format(time.RFC3339)
	}
	ended := !billing.Open || (!ends.IsZero() && time.Now().After(ends))
	switch {
	case cost == nil || net == nil:
		// Cloud Billing reports a new budget a few times a day.
		bucket.RemainingFraction, bucket.Description = 1, "Waiting for Google Cloud's first spend report"
		if ended {
			bucket.RemainingFraction, bucket.Description = 0, "The Google Cloud credit has ended"
		}
	default:
		spent := math.Max(0, cost.Cost-net.Cost)
		left := math.Max(0, cost.Budget-spent)
		if ended {
			left = 0
		}
		response.Summary = []pluginapi.QuotaMetric{
			{Key: "remaining", Label: "Credit left", Value: left, Format: "currency", Currency: cost.Currency},
			{Key: "spent", Label: "Spent", Value: spent, Format: "currency", Currency: cost.Currency},
			{Key: "credit", Label: "Credit", Value: cost.Budget, Format: "currency", Currency: cost.Currency},
		}
		reported := cost.published
		if net.published.Before(reported) {
			reported = net.published
		}
		bucket.RemainingFraction = left / cost.Budget
		bucket.Description = fmt.Sprintf("%s of %s left, reported %s", money(left, cost.Currency), money(cost.Budget, cost.Currency),
			reported.UTC().Format("2 Jan 15:04 UTC"))
	}
	switch {
	case ended && !ends.IsZero() && time.Now().After(ends):
		bucket.Description += fmt.Sprintf("; ended %s", ends.UTC().Format("2 Jan 2006"))
	case ended:
		bucket.Description += "; the billing account is closed"
	case !ends.IsZero():
		bucket.Description += fmt.Sprintf("; ends %s", ends.UTC().Format("2 Jan 2006"))
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key: "days_left", Label: "Days left", Value: math.Ceil(time.Until(ends).Hours() / 24), Unit: "days", Format: "number"})
	}
	response.Groups = []pluginapi.QuotaGroup{{DisplayName: "Google Cloud credits", Buckets: []pluginapi.QuotaBucket{bucket}}}
	return response, nil
}
