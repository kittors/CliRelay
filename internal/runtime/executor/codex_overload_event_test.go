package executor

import (
	"net/http"
	"testing"
	"time"
)

// ChatGPT reports a busy fleet inside the SSE stream: the HTTP status is 200 and
// the event carries its own headers object with the client's retry-after. Left
// as a 502 the client could neither back off nor tell capacity from a fault.
func TestCodexResponsesFailedStatusErrClassifiesFleetOverload(t *testing.T) {
	payload := []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","headers":{"retry-after":"373","retry-after-ms":"373000"},"message":"Our servers are currently overloaded. Please try again later."},"sequence_number":3}`)

	err := codexResponsesFailedStatusErr(payload)
	if err.code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", err.code)
	}
	if err.retryAfter == nil {
		t.Fatalf("the event's retry-after was dropped")
	}
	if got := *err.retryAfter; got != 373*time.Second {
		t.Fatalf("retryAfter = %v, want 373s", got)
	}
}

func TestCodexResponsesFailedStatusErrReadsMilliseconds(t *testing.T) {
	payload := []byte(`{"type":"error","error":{"code":"server_is_overloaded","headers":{"retry-after-ms":"1500"},"message":"Our servers are currently overloaded."}}`)
	err := codexResponsesFailedStatusErr(payload)
	if err.retryAfter == nil || *err.retryAfter != 1500*time.Millisecond {
		t.Fatalf("retryAfter = %v, want 1500ms", err.retryAfter)
	}
}

// A capacity rejection with no headers keeps the old shape.
func TestCodexResponsesFailedStatusErrOverloadWithoutHeaders(t *testing.T) {
	payload := []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`)
	err := codexResponsesFailedStatusErr(payload)
	if err.code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", err.code)
	}
	if err.retryAfter != nil {
		t.Fatalf("invented a wait: %v", *err.retryAfter)
	}
}

// An ordinary upstream fault must keep its 502 so the existing failover and
// cooldown rules for it are untouched.
func TestCodexResponsesFailedStatusErrKeepsOrdinaryFaultAsBadGateway(t *testing.T) {
	payload := []byte(`{"type":"error","error":{"type":"upstream_error","code":"internal_error","message":"something broke"}}`)
	err := codexResponsesFailedStatusErr(payload)
	if err.code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", err.code)
	}
}
