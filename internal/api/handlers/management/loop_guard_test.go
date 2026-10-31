package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loopguard"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

const loopGuardTenantB = "tenant-b"

// newLoopGuardTestDB opens a database with two keys of the system tenant and one
// of another tenant, the ids the tests refer to.
func newLoopGuardTestDB(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	for tenantID, rows := range map[string][]usage.APIKeyRow{
		identity.SystemTenantID: {{ID: "key-1", Key: "sk-loop-guard-1", Name: "One"}, {ID: "key-2", Key: "sk-loop-guard-2", Name: "Two"}},
		loopGuardTenantB:        {{ID: "key-b", Key: "sk-loop-guard-b", Name: "Other"}},
	} {
		for _, row := range rows {
			if err := usage.UpsertAPIKeyForTenant(tenantID, row); err != nil {
				t.Fatalf("upsert key %s: %v", row.ID, err)
			}
		}
	}
}

// loopGuardHandler returns a handler over a registry backed by the real store,
// so a write goes through the compare-and-swap of the runtime settings table.
func loopGuardHandler(t *testing.T) *Handler {
	t.Helper()
	newLoopGuardTestDB(t)
	registry := loopguard.New()
	registry.SetStore(settingsstore.LoopGuardSettings{})
	return &Handler{loopGuard: registry}
}

type loopGuardCall struct {
	handler   gin.HandlerFunc
	method    string
	keyID     string
	body      string
	principal identity.Principal
}

func (call loopGuardCall) do() *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, call.principal)
	c.Request = httptest.NewRequest(call.method, "/v0/management/loop-guard", strings.NewReader(call.body))
	c.Request.Header.Set("Content-Type", "application/json")
	if call.keyID != "" {
		c.Params = gin.Params{{Key: "id", Value: call.keyID}}
	}
	call.handler(c)
	return rec
}

var lgSystem = tenantPrincipal(identity.SystemTenantID)

func (h *Handler) getTenant(t *testing.T, principal identity.Principal) map[string]any {
	t.Helper()
	rec := loopGuardCall{handler: h.GetLoopGuard, method: http.MethodGet, principal: principal}.do()
	return decodeLoopGuard(t, rec, http.StatusOK)
}

func decodeLoopGuard(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) map[string]any {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, wantStatus, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, rec.Body.String())
	}
	return out
}

// lgAt walks a decoded response by key.
func lgAt(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%v is not an object at %q", v, key)
		}
		v, ok = m[key]
		if !ok {
			t.Fatalf("no %q (have %v)", key, m)
		}
	}
	return v
}

func lgErrorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	code, _ := lgAt(t, body, "error", "code").(string)
	return code
}

func TestGetLoopGuardWithNothingStoredIsEverythingOff(t *testing.T) {
	h := loopGuardHandler(t)
	got := h.getTenant(t, lgSystem)

	for _, path := range [][]string{{"reply", "enabled"}, {"tool", "enabled"}} {
		if lgAt(t, got, path...) != false {
			t.Fatalf("%v = %v, want false: nothing is on until a tenant turns it on", path, lgAt(t, got, path...))
		}
	}
	if lgAt(t, got, "persistent") != true || lgAt(t, got, "key_overrides") != float64(0) {
		t.Fatalf("persistent / overrides = %v / %v", got["persistent"], got["key_overrides"])
	}
	// What the panel builds its inputs from.
	if lgAt(t, got, "defaults", "reply_window") != float64(256) || lgAt(t, got, "defaults", "reply_max_distinct") != float64(24) || lgAt(t, got, "defaults", "tool_repeats") != float64(8) {
		t.Fatalf("defaults = %v", got["defaults"])
	}
	if lgAt(t, got, "bounds", "min_reply_window") != float64(64) || lgAt(t, got, "bounds", "max_reply_window") != float64(2048) ||
		lgAt(t, got, "bounds", "reply_distinct_window_share") != float64(4) || lgAt(t, got, "bounds", "min_tool_repeats") != float64(3) ||
		lgAt(t, got, "bounds", "max_tool_repeats") != float64(32) || lgAt(t, got, "bounds", "min_reply_distinct") != float64(2) {
		t.Fatalf("bounds = %v", got["bounds"])
	}
	if _, has := got["updated_at"]; has {
		t.Fatalf("updated_at is set for settings nobody saved: %v", got["updated_at"])
	}
}

func TestPutLoopGuardSavesAndAReadReturnsIt(t *testing.T) {
	h := loopGuardHandler(t)

	put := func(body string) map[string]any {
		t.Helper()
		return decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuard, method: http.MethodPut, body: body, principal: lgSystem}.do(), http.StatusOK)
	}

	saved := put(`{"reply":{"enabled":true,"window":512,"max_distinct":40},"tool":{"enabled":true,"repeats":5}}`)
	if lgAt(t, saved, "reply", "enabled") != true || lgAt(t, saved, "reply", "window") != float64(512) || lgAt(t, saved, "reply", "max_distinct") != float64(40) ||
		lgAt(t, saved, "tool", "enabled") != true || lgAt(t, saved, "tool", "repeats") != float64(5) {
		t.Fatalf("saved = %v", saved)
	}
	if _, has := saved["updated_at"]; !has {
		t.Fatalf("a saved setting carries no updated_at: %v", saved)
	}
	if read := h.getTenant(t, lgSystem); !lgJSONEqual(t, read, saved) {
		t.Fatalf("read after save = %v, want %v", read, saved)
	}

	// A section that is absent is left as it is.
	afterTool := put(`{"tool":{"enabled":false}}`)
	if lgAt(t, afterTool, "tool", "enabled") != false || lgAt(t, afterTool, "tool", "repeats") != float64(0) {
		t.Fatalf("tool section was not replaced: %v", afterTool["tool"])
	}
	if lgAt(t, afterTool, "reply", "enabled") != true || lgAt(t, afterTool, "reply", "window") != float64(512) {
		t.Fatalf("a body without a reply section changed the reply section: %v", afterTool["reply"])
	}

	// A section that is present replaces the stored one: a threshold it does not
	// name goes back to the default.
	afterReply := put(`{"reply":{"enabled":true}}`)
	if lgAt(t, afterReply, "reply", "window") != float64(0) || lgAt(t, afterReply, "reply", "max_distinct") != float64(0) {
		t.Fatalf("reply thresholds survived a replacement that did not name them: %v", afterReply["reply"])
	}

	// An empty body changes nothing and is not an error.
	if unchanged := put(`{}`); !lgJSONEqual(t, unchanged, afterReply) {
		t.Fatalf("an empty body changed the settings: %v -> %v", afterReply, unchanged)
	}
}

func lgJSONEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestPutLoopGuardRefusesWhatItCannotEnforce(t *testing.T) {
	for name, body := range map[string]string{
		"window below the bound":              `{"reply":{"enabled":true,"window":10}}`,
		"window above the bound":              `{"reply":{"enabled":true,"window":5000}}`,
		"negative window":                     `{"reply":{"window":-1}}`,
		"distinct below the bound":            `{"reply":{"enabled":true,"max_distinct":1}}`,
		"distinct wider than the window lets": `{"reply":{"enabled":true,"window":64,"max_distinct":17}}`,
		"distinct wider than the default":     `{"reply":{"enabled":true,"max_distinct":65}}`,
		"repeats below the bound":             `{"tool":{"enabled":true,"repeats":2}}`,
		"repeats above the bound":             `{"tool":{"enabled":true,"repeats":33}}`,
		"not json":                            `{"reply":`,
		"wrong type":                          `{"reply":{"enabled":"yes"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := loopGuardHandler(t)
			before := h.getTenant(t, lgSystem)
			got := decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuard, method: http.MethodPut, body: body, principal: lgSystem}.do(), http.StatusBadRequest)
			if lgErrorCode(t, got) != "invalid_request" {
				t.Fatalf("error = %v", got)
			}
			if after := h.getTenant(t, lgSystem); !lgJSONEqual(t, before, after) {
				t.Fatalf("a refused write changed the settings: %v -> %v", before, after)
			}
		})
	}

	// The edges are accepted.
	h := loopGuardHandler(t)
	for _, body := range []string{
		`{"reply":{"enabled":true,"window":64,"max_distinct":16}}`,
		`{"reply":{"enabled":true,"window":2048,"max_distinct":512}}`,
		`{"reply":{"enabled":true,"max_distinct":64}}`,
		`{"tool":{"enabled":true,"repeats":3}}`,
		`{"tool":{"enabled":true,"repeats":32}}`,
	} {
		decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuard, method: http.MethodPut, body: body, principal: lgSystem}.do(), http.StatusOK)
	}
}

func TestLoopGuardSettingsBelongToTheTenantTheSessionActsIn(t *testing.T) {
	h := loopGuardHandler(t)
	tenantB := tenantPrincipal(loopGuardTenantB)

	decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuard, method: http.MethodPut, body: `{"reply":{"enabled":true}}`, principal: tenantB}.do(), http.StatusOK)

	if h.getTenant(t, tenantB)["reply"].(map[string]any)["enabled"] != true {
		t.Fatal("the tenant's own setting was not saved")
	}
	if h.getTenant(t, lgSystem)["reply"].(map[string]any)["enabled"] != false {
		t.Fatal("a setting saved for one tenant switched loop protection on for another")
	}
}

func TestLoopGuardKeyOverride(t *testing.T) {
	h := loopGuardHandler(t)
	putTenant := func(body string) {
		t.Helper()
		decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuard, method: http.MethodPut, body: body, principal: lgSystem}.do(), http.StatusOK)
	}
	putKey := func(id, body string) map[string]any {
		t.Helper()
		return decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuardKey, method: http.MethodPut, keyID: id, body: body, principal: lgSystem}.do(), http.StatusOK)
	}
	getKey := func(id string) map[string]any {
		t.Helper()
		return decodeLoopGuard(t, loopGuardCall{handler: h.GetLoopGuardKey, method: http.MethodGet, keyID: id, principal: lgSystem}.do(), http.StatusOK)
	}

	putTenant(`{"reply":{"enabled":true,"window":512},"tool":{"enabled":false}}`)

	// A key that sets nothing follows the tenant, and says so.
	follow := getKey("key-1")
	if lgAt(t, follow, "reply", "enabled") != nil || lgAt(t, follow, "tool", "enabled") != nil {
		t.Fatalf("a key with no override has switches set: %v", follow)
	}
	if lgAt(t, follow, "effective", "reply", "enabled") != true || lgAt(t, follow, "effective", "reply", "source") != "tenant" ||
		lgAt(t, follow, "effective", "reply", "window") != float64(512) || lgAt(t, follow, "effective", "reply", "max_distinct") != float64(24) {
		t.Fatalf("effective reply = %v", lgAt(t, follow, "effective", "reply"))
	}
	if lgAt(t, follow, "effective", "tool", "enabled") != false {
		t.Fatalf("effective tool = %v", lgAt(t, follow, "effective", "tool"))
	}
	if lgAt(t, follow, "tenant", "reply", "enabled") != true || lgAt(t, follow, "tenant", "reply", "window") != float64(512) {
		t.Fatalf("tenant view = %v", follow["tenant"])
	}

	// A key exempts itself from the reply check and turns the tool check on with
	// a threshold of its own.
	saved := putKey("key-1", `{"reply":{"enabled":false},"tool":{"enabled":true,"repeats":4}}`)
	if lgAt(t, saved, "reply", "enabled") != false || lgAt(t, saved, "tool", "enabled") != true || lgAt(t, saved, "tool", "repeats") != float64(4) {
		t.Fatalf("saved override = %v", saved)
	}
	if lgAt(t, saved, "effective", "reply", "enabled") != false {
		t.Fatalf("an exempted key is still checked: %v", lgAt(t, saved, "effective", "reply"))
	}
	if lgAt(t, saved, "effective", "tool", "enabled") != true || lgAt(t, saved, "effective", "tool", "source") != "api_key" || lgAt(t, saved, "effective", "tool", "repeats") != float64(4) {
		t.Fatalf("effective tool = %v", lgAt(t, saved, "effective", "tool"))
	}
	if h.getTenant(t, lgSystem)["key_overrides"] != float64(1) {
		t.Fatalf("tenant view does not count the override: %v", h.getTenant(t, lgSystem))
	}
	// The other key and the tenant are untouched.
	if other := getKey("key-2"); lgAt(t, other, "effective", "reply", "enabled") != true || lgAt(t, other, "effective", "tool", "enabled") != false {
		t.Fatalf("another key's effective settings changed: %v", other["effective"])
	}
	if tenant := h.getTenant(t, lgSystem); lgAt(t, tenant, "reply", "enabled") != true {
		t.Fatalf("a key's override changed the tenant's settings: %v", tenant)
	}

	// A key whose override ends up setting nothing has none stored.
	cleared := putKey("key-1", `{"reply":{},"tool":{}}`)
	if lgAt(t, cleared, "reply", "enabled") != nil || lgAt(t, cleared, "tool", "enabled") != nil || lgAt(t, cleared, "tool", "repeats") != float64(0) {
		t.Fatalf("cleared override = %v", cleared)
	}
	if h.getTenant(t, lgSystem)["key_overrides"] != float64(0) {
		t.Fatalf("a cleared override is still counted: %v", h.getTenant(t, lgSystem))
	}

	// A tenant save keeps the overrides of the keys it has.
	putKey("key-2", `{"tool":{"enabled":true}}`)
	putTenant(`{"reply":{"enabled":false}}`)
	if lgAt(t, getKey("key-2"), "effective", "tool", "enabled") != true {
		t.Fatal("a tenant save dropped the override of a key that still exists")
	}
}

func TestLoopGuardKeyRoutesOnlyKnowKeysOfTheTenant(t *testing.T) {
	h := loopGuardHandler(t)
	for name, tc := range map[string]struct {
		id        string
		principal identity.Principal
	}{
		"unknown id":                        {"key-nope", lgSystem},
		"another tenant's key":              {"key-b", lgSystem},
		"the system tenant's key seen as b": {"key-1", tenantPrincipal(loopGuardTenantB)},
		"empty id":                          {"", lgSystem},
	} {
		for _, call := range []loopGuardCall{
			{handler: h.GetLoopGuardKey, method: http.MethodGet},
			{handler: h.PutLoopGuardKey, method: http.MethodPut, body: `{"reply":{"enabled":true}}`},
		} {
			t.Run(name+" "+call.method, func(t *testing.T) {
				call.keyID, call.principal = tc.id, tc.principal
				got := decodeLoopGuard(t, call.do(), http.StatusNotFound)
				if lgErrorCode(t, got) != "api_key_not_found" {
					t.Fatalf("error = %v", got)
				}
			})
		}
	}
	// Nothing was written for any of them.
	if h.getTenant(t, lgSystem)["key_overrides"] != float64(0) || h.getTenant(t, tenantPrincipal(loopGuardTenantB))["key_overrides"] != float64(0) {
		t.Fatal("a refused key write left an override behind")
	}
}

func TestPutLoopGuardKeyRefusesWhatItCannotEnforce(t *testing.T) {
	h := loopGuardHandler(t)
	for name, body := range map[string]string{
		"window":   `{"reply":{"window":3}}`,
		"distinct": `{"reply":{"window":64,"max_distinct":40}}`,
		"repeats":  `{"tool":{"repeats":100}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuardKey, method: http.MethodPut, keyID: "key-1", body: body, principal: lgSystem}.do(), http.StatusBadRequest)
			if lgErrorCode(t, got) != "invalid_request" {
				t.Fatalf("error = %v", got)
			}
		})
	}
}

// Deleting a key does not know about its override, so a tenant save removes the
// ones whose key is gone.
func TestPutLoopGuardDropsTheOverridesOfDeletedKeys(t *testing.T) {
	h := loopGuardHandler(t)
	for _, id := range []string{"key-1", "key-2"} {
		decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuardKey, method: http.MethodPut, keyID: id, body: `{"tool":{"enabled":true}}`, principal: lgSystem}.do(), http.StatusOK)
	}
	if h.getTenant(t, lgSystem)["key_overrides"] != float64(2) {
		t.Fatal("setup: two overrides expected")
	}
	if err := usage.DeleteAPIKeyByIDForTenant(identity.SystemTenantID, "key-2"); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	got := decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuard, method: http.MethodPut, body: `{}`, principal: lgSystem}.do(), http.StatusOK)
	if got["key_overrides"] != float64(1) {
		t.Fatalf("overrides after the save = %v, want only the live key's", got["key_overrides"])
	}
	if live := decodeLoopGuard(t, loopGuardCall{handler: h.GetLoopGuardKey, method: http.MethodGet, keyID: "key-1", principal: lgSystem}.do(), http.StatusOK); lgAt(t, live, "effective", "tool", "enabled") != true {
		t.Fatal("the live key lost its override")
	}
}

// A tenant that lists no keys is indistinguishable from one whose key list could
// not be read, and dropping every override on a failed read is the worse error.
func TestPutLoopGuardKeepsOverridesWhenNoKeysAreListed(t *testing.T) {
	h := loopGuardHandler(t)
	decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuardKey, method: http.MethodPut, keyID: "key-1", body: `{"tool":{"enabled":true}}`, principal: lgSystem}.do(), http.StatusOK)
	for _, id := range []string{"key-1", "key-2"} {
		if err := usage.DeleteAPIKeyByIDForTenant(identity.SystemTenantID, id); err != nil {
			t.Fatalf("delete key: %v", err)
		}
	}
	got := decodeLoopGuard(t, loopGuardCall{handler: h.PutLoopGuard, method: http.MethodPut, body: `{}`, principal: lgSystem}.do(), http.StatusOK)
	if got["key_overrides"] != float64(1) {
		t.Fatalf("overrides = %v", got["key_overrides"])
	}
}

func TestLoopGuardEndpointsWithoutAStore(t *testing.T) {
	newLoopGuardTestDB(t)
	h := &Handler{loopGuard: loopguard.New()}

	got := h.getTenant(t, lgSystem)
	if got["persistent"] != false || lgAt(t, got, "reply", "enabled") != false {
		t.Fatalf("read without a store = %v", got)
	}
	for name, call := range map[string]loopGuardCall{
		"tenant": {handler: h.PutLoopGuard, method: http.MethodPut, body: `{"reply":{"enabled":true}}`},
		"key":    {handler: h.PutLoopGuardKey, method: http.MethodPut, keyID: "key-1", body: `{"reply":{"enabled":true}}`},
	} {
		t.Run(name, func(t *testing.T) {
			call.principal = lgSystem
			body := decodeLoopGuard(t, call.do(), http.StatusServiceUnavailable)
			if lgErrorCode(t, body) != "loop_guard_not_persistent" {
				t.Fatalf("error = %v", body)
			}
		})
	}
}

// lgFailingStore reads an empty row and refuses every write with err.
type lgFailingStore struct{ err error }

func (lgFailingStore) Load(context.Context, string) (loopguard.Stored, error) {
	return loopguard.Stored{}, nil
}
func (s lgFailingStore) Save(context.Context, string, loopguard.Settings, int64) error { return s.err }

func TestLoopGuardWriteFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		wantStatus int
		wantCode   string
		hidden     string
	}{
		"someone else keeps changing it": {loopguard.ErrConflict, http.StatusConflict, "loop_guard_conflict", ""},
		"the database fails":             {errors.New("pq: password authentication failed for user cliproxy"), http.StatusInternalServerError, "loop_guard_failed", "password"},
	} {
		t.Run(name, func(t *testing.T) {
			newLoopGuardTestDB(t)
			registry := loopguard.New()
			registry.SetStore(lgFailingStore{err: tc.err})
			h := &Handler{loopGuard: registry}

			rec := loopGuardCall{handler: h.PutLoopGuard, method: http.MethodPut, body: `{"reply":{"enabled":true}}`, principal: lgSystem}.do()
			body := decodeLoopGuard(t, rec, tc.wantStatus)
			if lgErrorCode(t, body) != tc.wantCode {
				t.Fatalf("error = %v", body)
			}
			if tc.hidden != "" && strings.Contains(rec.Body.String(), tc.hidden) {
				t.Fatalf("the response leaks the database error: %s", rec.Body.String())
			}
		})
	}
}

// A handler built without a registry of its own uses the process-wide one the
// inference path reads, which is what makes a saved setting take effect.
func TestLoopGuardHandlerUsesTheProcessRegistryByDefault(t *testing.T) {
	if (&Handler{}).loopGuardRegistry() != loopguard.Default() {
		t.Fatal("a handler without an override does not use the registry the gateway resolves settings from")
	}
	var h *Handler
	if h.loopGuardRegistry() != loopguard.Default() {
		t.Fatal("a nil handler does not fall back to the process registry")
	}
}
