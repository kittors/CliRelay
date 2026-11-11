package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
)

// egressUnavailableError is the client-facing form of a request that failed
// on the proxy its account leaves through (package egressbreaker). Its text is
// the OpenAI-shaped body; the other protocols rewrite it as any other error.
type egressUnavailableError struct {
	body string
	// retryAfter is the Retry-After to send, in seconds; 0 sends none.
	retryAfter int
}

func (e *egressUnavailableError) Error() string { return e.body }

// egressUnavailableMessage turns a proxy failure into the error a client is
// answered with, or returns nil when err is not one.
//
// Such an error carries no HTTP status, and used to go out as a bare 500
// whose message quoted the upstream URL and the proxy's address. A failed
// connect to the proxy was even reported as 499, the client's own doing,
// because Go's dial timeout matches context.DeadlineExceeded. Callers must
// therefore check for it before they look at the context errors.
//
// It is answered with 503 and the code egress_unavailable: the service cannot
// reach the upstream for now and the client should back off, which is what
// Retry-After and reset_seconds say for how long. The message names neither
// the proxy nor the upstream.
func egressUnavailableMessage(err error) *StreamErrorMessage {
	egressErr, ok := egressbreaker.AsError(err)
	if !ok {
		return nil
	}
	seconds := egressErr.RetryAfterSeconds()
	message := "The proxy this account's upstream traffic leaves through is unavailable (" + egressErr.Fault.String() + ")."
	if egressErr.Refused {
		message += " The request was not sent."
	}
	detail := map[string]any{
		"message": message,
		"type":    "server_error",
		"code":    coreauth.EgressUnavailableCode,
	}
	if seconds > 0 {
		detail["message"] = message + " Retry in " + strconv.Itoa(seconds) + " seconds."
		detail["reset_seconds"] = seconds
	}
	body, errMarshal := json.Marshal(map[string]any{"error": detail})
	if errMarshal != nil {
		body = []byte(`{"error":{"message":"The proxy this account's upstream traffic leaves through is unavailable.","type":"server_error","code":"` + coreauth.EgressUnavailableCode + `"}}`)
	}
	return &StreamErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      &egressUnavailableError{body: string(body), retryAfter: seconds},
	}
}

// localRetryAfter returns the Retry-After this server decided on for msg, ""
// when it has none. Unlike the headers in msg.Addon, which come from the
// upstream and are only forwarded when passthrough-headers is on, this one is
// the server's own and is always sent.
func localRetryAfter(msg *StreamErrorMessage) string {
	if msg == nil || msg.Error == nil {
		return ""
	}
	var egressErr *egressUnavailableError
	if !errors.As(msg.Error, &egressErr) || egressErr == nil || egressErr.retryAfter <= 0 {
		return ""
	}
	return strconv.Itoa(egressErr.retryAfter)
}
