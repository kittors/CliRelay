package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// retryAfterError is the optional shape an executor uses to publish the wait it
// parsed from the upstream.
type retryAfterError struct {
	wait time.Duration
}

func (e retryAfterError) Error() string { return "upstream is busy" }

func (e retryAfterError) RetryAfter() *time.Duration { return &e.wait }

// A client that is told to back off retries into the same refusal; the wait the
// upstream named has to reach it.
func TestWriteErrorResponseSendsTheUpstreamsRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	NewBaseAPIHandlers(nil, nil).WriteErrorResponseAs(c, &StreamErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      retryAfterError{wait: 373 * time.Second},
	}, ErrorFormatOpenAI)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "373" {
		t.Fatalf("Retry-After = %q, want 373", got)
	}
}

func TestWriteErrorResponseRoundsUpSubSecondWaits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	NewBaseAPIHandlers(nil, nil).WriteErrorResponseAs(c, &StreamErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      retryAfterError{wait: 200 * time.Millisecond},
	}, ErrorFormatOpenAI)

	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
}

func TestWriteErrorResponseSendsNoWaitWhenTheErrorHasNone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	NewBaseAPIHandlers(nil, nil).WriteErrorResponseAs(c, &StreamErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      errors.New("bad gateway"),
	}, ErrorFormatOpenAI)

	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want none", got)
	}
}
