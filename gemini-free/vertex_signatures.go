package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	vertexMigratedSignature        = "skip_thought_signature_validator"
	vertexContextEngineeringMarker = "context_engineering_is_the_way_to_go"
)

// Vertex doesn't identify the rejected part. On that specific error, migrate
// foreign signatures (including Antigravity Gemini and Claude) but keep native
// Vertex signatures, which can be needed by the next tool call in the same turn.
func repairVertexSignatures(body, rejection []byte) []byte {
	if gjson.GetBytes(rejection, "error.status").String() != "INVALID_ARGUMENT" {
		return nil
	}
	switch strings.TrimSuffix(gjson.GetBytes(rejection, "error.message").String(), ".") {
	case "Invalid thought signature", "Corrupted thought signature":
	default:
		return nil
	}

	contents := gjson.GetBytes(body, "contents").Array()
	updated := make([]json.RawMessage, len(contents))
	changed := false
	for i, content := range contents {
		updated[i] = json.RawMessage(content.Raw)
		if content.Get("role").String() != "model" {
			continue
		}
		parts := content.Get("parts").Array()
		revised := make([]json.RawMessage, len(parts))
		firstCallSeen, changedContent := false, false
		for j, part := range parts {
			revised[j] = json.RawMessage(part.Raw)
			call := part.Get("functionCall").Exists() || part.Get("function_call").Exists()
			firstCall := call && !firstCallSeen
			if call || !part.Get("thought").Bool() {
				firstCallSeen = call
			}
			// Server-side tool parts are echoed back as issued by Google.
			if part.Get("toolCall").Exists() || part.Get("toolResponse").Exists() ||
				part.Get("tool_call").Exists() || part.Get("tool_response").Exists() {
				continue
			}
			for _, field := range []string{"thoughtSignature", "thought_signature"} {
				signature := part.Get(field).String()
				if signature == "" || isVertexReplaySignature(signature) {
					continue
				}
				if firstCall {
					revised[j], _ = sjson.SetBytes(revised[j], field, vertexMigratedSignature)
				} else {
					revised[j], _ = sjson.DeleteBytes(revised[j], field)
				}
				changedContent = true
			}
		}
		if changedContent {
			raw, _ := json.Marshal(revised)
			updated[i], _ = sjson.SetRawBytes(updated[i], "parts", raw)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	raw, _ := json.Marshal(updated)
	repaired, _ := sjson.SetRawBytes(body, "contents", raw)
	return repaired
}

// Vertex issues unwrapped Tink signatures (prefix byte and four-byte key ID)
// and accepts both text and base64 spellings of its documented migration markers.
func isVertexReplaySignature(signature string) bool {
	if signature == vertexMigratedSignature || signature == vertexContextEngineeringMarker {
		return true
	}
	unpadded := strings.TrimRight(signature, "=")
	raw, err := base64.RawStdEncoding.DecodeString(unpadded)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(unpadded)
	}
	if err != nil {
		return false
	}
	return len(raw) > 5 && raw[0] == 0x01 ||
		string(raw) == vertexMigratedSignature || string(raw) == vertexContextEngineeringMarker
}

func (p *providerPlugin) readVertexRejection(ctx context.Context, streamID string) []byte {
	const maxErrorBytes = 8 << 10
	var body []byte
	for {
		part, err := p.host.ReadStream(ctx, streamID)
		if err != nil || part.Error != "" || len(body)+len(part.Payload) > maxErrorBytes {
			return nil
		}
		body = append(body, part.Payload...)
		if part.Done || json.Valid(body) {
			return body
		}
		if len(part.Payload) == 0 {
			return nil
		}
	}
}
