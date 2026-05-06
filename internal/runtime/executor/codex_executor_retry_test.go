package executor

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseCodexRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run("resets_in_seconds", func(t *testing.T) {
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":123}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 123*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 123*time.Second)
		}
	})

	t.Run("prefers resets_at", func(t *testing.T) {
		resetAt := now.Add(5 * time.Minute).Unix()
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":1}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 5*time.Minute {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 5*time.Minute)
		}
	})

	t.Run("fallback when resets_at is past", func(t *testing.T) {
		resetAt := now.Add(-1 * time.Minute).Unix()
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":77}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 77*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 77*time.Second)
		}
	})

	t.Run("non-429 status code", func(t *testing.T) {
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":30}}`)
		if got := parseCodexRetryAfter(http.StatusBadRequest, body, now); got != nil {
			t.Fatalf("expected nil for non-429, got %v", *got)
		}
	})

	t.Run("non usage_limit_reached error type", func(t *testing.T) {
		body := []byte(`{"error":{"type":"server_error","resets_in_seconds":30}}`)
		if got := parseCodexRetryAfter(http.StatusTooManyRequests, body, now); got != nil {
			t.Fatalf("expected nil for non-usage_limit_reached, got %v", *got)
		}
	})
}

func TestParseCodexQuotaProbe(t *testing.T) {
	t.Run("does not recover when primary window is exhausted", func(t *testing.T) {
		primaryResetAt := time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC).Unix()
		secondaryResetAt := time.Date(2030, 1, 1, 0, 15, 0, 0, time.UTC).Unix()
		body := []byte(`{"rate_limit":{"primary_window":{"used_percent":100,"reset_at":` + itoa(primaryResetAt) + `},"secondary_window":{"used_percent":70,"reset_at":` + itoa(secondaryResetAt) + `}}}`)

		got := parseCodexQuotaProbe(body)
		if got == nil {
			t.Fatal("expected quota probe result, got nil")
		}
		if got.Recovered {
			t.Fatal("Recovered = true, want false while primary window is exhausted")
		}
		wantRecoverAt := time.Unix(primaryResetAt, 0)
		if !got.NextRecoverAt.Equal(wantRecoverAt) {
			t.Fatalf("NextRecoverAt = %v, want %v", got.NextRecoverAt, wantRecoverAt)
		}
	})

	t.Run("does not recover when explicit limit reached", func(t *testing.T) {
		resetAt := time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC).Unix()
		body := []byte(`{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":80,"reset_at":` + itoa(resetAt) + `},"secondary_window":{"used_percent":70,"reset_at":` + itoa(resetAt) + `}}}`)

		got := parseCodexQuotaProbe(body)
		if got == nil {
			t.Fatal("expected quota probe result, got nil")
		}
		if got.Recovered {
			t.Fatal("Recovered = true, want false while limit_reached is true")
		}
		if !got.NextRecoverAt.Equal(time.Unix(resetAt, 0)) {
			t.Fatalf("NextRecoverAt = %v, want %v", got.NextRecoverAt, time.Unix(resetAt, 0))
		}
	})

	t.Run("recovers when all windows have capacity", func(t *testing.T) {
		body := []byte(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":20},"secondary_window":{"used_percent":80}}}`)
		result := parseCodexQuotaProbe(body)
		if result == nil {
			t.Fatalf("expected quota probe result")
		}
		if !result.Recovered {
			t.Fatalf("expected recovered result when all quota windows have capacity")
		}
	})

	t.Run("recognizes explicit weekly window", func(t *testing.T) {
		body := []byte(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":20},"weekly_window":{"used_percent":100,"reset_after_seconds":3600}}}`)
		result := parseCodexQuotaProbe(body)
		if result == nil {
			t.Fatalf("expected quota probe result")
		}
		if result.Recovered {
			t.Fatalf("expected blocked result when weekly_window is exhausted")
		}
		if result.NextRecoverAt.IsZero() {
			t.Fatalf("expected NextRecoverAt from weekly_window reset_after_seconds")
		}
	})
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
