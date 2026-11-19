package handlers

import (
	"context"
	"errors"
	"net/http"
)

// executionError is the error message a failed execution through the auth
// manager is reported as: the status the error carries, 499 when the client's
// context ended it, and the headers the upstream sent with it. A failure of
// the account's proxy is answered as egressUnavailableMessage describes.
func executionError(err error) *StreamErrorMessage {
	if msg := egressUnavailableMessage(err); msg != nil {
		return msg
	}
	if msg := providerAccessFrozenMessage(err); msg != nil {
		return msg
	}
	status := http.StatusInternalServerError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = 499 // Client Closed Request
	} else if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
		if code := se.StatusCode(); code > 0 {
			status = code
		}
	}
	var addon http.Header
	if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
		if hdr := he.Headers(); hdr != nil {
			addon = hdr.Clone()
		}
	}
	return &StreamErrorMessage{StatusCode: status, Error: err, Addon: addon}
}
