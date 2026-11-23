package provider

import (
	"fmt"
	"net/http"

	"github.com/BierDav/cliproxyapi-plugins/framework"
)

// StatusError is a failure whose envelope fields the provider chooses.
type StatusError = framework.Error

func statusError(code, message string, status int) error {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &StatusError{Code: code, Message: message, HTTPStatus: status}
}

func upstreamStatusError(status int, detail string) error {
	message := fmt.Sprintf("Copilot upstream returned HTTP %d", status)
	if detail != "" {
		message += ": " + detail
	}
	return &StatusError{
		Code:       "upstream_error",
		Message:    message,
		HTTPStatus: status,
		Retryable:  status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500,
	}
}
