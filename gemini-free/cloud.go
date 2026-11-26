package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
)

const (
	resourceManager = "https://cloudresourcemanager.googleapis.com/v3/"
	billingAPI      = "https://cloudbilling.googleapis.com/v1/"
	serviceUsage    = "https://serviceusage.googleapis.com/v1/"
	iamAPI          = "https://iam.googleapis.com/v1/"
	keyAPI          = "https://apikeys.googleapis.com/v2/"
	geminiAPI       = "https://generativelanguage.googleapis.com/v1beta/"
	geminiService   = "generativelanguage.googleapis.com"
	vertexService   = "aiplatform.googleapis.com"
	serviceAccount  = "cliproxy-gemini-free"
	keyID           = "cliproxy-gemini-free"
	ownerLabel      = "cliproxy-gemini-free"
)

type stage uint8

const (
	identify stage = iota
	findProject
	createProject
	checkBilling
	enableGemini
	enableIAM
	enableKeys
	getAccount
	createAccount
	getKey
	createKey
	readKey
	probeKey
	findVertex
	enableVertex
	finished
)

type setup struct {
	phase                 stage
	token, clientID       string
	refresh, vertex       string
	project, number, mail string
	key                   string
	operation, opBase     string
	opStep                string
	opStage               stage
	enableRetries         int
	retryUntil            time.Time
}

type projectInfo struct {
	Name, ProjectID, State string
	CreateTime             string
	Labels                 map[string]string
}

type keyInfo struct {
	ServiceAccountEmail string
	Restrictions        struct {
		APITargets []struct {
			Service string
		}
	}
}

type operation struct {
	Name  string
	Done  bool
	Error *struct {
		Code    int
		Details json.RawMessage
	}
}

type googleError struct {
	status int
	step   string
	reason string
}

func (e *googleError) Error() string {
	if e.reason != "" {
		return fmt.Sprintf("Google could not %s (HTTP %d, %s)", e.step, e.status, e.reason)
	}
	return fmt.Sprintf("Google could not %s (HTTP %d)", e.step, e.status)
}

func (e *googleError) StatusCode() int { return e.status }

func statusIs(err error, status int) bool {
	var google *googleError
	return errors.As(err, &google) && google.status == status
}

func (p *providerPlugin) call(callback, step string, req framework.HTTPRequest, result any) error {
	res, err := p.host.Do(context.Background(), callback, req)
	return decodeGoogleResponse(step, res, err, result)
}

func decodeGoogleResponse(step string, res framework.HTTPResponse, err error, result any) error {
	if err != nil {
		return failure(http.StatusBadGateway, "Could not contact Google to "+step)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &googleError{status: res.StatusCode, step: step, reason: safeReason(res.Body)}
	}
	if result != nil && json.Unmarshal(res.Body, result) != nil {
		return failure(http.StatusBadGateway, "Google returned an invalid response while trying to "+step)
	}
	return nil
}

// Use fixed diagnostics rather than forwarding bodies, which can contain
// identity data, OAuth material, or API keys.
func safeReason(data []byte) string {
	for _, reason := range []string{"FREE_TIER_VERIFICATION_REQUIRED", "TERMS_OF_SERVICE_REQUIRED", "TERMS_OF_SERVICE_NOT_ACCEPTED",
		"SERVICE_DISABLED", "SERVICE_NOT_AVAILABLE", "SERVICE_CONFIG_NOT_FOUND", "ACCESS_TOKEN_SCOPE_INSUFFICIENT",
		"ORG_RESTRICTION_VIOLATION", "USER_PROJECT_DENIED", "BILLING_DISABLED", "BILLING_NOT_ENABLED",
		"CONSUMER_INVALID", "PROJECT_NOT_FOUND", "FAILED_PRECONDITION", "INVALID_ARGUMENT", "invalid_grant"} {
		if strings.Contains(string(data), reason) {
			return reason
		}
	}
	return ""
}

func (s *setup) api(p *providerPlugin, cb, step, method, endpoint string, input, result any) error {
	h := http.Header{"Authorization": {"Bearer " + s.token}, "Accept": {"application/json"}}
	var body []byte
	if input != nil {
		h.Set("Content-Type", "application/json")
		body, _ = json.Marshal(input)
	}
	return p.call(cb, step, framework.HTTPRequest{Method: method, URL: endpoint, Headers: h, Body: body}, result)
}

// Long-running Google operations are advanced once per dashboard poll. Never
// hold a host HTTP callback open while a Google Cloud operation is pending.
func (s *setup) begin(p *providerPlugin, cb, step, base, path string, input any, next stage) error {
	var op operation
	err := s.api(p, cb, step, http.MethodPost, base+path, input, &op)
	if err != nil {
		return err
	}
	if err := operationError(step, op); err != nil {
		return err
	}
	if !op.Done {
		validName := strings.HasPrefix(op.Name, "operations/") ||
			(base == keyAPI && strings.HasPrefix(op.Name, s.keyParent()+"/operations/"))
		if !validName || strings.ContainsAny(op.Name, "?#%\\") || strings.Contains(op.Name, "..") {
			return failure(http.StatusBadGateway, "Google Cloud did not return a valid setup operation")
		}
		s.operation, s.opBase, s.opStep, s.opStage = op.Name, base, step, s.phase
	}
	s.phase = next
	return nil
}

func operationError(step string, op operation) error {
	if op.Error == nil {
		return nil
	}
	// Long-running Google operations return canonical gRPC codes.
	status := map[int]int{3: 400, 5: 404, 6: 409, 7: 403, 8: 429, 9: 400, 14: 503}[op.Error.Code]
	if status == 0 {
		status = http.StatusBadGateway
	}
	reason := safeReason(op.Error.Details)
	if reason == "" && op.Error.Code == 9 {
		reason = "FAILED_PRECONDITION"
	}
	return &googleError{status: status, step: step, reason: reason}
}

func serviceToEnable(phase stage) (id, name string) {
	switch phase {
	case enableGemini:
		return geminiService, "Gemini Developer API"
	case enableIAM:
		return "iam.googleapis.com", "Identity and Access Management API"
	case enableKeys:
		return "apikeys.googleapis.com", "API Keys API"
	case enableVertex:
		return vertexService, "Vertex AI API"
	default:
		return "", ""
	}
}

func (s *setup) serviceEnableError(phase stage, err error) error {
	var google *googleError
	if !errors.As(err, &google) {
		return err
	}
	id, _ := serviceToEnable(phase)
	if id == geminiService && requiresGoogleVerification(google.reason) {
		return verificationError(s.number)
	}
	project := s.project
	if phase == enableVertex {
		// Google may refuse Vertex AI until the billing account is paid up.
		// Enroll anyway; requests fail until billing works.
		if strings.HasPrefix(google.reason, "BILLING_") {
			s.phase = finished
			return nil
		}
		project = s.vertex
	}
	// A new project can appear in Resource Manager before Service Usage can
	// enable APIs for it. Give ambiguous and transient errors a few more polls.
	transient := google.reason == "" || google.reason == "FAILED_PRECONDITION" ||
		google.reason == "CONSUMER_INVALID" || google.reason == "PROJECT_NOT_FOUND"
	switch google.status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable:
		if transient && s.enableRetries < 3 {
			s.enableRetries++
			s.phase = phase
			return nil
		}
	}
	return failure(google.status, fmt.Sprintf("%s. Check this API in Google Cloud using the same account: https://console.cloud.google.com/apis/library/%s?project=%s. Then enroll again.", google.Error(), id, project))
}

func (s *setup) advance(p *providerPlugin, cb string) (*credential, error) {
	if s.operation != "" {
		var op operation
		err := s.api(p, cb, s.opStep, http.MethodGet, s.opBase+s.operation, nil, &op)
		if err == nil {
			err = operationError(s.opStep, op)
		}
		if err != nil {
			if s.opBase == serviceUsage {
				s.operation, s.opBase = "", ""
				return nil, s.serviceEnableError(s.opStage, err)
			}
			return nil, err
		}
		if op.Done {
			s.operation, s.opBase = "", ""
			s.enableRetries = 0
		}
		return nil, nil
	}
	err := s.next(p, cb)
	if err != nil {
		return nil, err
	}
	switch {
	case s.phase == probeKey && s.key != "":
		if err := p.probe(cb, s.number, s.key); err != nil {
			return nil, err
		}
		if s.refresh == "" {
			return s.credential(), nil
		}
		s.phase = findVertex
	case s.phase == finished && s.operation == "":
		return s.credential(), nil
	}
	return nil, nil
}

func (s *setup) credential() *credential {
	c := &credential{Type: provider, Project: s.project, Email: s.mail, Key: s.key}
	if s.vertex != "" {
		c.Vertex, c.Refresh = s.vertex, s.refresh
	}
	return c
}

type billedProject struct {
	id      string
	enabled bool
	created string
}

type billingAccount struct {
	Name string `json:"name"`
	Open bool   `json:"open"`
}

// Bounds keep one enrollment poll well within the dashboard's request timeout.
const (
	maxBillingAccounts = 10
	maxProjectLookups  = 5
)

// findBilledProject picks the account's project for Vertex AI: one linked to
// a billing account the account can see, preferring billing that is enabled,
// then the newest project. Billing linked but disabled still counts, so an
// account can be enrolled before its billing account is paid up. It asks
// Cloud Billing for each billing account's projects rather than checking every
// project the account can access, which can take minutes.
func (s *setup) findBilledProject(p *providerPlugin, cb string) (string, error) {
	var accounts struct {
		BillingAccounts []billingAccount `json:"billingAccounts"`
	}
	if err := s.api(p, cb, "list the account's billing accounts", http.MethodGet, billingAPI+"billingAccounts?pageSize=50", nil, &accounts); err != nil {
		return "", err
	}
	slices.SortStableFunc(accounts.BillingAccounts, func(a, b billingAccount) int { return boolOrder(a.Open, b.Open) })
	var found []billedProject
	for i, account := range accounts.BillingAccounts {
		if i == maxBillingAccounts {
			break
		}
		if !strings.HasPrefix(account.Name, "billingAccounts/") || strings.ContainsAny(account.Name, "?#%\\") || strings.Contains(account.Name, "..") {
			continue
		}
		var linked struct {
			Projects []struct {
				ProjectID string `json:"projectId"`
				Enabled   bool   `json:"billingEnabled"`
			} `json:"projectBillingInfo"`
		}
		err := s.api(p, cb, "list a billing account's projects", http.MethodGet, billingAPI+account.Name+"/projects?pageSize=100", nil, &linked)
		if statusIs(err, http.StatusForbidden) {
			continue // the account may see a billing account without listing its projects
		}
		if err != nil {
			return "", err
		}
		for _, project := range linked.Projects {
			if project.ProjectID != s.project && vertexPattern.MatchString(project.ProjectID) {
				found = append(found, billedProject{id: project.ProjectID, enabled: project.Enabled})
			}
		}
	}
	slices.SortStableFunc(found, func(a, b billedProject) int { return boolOrder(a.enabled, b.enabled) })
	// Among the best candidates, keep projects the account can use and pick
	// the newest. A billing administrator can see projects it cannot change.
	var usable []billedProject
	for _, project := range found {
		if len(usable) == maxProjectLookups || (len(usable) > 0 && project.enabled != usable[0].enabled) {
			break
		}
		var info projectInfo
		err := s.api(p, cb, "look up a billed project", http.MethodGet, resourceManager+"projects/"+project.id, nil, &info)
		if statusIs(err, http.StatusForbidden) || statusIs(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.State == "ACTIVE" {
			project.created = info.CreateTime
			usable = append(usable, project)
		}
	}
	if len(usable) == 0 {
		return "", nil
	}
	slices.SortStableFunc(usable, func(a, b billedProject) int { return strings.Compare(b.created, a.created) })
	return usable[0].id, nil
}

// boolOrder sorts true before false.
func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	}
	return 1
}

func (s *setup) next(p *providerPlugin, cb string) error {
	switch s.phase {
	case identify:
		var account struct {
			Sub, Email    string
			EmailVerified bool `json:"email_verified"`
		}
		if err := s.api(p, cb, "identify the Google account", http.MethodGet, userEndpoint, nil, &account); err != nil {
			return err
		}
		if account.Sub == "" || account.Email == "" || !account.EmailVerified {
			return failure(http.StatusUnauthorized, "Google did not confirm the account's email")
		}
		digest := sha256.Sum256([]byte(s.clientID + "\x00" + account.Sub))
		s.project = "cliproxy-gf-" + hex.EncodeToString(digest[:8])
		s.mail, s.phase = account.Email, findProject
	case findProject:
		var info projectInfo
		err := s.api(p, cb, "look up the Free tier project", http.MethodGet, resourceManager+"projects/"+s.project, nil, &info)
		var google *googleError
		// Resource Manager can hide an absent project behind PERMISSION_DENIED.
		// Create is safe here: the globally unique ID cannot replace an existing project.
		if statusIs(err, http.StatusNotFound) || (errors.As(err, &google) && google.status == http.StatusForbidden &&
			(google.reason == "" || google.reason == "PROJECT_NOT_FOUND")) {
			if s.retryUntil.IsZero() {
				s.phase = createProject
				return nil
			}
			if time.Now().Before(s.retryUntil) {
				return nil // project creation has completed but IAM may still be propagating
			}
		}
		if err != nil {
			return err
		}
		num := strings.TrimPrefix(info.Name, "projects/")
		if info.ProjectID != s.project || info.State != "ACTIVE" || !numberPattern.MatchString(num) ||
			info.Labels[ownerLabel] != strings.TrimPrefix(s.project, "cliproxy-gf-") {
			return failure(http.StatusConflict, "The project ID is in use by a project not managed by this Gemini Free enrollment")
		}
		s.number, s.phase, s.retryUntil = num, checkBilling, time.Time{}
	case createProject:
		body := map[string]any{"projectId": s.project, "displayName": "CLIProxy Gemini Free",
			"labels": map[string]string{ownerLabel: strings.TrimPrefix(s.project, "cliproxy-gf-")}}
		err := s.begin(p, cb, "create a Google Cloud project", resourceManager, "projects", body, findProject)
		if err == nil || statusIs(err, http.StatusConflict) {
			s.retryUntil, s.phase = time.Now().Add(45*time.Second), findProject
			return nil
		}
		return err
	case checkBilling:
		var info struct {
			Enabled bool   `json:"billingEnabled"`
			Account string `json:"billingAccountName"`
		}
		if err := s.api(p, cb, "check Cloud Billing", http.MethodGet, billingAPI+"projects/"+s.project+"/billingInfo", nil, &info); err != nil {
			return err
		}
		if info.Enabled || info.Account != "" {
			return failure(http.StatusConflict, "Google Cloud billing is linked to this project; Gemini Free enrollment requires a project without billing")
		}
		s.phase = enableGemini
	case enableGemini, enableIAM, enableKeys:
		phase := s.phase
		id, name := serviceToEnable(phase)
		path := "projects/" + s.number + "/services/" + id + ":enable"
		if err := s.begin(p, cb, "enable the "+name, serviceUsage, path, struct{}{}, phase+1); err != nil {
			return s.serviceEnableError(phase, err)
		}
		if s.operation == "" {
			s.enableRetries = 0
		}
	case getAccount:
		err := s.api(p, cb, "look up the Gemini key service account", http.MethodGet, iamAPI+s.accountPath(), nil, nil)
		if statusIs(err, http.StatusNotFound) {
			if s.retryUntil.IsZero() {
				s.phase = createAccount
				return nil
			}
			if time.Now().Before(s.retryUntil) {
				return nil
			}
		}
		if err != nil {
			return err
		}
		s.phase = getKey
	case createAccount:
		err := s.api(p, cb, "create a service account for the Gemini key", http.MethodPost, iamAPI+"projects/"+s.project+"/serviceAccounts",
			map[string]any{"accountId": serviceAccount, "serviceAccount": map[string]string{"displayName": "CLIProxy Gemini Free key"}}, nil)
		if err != nil && !statusIs(err, http.StatusConflict) {
			return err
		}
		// Give IAM time to propagate the new account before binding the key.
		s.retryUntil, s.phase = time.Now().Add(time.Minute), getAccount
	case getKey:
		var info keyInfo
		err := s.api(p, cb, "look up the Gemini authorization key", http.MethodGet, keyAPI+s.keyPath(), nil, &info)
		if statusIs(err, http.StatusNotFound) {
			s.phase = createKey
			return nil
		}
		if err != nil {
			return err
		}
		if info.ServiceAccountEmail != s.accountEmail() || len(info.Restrictions.APITargets) != 1 ||
			info.Restrictions.APITargets[0].Service != geminiService {
			return failure(http.StatusConflict, "The existing key is not bound to the expected service account and restricted to the Gemini API")
		}
		s.phase = readKey
	case createKey:
		body := map[string]any{"displayName": "CLIProxy Gemini Free", "serviceAccountEmail": s.accountEmail(),
			"restrictions": map[string]any{"apiTargets": []map[string]string{{"service": geminiService}}}}
		err := s.begin(p, cb, "create a restricted Gemini authorization key", keyAPI, s.keyParent()+"/keys?keyId="+keyID, body, getKey)
		if statusIs(err, http.StatusConflict) {
			s.phase = getKey
			return nil
		}
		return err
	case readKey:
		var data struct {
			Key string `json:"keyString"`
		}
		if err := s.api(p, cb, "read the Gemini authorization key", http.MethodGet, keyAPI+s.keyPath()+"/keyString", nil, &data); err != nil {
			return err
		}
		if data.Key == "" {
			return failure(http.StatusBadGateway, "Google did not return the Gemini authorization key")
		}
		s.key, s.phase = data.Key, probeKey
	case findVertex:
		project, err := s.findBilledProject(p, cb)
		if err != nil {
			return err
		}
		if project == "" {
			s.phase = finished // no billed project: serve the Free tier only
			return nil
		}
		s.vertex, s.phase = project, enableVertex
	case enableVertex:
		path := "projects/" + s.vertex + "/services/" + vertexService + ":enable"
		if err := s.begin(p, cb, "enable the Vertex AI API", serviceUsage, path, struct{}{}, finished); err != nil {
			return s.serviceEnableError(enableVertex, err)
		}
	}
	return nil
}

func (s *setup) accountEmail() string {
	return serviceAccount + "@" + s.project + ".iam.gserviceaccount.com"
}
func (s *setup) accountPath() string {
	return "projects/" + s.project + "/serviceAccounts/" + s.accountEmail()
}
func (s *setup) keyParent() string { return "projects/" + s.number + "/locations/global" }
func (s *setup) keyPath() string   { return s.keyParent() + "/keys/" + keyID }

func requiresGoogleVerification(reason string) bool {
	return reason == "FREE_TIER_VERIFICATION_REQUIRED" || reason == "TERMS_OF_SERVICE_REQUIRED" ||
		reason == "TERMS_OF_SERVICE_NOT_ACCEPTED"
}

func verificationError(number string) error {
	return failure(http.StatusForbidden, "Google requires a one-time AI Studio terms or verification step. Open https://aistudio.google.com/verify?project="+number+" with the same account, complete Google's prompt, then enroll again.")
}

func (p *providerPlugin) probe(cb, number, key string) error {
	var response struct {
		Candidates []json.RawMessage
	}
	err := p.call(cb, "validate Free tier generation", geminiRequest(key, probeModel, "generateContent", []byte(
		`{"contents":[{"parts":[{"text":"Reply with OK."}]}],"generationConfig":{"maxOutputTokens":8,"thinkingConfig":{"thinkingLevel":"MINIMAL"}}}`)), &response)
	var google *googleError
	if errors.As(err, &google) && requiresGoogleVerification(google.reason) {
		return verificationError(number)
	}
	if err != nil {
		return err
	}
	if len(response.Candidates) == 0 {
		return failure(http.StatusBadGateway, "Google returned no candidate in the Free tier probe")
	}
	return nil
}
