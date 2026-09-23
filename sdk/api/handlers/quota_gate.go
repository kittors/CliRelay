package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// QuotaGate admits one billable turn of a long-lived request.
//
// The quota middleware checks a POST once, before its handler runs. A Responses
// WebSocket turn is billed like that POST — it reaches the executor and spends
// upstream quota — but it arrives inside a frame, after the middleware has run
// on the upgrade request. The middleware therefore publishes a gate on the gin
// context, and frame-driven handlers call it before each turn reaches the
// executor.
//
// On admission the gate returns a release func, which the caller must call once
// the turn has ended, however it ended: it gives back the concurrency slot the
// turn holds. On refusal it returns what to report instead of running the turn.
type QuotaGate func() (release func(), rejection *QuotaRejection)

// QuotaGateContextKey is the gin context key the middleware stores a QuotaGate under.
const QuotaGateContextKey = "cliproxy.quotaGate"

// QuotaRejection is a refused turn, carrying the answer the HTTP path gives for
// the same verdict.
type QuotaRejection struct {
	// StatusCode is the HTTP status of the refusal: 429, or 503 when usage
	// could not be read.
	StatusCode int
	// Body is the JSON error body the HTTP path responds with.
	Body []byte
	// Headers are the X-CliRelay-Quota-* diagnostics the HTTP path sets.
	Headers http.Header
}

// Error returns the JSON body, so renderers that pass a JSON error text through
// unchanged (BuildErrorResponseBody) report the HTTP path's code and type.
func (r *QuotaRejection) Error() string {
	if r == nil {
		return ""
	}
	return string(r.Body)
}

// QuotaGateFromGin returns the gate published for this request, or nil when no
// quota applies (the middleware did not run, or the key has no limits).
func QuotaGateFromGin(c *gin.Context) QuotaGate {
	if c == nil {
		return nil
	}
	value, ok := c.Get(QuotaGateContextKey)
	if !ok {
		return nil
	}
	gate, _ := value.(QuotaGate)
	return gate
}
