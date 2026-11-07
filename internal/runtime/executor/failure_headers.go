package executor

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/retryhint"
)

// failureHeaders is what an executor attaches to the error of a failed upstream
// call for the scheduler's sake: the headers that carry a retry wait
// (Retry-After, the rate-limit reset family) and the markers a proxy puts on its
// own error pages, and nothing else.
//
// The scheduler reads a wait out of every failed result, so an executor that
// attaches no headers loses a hint the upstream sent in plain sight. The error
// may also be forwarded to the client when passthrough-headers is on, which is
// why only those headers are kept: cookies and the upstream's own framing
// headers have no business in that response.
func failureHeaders(src http.Header) http.Header {
	return retryhint.FailureHeaders(src)
}
