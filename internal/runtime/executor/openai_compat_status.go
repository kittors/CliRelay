package executor

import (
	"net/http"
	"time"
)

func newOpenAICompatStatusErr(code int, body []byte, headers http.Header) statusErr {
	err := statusErr{code: code, msg: string(body), headers: headers.Clone()}
	// The scheduler consumes RetryAfter(), not the forwarded response headers.
	if code == http.StatusTooManyRequests {
		err.retryAfter = parseHTTPRetryAfterHeader(headers, time.Now())
	}
	return err
}
