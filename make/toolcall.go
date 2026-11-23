package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Client tools exist only as <tool_call name="…">{json}</tool_call> text in
// the model's reply. Models drift from that format in small ways: an OpenAI
// tool namespace on the name, a JSON string escaped for the language inside
// it rather than for JSON, raw line breaks in strings. A block that cannot be
// decoded must never silently reach the client as text, because the client
// then sees no tool call and the agent's turn ends. So decoding repairs what
// has exactly one sensible reading, and reports everything else as a problem
// that the executor sends back to the model for a corrected reply.

type parsedCall struct {
	Name      string
	Arguments string
}

type parsedReply struct {
	Content  string
	Calls    []parsedCall
	Problems []string
}

var (
	toolCallOpen = regexp.MustCompile(`<tool_call\b([^>]*)>`)
	toolCallName = regexp.MustCompile(`\bname\s*=\s*"([^"]*)"`)
)

const toolCallClose = "</tool_call>"

// parseReply splits the model's text into visible content and tool calls.
// Blocks that do not decode stay in the content and are listed in Problems,
// except blocks quoted inside Markdown code, which are prose about the format.
func parseReply(text string, tools map[string]bool) parsedReply {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "<assistant>")
	text = strings.TrimSpace(strings.TrimSuffix(text, "</assistant>"))
	if len(tools) == 0 {
		return parsedReply{Content: text}
	}
	var reply parsedReply
	var content strings.Builder
	last, pos, block := 0, 0, 0
	for {
		loc := toolCallOpen.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		block++
		start, bodyStart := pos+loc[0], pos+loc[1]
		attributes := text[pos+loc[2] : pos+loc[3]]
		body, end := blockBody(text, bodyStart)
		pos = end
		call, err := decodeCall(attributes, body, tools)
		if err != nil {
			if !insideCode(text, start) {
				label := ""
				if match := toolCallName.FindStringSubmatch(attributes); match != nil {
					label = fmt.Sprintf(" (name %q)", match[1])
				}
				reply.Problems = append(reply.Problems, fmt.Sprintf("tool_call block %d%s: %v", block, label, err))
			}
			continue
		}
		content.WriteString(text[last:start])
		last = end
		reply.Calls = append(reply.Calls, call)
	}
	content.WriteString(text[last:])
	reply.Content = strings.TrimSpace(content.String())
	return reply
}

// blockBody finds the arguments of a block that starts at bodyStart and the
// end of the whole block. The JSON object is delimited by its own structure,
// so arguments may contain "</tool_call>" (for example when editing this very
// protocol). The final block may lack its closing tag.
func blockBody(text string, bodyStart int) (string, int) {
	i := skipFence(text, skipSpace(text, bodyStart))
	if i < len(text) && text[i] == '{' {
		if objectEnd, ok := scanObject(text, i); ok {
			after := skipSpace(text, objectEnd)
			if strings.HasPrefix(text[after:], "```") {
				after = skipSpace(text, after+3)
			}
			if strings.HasPrefix(text[after:], toolCallClose) {
				return text[bodyStart:objectEnd], after + len(toolCallClose)
			}
			if after == len(text) {
				return text[bodyStart:objectEnd], after
			}
		}
	}
	if offset := strings.Index(text[bodyStart:], toolCallClose); offset >= 0 {
		return text[bodyStart : bodyStart+offset], bodyStart + offset + len(toolCallClose)
	}
	return text[bodyStart:], len(text)
}

func skipSpace(text string, i int) int {
	for i < len(text) && strings.ContainsRune(" \t\r\n", rune(text[i])) {
		i++
	}
	return i
}

func skipFence(text string, i int) int {
	if !strings.HasPrefix(text[i:], "```") {
		return i
	}
	i += 3
	for i < len(text) && text[i] != '\n' && text[i] != '{' {
		i++
	}
	return skipSpace(text, i)
}

// scanObject returns the index after the object that opens at text[start].
// A backslash in a string always skips the next byte, valid escape or not.
func scanObject(text string, start int) (int, bool) {
	depth, inString := 0, false
	for i := start; i < len(text); i++ {
		c := text[i]
		switch {
		case inString && c == '\\':
			i++
		case c == '"':
			inString = !inString
		case inString:
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// insideCode reports whether position i is inside a fenced code block or
// directly follows an inline code backtick.
func insideCode(text string, i int) bool {
	return strings.Count(text[:i], "```")%2 == 1 || (i > 0 && text[i-1] == '`')
}

func decodeCall(attributes, body string, tools map[string]bool) (parsedCall, error) {
	body = strings.TrimSpace(body)
	body = strings.TrimSpace(strings.TrimSuffix(skipFenceString(body), "```"))
	written := ""
	if match := toolCallName.FindStringSubmatch(attributes); match != nil {
		written = match[1]
	} else {
		// <tool_call>{"name": "...", "arguments": {...}}</tool_call>, the
		// format many models are trained on.
		var wrapped struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		arguments, err := decodeArguments(body)
		if err != nil {
			return parsedCall{}, err
		}
		if json.Unmarshal([]byte(arguments), &wrapped) != nil || wrapped.Name == "" {
			return parsedCall{}, errors.New(`the opening tag has no name attribute; write <tool_call name="TOOL_NAME">`)
		}
		written, body = wrapped.Name, string(wrapped.Arguments)
		var encoded string
		if json.Unmarshal(wrapped.Arguments, &encoded) == nil {
			body = encoded
		}
	}
	name, ok := resolveToolName(written, tools)
	if !ok {
		return parsedCall{}, fmt.Errorf("%q is not a client tool; use a name exactly as listed under \"Client tools\"", written)
	}
	arguments, err := decodeArguments(body)
	if err != nil {
		return parsedCall{}, err
	}
	return parsedCall{Name: name, Arguments: arguments}, nil
}

func skipFenceString(body string) string {
	return body[skipFence(body, 0):]
}

// resolveToolName maps the name a model wrote onto a declared client tool.
// OpenAI models are trained to address tools as "functions.NAME", and with
// namespaced names such as "mcp__server__tool" in view they sometimes write
// "functions__NAME".
func resolveToolName(name string, tools map[string]bool) (string, bool) {
	name = strings.TrimSpace(name)
	if tools[name] {
		return name, true
	}
	for _, namespace := range []string{"functions", "function", "tools"} {
		for _, separator := range []string{".", "__", "/", ":"} {
			if rest, found := strings.CutPrefix(name, namespace+separator); found && tools[rest] {
				return rest, true
			}
		}
	}
	return "", false
}

// decodeArguments returns the compact JSON object of a block body. Bodies
// that are not valid JSON are repaired when the repair is unambiguous.
func decodeArguments(body string) (string, error) {
	if body == "" {
		return "{}", nil
	}
	compact, err := compactObject(body)
	if err == nil {
		return compact, nil
	}
	repaired := repairJSON(body)
	if repaired == body {
		return "", describeJSONError(body, err)
	}
	compact, err = compactObject(repaired)
	if err != nil {
		return "", describeJSONError(repaired, err)
	}
	return compact, nil
}

// compactObject validates a single JSON object and compacts it, keeping the
// original key order and number precision.
func compactObject(body string) (string, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if decoder.More() {
		return "", fmt.Errorf("unexpected text after the JSON object at byte %d", decoder.InputOffset())
	}
	if _, ok := value.(map[string]any); !ok {
		return "", errors.New("the body must be a JSON object")
	}
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(body)); err != nil {
		return "", err
	}
	return out.String(), nil
}

// repairJSON fixes the mistakes models make when they write a JSON string by
// hand and that have one reading:
//   - An invalid escape such as \$ or \' becomes a literal backslash. It comes
//     from escaping for the language inside the string (a JavaScript template
//     literal, a shell command) and forgetting the JSON layer.
//   - Raw control characters inside strings, usually line breaks, are escaped.
//   - A trailing comma before } or ] is removed.
//
// Unescaped quotes inside strings are not repaired: where the string ends is
// ambiguous, so the model has to rewrite the block.
func repairJSON(body string) string {
	out := make([]byte, 0, len(body)+16)
	inString := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inString {
			switch {
			case c == '\\':
				if i+1 < len(body) && validEscape(body[i+1:]) {
					out = append(out, c, body[i+1])
					i++
				} else {
					out = append(out, '\\', '\\')
				}
			case c == '"':
				inString = false
				out = append(out, c)
			case c == '\n':
				out = append(out, '\\', 'n')
			case c == '\r':
				out = append(out, '\\', 'r')
			case c == '\t':
				out = append(out, '\\', 't')
			case c < 0x20:
				out = fmt.Appendf(out, `\u%04x`, c)
			default:
				out = append(out, c)
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '}', ']':
			j := len(out)
			for j > 0 && strings.IndexByte(" \t\r\n", out[j-1]) >= 0 {
				j--
			}
			if j > 0 && out[j-1] == ',' {
				out = append(out[:j-1], out[j:]...)
			}
		}
		out = append(out, c)
	}
	return string(out)
}

func validEscape(rest string) bool {
	switch rest[0] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return true
	case 'u':
		if len(rest) < 5 {
			return false
		}
		for _, h := range rest[1:5] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", h) {
				return false
			}
		}
		return true
	}
	return false
}

// describeJSONError names the error and quotes the text around it, so the
// model can find the mistake in a long body.
func describeJSONError(body string, err error) error {
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		return fmt.Errorf("the body is not a valid JSON object: %v", err)
	}
	offset := int(syntax.Offset)
	from, to := max(0, offset-60), min(len(body), offset+20)
	return fmt.Errorf("the body is not valid JSON: %v at byte %d, near %q", err, offset, body[from:to])
}
