package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestParseHTTPRetryAfterHeaderDurationBounds(t *testing.T) {
	for _, tt := range []struct {
		header string
		valid  bool
	}{
		{"9223372036", true},
		{"9223372037", false},
		{"18446744074", false},
		{"9223372036854775807", false},
	} {
		t.Run(tt.header, func(t *testing.T) {
			got := parseHTTPRetryAfterHeader(http.Header{"Retry-After": {tt.header}}, time.Now())
			if !tt.valid {
				if got != nil {
					t.Fatalf("unrepresentable delay = %s, want nil", *got)
				}
				return
			}
			if got == nil || *got != 9223372036*time.Second {
				t.Fatalf("largest representable seconds not preserved: %v", got)
			}
		})
	}
}
