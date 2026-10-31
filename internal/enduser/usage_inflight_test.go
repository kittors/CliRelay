package enduser

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
)

func TestKeyUsageViewShowsWhatTheKeyHasInFlight(t *testing.T) {
	const keyID = "key-usage-view-in-flight"
	r := inflight.Default().Reserve("", inflight.Owner{Key: inflight.KeyName(keyID)})
	view := keyUsageView(keyID, quota.UsageSnapshot{}, nil)
	if view.InFlight == nil || view.InFlight.Requests != 1 {
		t.Fatalf("in flight = %+v, want the running request", view.InFlight)
	}
	raw, _ := json.Marshal(view)
	if !strings.Contains(string(raw), `"in-flight":{"requests":1,"tokens":0,"cost":0}`) {
		t.Fatalf("view = %s", raw)
	}

	r.Finish()
	view = keyUsageView(keyID, quota.UsageSnapshot{}, nil)
	raw, _ = json.Marshal(view)
	if view.InFlight != nil || strings.Contains(string(raw), "in-flight") {
		t.Fatalf("view after the request ended = %s, want no in-flight entry", raw)
	}
}
