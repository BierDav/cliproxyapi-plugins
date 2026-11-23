package sse

import "bytes"

// DataFrame keeps only the data lines of an SSE frame, joined into a single
// "data: ..." line. The SDK's stream translators read data lines only and
// pass frames that start with an event line through unchanged. A frame
// without data lines is returned as is.
func DataFrame(frame []byte) []byte {
	data, ok := dataLines(frame)
	if !ok {
		return frame
	}
	return append([]byte("data: "), data...)
}

// DataPayload strips the SSE envelope from a frame for the host's Chat
// Completions handler, which adds its own envelope and terminal marker. The
// "[DONE]" marker becomes nil. A frame without data lines is only trimmed.
func DataPayload(frame []byte) []byte {
	frame = bytes.TrimSpace(frame)
	if data, ok := dataLines(frame); ok {
		frame = data
	}
	if bytes.Equal(frame, []byte("[DONE]")) {
		return nil
	}
	return frame
}

func dataLines(frame []byte) ([]byte, bool) {
	var data [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
	}
	if len(data) == 0 {
		return nil, false
	}
	return bytes.Join(data, []byte("\n")), true
}
