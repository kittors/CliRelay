package logging

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterIncludesModerationFields(t *testing.T) {
	entry := &log.Entry{
		Logger:  log.New(),
		Time:    time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		Level:   log.WarnLevel,
		Message: "content moderation failed open",
		Data: log.Fields{
			"error_class":       "timeout",
			"tenant_id":         "tenant-requester",
			"policy_tenant_id":  "tenant-owner",
			"profile_id":        "profile-1",
			"profile_version":   3,
			"resolution_source": "provider_key",
			"channel_type":      "provider_key",
			"channel_id":        "key-1",
			"action":            "api_error",
			"latency_ms":        3000,
			"cache_hit":         false,
			"category":          "violence",
			"score":             0.91,
		},
	}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	output := string(formatted)
	for _, field := range []string{
		"error_class=timeout",
		"tenant_id=tenant-requester",
		"policy_tenant_id=tenant-owner",
		"profile_id=profile-1",
		"profile_version=3",
		"resolution_source=provider_key",
		"channel_type=provider_key",
		"channel_id=key-1",
		"action=api_error",
		"latency_ms=3000",
		"cache_hit=false",
		"category=violence",
		"score=0.91",
	} {
		if !strings.Contains(output, field) {
			t.Fatalf("formatted log missing %q: %s", field, output)
		}
	}
}
