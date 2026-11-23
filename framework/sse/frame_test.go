package sse

import "testing"

func TestDataFrame(t *testing.T) {
	for frame, want := range map[string]string{
		"event: response.created\ndata: {\"a\":1}\n\n": `data: {"a":1}`,
		"event: x\r\ndata: {\"a\":1}\r\n\r\n":          `data: {"a":1}`,
		"data: {\"a\":\ndata: 1}\n\n":                  "data: {\"a\":\n1}",
		": keep-alive\n\n":                             ": keep-alive\n\n",
		"event: ping\ndata: {\"type\": \"ping\"}\n\n":  `data: {"type": "ping"}`,
		"data:[DONE]\n\n":                              "data: [DONE]",
	} {
		if got := string(DataFrame([]byte(frame))); got != want {
			t.Errorf("DataFrame(%q) = %q, want %q", frame, got, want)
		}
	}
}

func TestDataPayload(t *testing.T) {
	for frame, want := range map[string]string{
		"data: {\"choices\":[]}\n\n":     `{"choices":[]}`,
		"data: {\"choices\":[]}\r\n\r\n": `{"choices":[]}`,
		`{"choices":[]}`:                 `{"choices":[]}`,
		"data: [DONE]\n\n":               "",
		"[DONE]":                         "",
	} {
		if got := string(DataPayload([]byte(frame))); got != want {
			t.Errorf("DataPayload(%q) = %q, want %q", frame, got, want)
		}
	}
}
