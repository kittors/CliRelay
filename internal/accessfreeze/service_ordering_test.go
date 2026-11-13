package accessfreeze

import (
	"context"
	"errors"
	"testing"
)

func TestServiceReleaseCanceledAndFailedCallsLeaveGateReusable(t *testing.T) {
	service := NewService(NewStore(nil), testZone)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Release(ctx, "tenant", 1, "admin", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled release = %v", err)
	}
	for range 2 {
		if _, err := service.Release(context.Background(), "tenant", 1, "admin", ""); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("failed release = %v", err)
		}
	}
	// A failed refresh must release the same gate too.
	service.refresh(context.Background())
	if _, err := service.Release(context.Background(), "tenant", 1, "admin", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("release after failed refresh = %v", err)
	}
	if got := service.FrozenProviders("tenant", "user", "key"); got != "" {
		t.Fatalf("failed transitions changed cache: %s", got)
	}
}
