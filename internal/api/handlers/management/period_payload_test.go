package management

import (
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quota"
)

func TestValidatePeriodPayloadJSONDetectsNormalizedDayConflict(t *testing.T) {
	if err := validatePeriodPayloadJSON([]byte(`{"items":[{"daily-spending-limit":10.1,"period-spending-limits":{"day":11}}]}`)); err != nil {
		t.Fatalf("same normalized day: %v", err)
	}
	if err := validatePeriodPayloadJSON([]byte(`{"items":[{"daily-spending-limit":10,"period-spending-limits":{"day":11}}]}`)); !errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	if err := validatePeriodPayloadJSON([]byte(`{"period-spending-limits":{"week":-1}}`)); !errors.Is(err, quota.ErrInvalidSpendingLimit) {
		t.Fatalf("negative err = %v", err)
	}
}
