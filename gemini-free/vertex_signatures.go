package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const vertexMigratedSignature = "skip_thought_signature_validator" // Google's marker for migrated function calls.

// Vertex gives no part index for this error. Only replace the wrapped Gemini
// signatures observed on Antigravity. Native Vertex signatures are unwrapped;
// Google can still use them for subsequent tool calls in the same turn.
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
				if !wrappedThoughtSignature(part.Get(field).String()) {
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

// Antigravity wraps the opaque Tink output (prefix 0x01) in two protobuf bytes
// fields. We recognize the wire envelope without interpreting the signature.
func wrappedThoughtSignature(signature string) bool {
	if signature == "" {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	for _, tag := range []byte{0x12, 0x0a} {
		if len(raw) < 2 || raw[0] != tag {
			return false
		}
		n, size := binary.Uvarint(raw[1:])
		if size <= 0 || n != uint64(len(raw)-1-size) {
			return false
		}
		raw = raw[1+size:]
	}
	// Tink's output starts with a prefix byte and a four-byte key ID.
	return len(raw) > 5 && raw[0] == 0x01
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
