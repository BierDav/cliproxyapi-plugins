package main

import (
	"encoding/json"
	"errors"
	"strings"
)

// gatewayVersion is bumped whenever the scenario blueprint changes; outdated
// gateways are replaced at the next credential refresh.
const gatewayVersion = 1

const gatewayName = "CLIProxyAPI gateway (managed, do not edit)"

// The gateway is an on-demand scenario: Start scenario -> Run an agent ->
// Transform usage to JSON -> Return output. It runs without tools; client
// tool calls are expressed in the agent's text and returned to the client,
// which keeps the loop stateless and the prompt prefix cacheable.
func gatewayBlueprint(teamID, connectionID int64) map[string]any {
	return map[string]any{
		"teamId":       teamID,
		"name":         gatewayName,
		"scheduling":   map[string]any{"type": "on-demand"},
		"autoActivate": true,
		"interface": map[string]any{
			"input": []map[string]any{
				{"name": "model", "type": "text", "required": true},
				{"name": "reasoning", "type": "text"},
				{"name": "system", "type": "text", "multiline": true},
				{"name": "message", "type": "text", "multiline": true, "required": true},
			},
			"output": []map[string]any{
				{"name": "response", "type": "text"},
				{"name": "reasoning", "type": "text"},
				{"name": "usage", "type": "text"},
			},
		},
		"modules": []map[string]any{
			{"id": 1, "module": "scenario-service:StartSubscenario", "version": 2, "root": "main"},
			{"id": 2, "module": "ai-local-agent:RunLocalAIAgent", "version": 0, "follows": 1,
				"config": map[string]any{
					"parameters": map[string]any{"makeConnectionId": connectionID},
					"mapper": map[string]any{
						"systemPrompt": "{{1.system}}",
						"message":      "{{1.message}}",
						"outputType":   "text",
						"defaultModel": "{{1.model}}",
						"modelConfig": map[string]any{
							// Make's own history and compaction would rewrite the
							// prompt and break prefix caching.
							"aiCompactionEnabled":        false,
							"iterationsFromHistoryCount": 0,
							"recursionLimit":             3,
							"timeout":                    600,
							"reasoningLevel":             "{{1.reasoning}}",
						},
					},
				}},
			{"id": 3, "module": "json:TransformToJSON", "version": 1, "follows": 2,
				"config": map[string]any{"mapper": map[string]any{"object": "{{2.metadata.tokenUsageSummary}}"}}},
			{"id": 4, "module": "scenario-service:ReturnData", "version": 2, "follows": 3,
				"config": map[string]any{"mapper": map[string]any{
					"response":  "{{2.response}}",
					"reasoning": "{{2.reasoning}}",
					"usage":     "{{3.json}}",
				}}},
		},
	}
}

type environment struct {
	Organizations []struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Teams []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"teams"`
	} `json:"organizations"`
}

// ensureGateway provisions (or replaces) the account's gateway scenario and
// persists its identifiers.
func ensureGateway(callback, key string, s storage) (storage, error) {
	raw, err := callTool(callback, s.AccessToken, "environment_get", map[string]any{})
	if err != nil {
		return s, err
	}
	var env environment
	if json.Unmarshal(raw, &env) != nil {
		return s, newStatusError(502, "Invalid Make environment")
	}
	teamID, orgName := int64(0), ""
	wanted := currentConfig().TeamID
	for _, org := range env.Organizations {
		if org.ID != s.OrganizationID {
			continue
		}
		orgName = org.Name
		for _, team := range org.Teams {
			if wanted == 0 || team.ID == wanted {
				teamID = team.ID
				break
			}
		}
	}
	if teamID == 0 {
		if wanted != 0 {
			return s, newStatusError(400, "Make team %d is not in the signed-in organization", wanted)
		}
		return s, newStatusError(400, "The signed-in Make organization has no team")
	}
	connectionID, err := aiProviderConnection(callback, s.AccessToken, s.OrganizationID, teamID)
	if err != nil {
		return s, err
	}
	if s.ScenarioID != 0 {
		// Replace the old gateway; ignore "already deleted".
		_, _ = callTool(callback, s.AccessToken, "scenario_delete", map[string]any{"scenarioId": s.ScenarioID})
	}
	created, err := callTool(callback, s.AccessToken, "scenario_create", gatewayBlueprint(teamID, connectionID))
	if err != nil {
		return s, err
	}
	var result struct {
		ScenarioID int64 `json:"scenarioId"`
		IsActive   bool  `json:"isActive"`
		Errors     []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(created, &result) != nil || result.ScenarioID == 0 {
		message := "Make rejected the gateway scenario"
		if len(result.Errors) > 0 {
			message += ": " + truncate(result.Errors[0].Message, 300)
		}
		return s, newStatusError(502, "%s", message)
	}
	if !result.IsActive {
		if _, err := callTool(callback, s.AccessToken, "scenario_activate", map[string]any{"scenarioId": result.ScenarioID}); err != nil {
			return s, err
		}
	}
	return updateStorage(key, func(next *storage) {
		next.OrganizationName = orgName
		next.TeamID = teamID
		next.ConnectionID = connectionID
		next.ScenarioID = result.ScenarioID
		next.GatewayVersion = gatewayVersion
	})
}

// aiProviderConnection finds the organization's built-in "Make's AI Provider"
// connection, which bills model usage to Make credits.
func aiProviderConnection(callback, token string, organizationID, teamID int64) (int64, error) {
	raw, err := callTool(callback, token, "module_spec", map[string]any{"organizationId": organizationID, "teamId": teamID, "modules": []string{"ai-local-agent:RunLocalAIAgent"}})
	if err != nil {
		return 0, err
	}
	var spec struct {
		Modules []struct {
			Connection struct {
				Existing []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
				} `json:"existing"`
			} `json:"connection"`
		} `json:"modules"`
	}
	if json.Unmarshal(raw, &spec) != nil || len(spec.Modules) == 0 {
		return 0, newStatusError(502, "Invalid Make module specification")
	}
	for _, connection := range spec.Modules[0].Connection.Existing {
		if strings.Contains(strings.ToLower(connection.Name), "make's ai provider") || strings.Contains(strings.ToLower(connection.Name), "make ai provider") {
			return connection.ID, nil
		}
	}
	return 0, newStatusError(400, "No \"Make's AI Provider\" connection exists in the Make team")
}

// isMissingScenario reports a run against a deleted or deactivated gateway.
func isMissingScenario(err error) bool {
	var failure *mcpError
	if !errors.As(err, &failure) {
		return false
	}
	message := strings.ToLower(failure.message)
	return failure.status == 404 || strings.Contains(message, "not found") || strings.Contains(message, "inactive") ||
		strings.Contains(message, "not active") || strings.Contains(message, "activate it")
}
