package authfiles

import (
	"context"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestClearQuotaStatusRemovesHTTPOnlyUnauthorizedRestriction(t *testing.T) {
	now := time.Date(2026, 7, 27, 5, 19, 8, 0, time.UTC)
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:        "xai-http-only-401",
		Provider:  "xai",
		Status:    coreauth.StatusError,
		LastError: &coreauth.Error{HTTPStatus: http.StatusUnauthorized},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	before, ok := manager.GetByID(auth.ID)
	if !ok || before == nil {
		t.Fatal("GetByID() missing auth before clear")
	}
	restrictionsBefore := BuildRestrictionPayload(before, now)
	t.Logf("restrictions before clear: %#v", restrictionsBefore)
	if len(restrictionsBefore) != 1 {
		t.Fatalf("restrictions before clear = %#v, want one 401 restriction", restrictionsBefore)
	}
	if got := restrictionsBefore[0]; got["http_status"] != http.StatusUnauthorized || got["scope"] != "auth" {
		t.Fatalf("restriction before clear = %#v, want auth HTTP 401", got)
	} else if _, ok := got["status_message"]; ok {
		t.Fatalf("restriction before clear unexpectedly has status_message: %#v", got)
	}

	changed, err := manager.ClearQuotaStatus(context.Background(), auth.ID)
	if err != nil {
		t.Fatalf("ClearQuotaStatus() error = %v", err)
	}
	if !changed {
		t.Fatal("ClearQuotaStatus() changed = false, want true")
	}
	after, ok := manager.GetByID(auth.ID)
	if !ok || after == nil {
		t.Fatal("GetByID() missing auth after clear")
	}
	restrictionsAfter := BuildRestrictionPayload(after, now)
	t.Logf("restrictions after clear: %#v", restrictionsAfter)
	if len(restrictionsAfter) != 0 {
		t.Fatalf("restrictions after clear = %#v, want empty", restrictionsAfter)
	}
}

func TestBuildRestrictionPayloadIncludesModelRestriction(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)
	nextRetry := now.Add(30 * time.Minute)
	auth := &coreauth.Auth{
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5": {
				Status:         coreauth.StatusError,
				StatusMessage:  "unauthorized",
				Unavailable:    true,
				NextRetryAfter: nextRetry,
				LastError:      &coreauth.Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
			},
		},
	}

	restrictions := BuildRestrictionPayload(auth, now)
	if len(restrictions) != 1 {
		t.Fatalf("restrictions length = %d, want 1", len(restrictions))
	}
	got := restrictions[0]
	if got["scope"] != "model" || got["model"] != "gpt-5" || got["http_status"] != http.StatusUnauthorized {
		t.Fatalf("restriction = %#v, want model gpt-5 401", got)
	}
	if retry, ok := got["next_retry_after"].(time.Time); !ok || !retry.Equal(nextRetry) {
		t.Fatalf("next_retry_after = %#v, want %v", got["next_retry_after"], nextRetry)
	}
}

func TestBuildRestrictionPayloadDedupesRepeatedQuotaErrors(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)
	nextRetry := now.Add(25 * time.Minute)
	nextRecover := now.Add(25 * time.Minute)

	makeModelState := func() *coreauth.ModelState {
		return &coreauth.ModelState{
			Status:         coreauth.StatusError,
			StatusMessage:  `{"error":{"type":"usage_limit_reached"}}`,
			Unavailable:    true,
			NextRetryAfter: nextRetry,
			LastError:      &coreauth.Error{Message: "usage limit reached", HTTPStatus: http.StatusTooManyRequests},
			Quota: coreauth.QuotaState{
				Exceeded:      true,
				Reason:        "quota",
				NextRecoverAt: nextRecover,
			},
		}
	}

	auth := &coreauth.Auth{
		Status:         coreauth.StatusError,
		StatusMessage:  `{"error":{"type":"usage_limit_reached"}}`,
		NextRetryAfter: nextRetry,
		LastError:      &coreauth.Error{Message: "usage limit reached", HTTPStatus: http.StatusTooManyRequests},
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: nextRecover,
			Window:        "5h",
			WindowMinutes: 300,
		},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5.4-mini": makeModelState(),
			"gpt-5.5":      makeModelState(),
		},
	}

	restrictions := BuildRestrictionPayload(auth, now)
	if len(restrictions) != 1 {
		t.Fatalf("restrictions length = %d, want 1", len(restrictions))
	}
	got := restrictions[0]
	if got["scope"] != "auth" || got["http_status"] != http.StatusTooManyRequests {
		t.Fatalf("restriction = %#v, want auth 429", got)
	}
	if got["quota_window"] != "5h" || got["quota_window_minutes"] != 300 {
		t.Fatalf("quota window = %#v/%#v, want 5h/300", got["quota_window"], got["quota_window_minutes"])
	}
	if _, hasModel := got["model"]; hasModel {
		t.Fatalf("restriction model = %#v, want no model field", got["model"])
	}
}

func TestDeduplicateRestrictionEntriesKeepsDistinctReasons(t *testing.T) {
	entries := []map[string]any{
		{"scope": "model", "status": coreauth.StatusError, "http_status": http.StatusTooManyRequests, "reason": "quota-a"},
		{"scope": "model", "status": coreauth.StatusError, "http_status": http.StatusTooManyRequests, "reason": "quota-b"},
	}

	got := DeduplicateRestrictionEntries(entries)
	if len(got) != 2 {
		t.Fatalf("deduped length = %d, want 2", len(got))
	}
}
