package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const modelCooldownClearPath = "/v0/management/auth-files/model-cooldown/clear"

// coolingModelHandler builds a handler over one account whose models "m-a" and
// "m-b" are both held back by a 503, and returns the account's index.
func coolingModelHandler(t *testing.T, tenantID string) (*Handler, *coreauth.Manager, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	registered, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-cooling.json",
		FileName: "codex-cooling.json",
		TenantID: tenantID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		// The list shows file-backed accounts from the auth directory and the
		// others only when they are runtime-only, like a credential imported
		// through the API.
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"type": "codex", "email": "cooling@example.com"},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	for _, model := range []string{"m-a", "m-b"} {
		wait := time.Hour
		manager.MarkResult(context.Background(), coreauth.Result{
			AuthID: registered.ID, Provider: "codex", Model: model, RetryAfter: &wait,
			Error: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable, Message: "overloaded"},
		})
	}
	for _, model := range []string{"m-a", "m-b"} {
		if !modelIsCooling(t, manager, registered.ID, model) {
			t.Fatalf("setup: %s is not cooling", model)
		}
	}
	return &Handler{cfg: &config.Config{}, authManager: manager}, manager, registered.Index
}

func modelIsCooling(t *testing.T, manager *coreauth.Manager, authID, model string) bool {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok {
		t.Fatalf("auth %s vanished", authID)
	}
	state := auth.ModelStates[model]
	return state != nil && state.Unavailable && state.NextRetryAfter.After(time.Now())
}

func postModelCooldownClear(h *Handler, tenantID, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	if tenantID != "" {
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
	}
	c.Request = httptest.NewRequest(http.MethodPost, modelCooldownClearPath, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PostAuthFileModelCooldownClear(c)
	return rec
}

func clearResult(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	var body struct {
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body.Status != "ok" {
		t.Fatalf("status field = %q, want ok; body=%s", body.Status, rec.Body.String())
	}
	return body.Changed
}

func TestPostAuthFileModelCooldownClearLiftsOnlyThatModel(t *testing.T) {
	h, manager, index := coolingModelHandler(t, "")
	body := `{"auth_index":"` + index + `","model":"m-a"}`

	rec := postModelCooldownClear(h, "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !clearResult(t, rec) {
		t.Fatalf("changed = false, want true; body=%s", rec.Body.String())
	}
	if modelIsCooling(t, manager, "codex-cooling.json", "m-a") {
		t.Fatal("m-a is still cooling after it was cleared")
	}
	if !modelIsCooling(t, manager, "codex-cooling.json", "m-b") {
		t.Fatal("clearing m-a released m-b as well")
	}

	// Asking again finds nothing to do, which is a success, not an error: the
	// operator's goal (this model is usable) already holds.
	rec = postModelCooldownClear(h, "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if clearResult(t, rec) {
		t.Fatalf("repeat reported a change; body=%s", rec.Body.String())
	}
}

// The panel and older callers spell the key differently; all three must reach
// the same account.
func TestPostAuthFileModelCooldownClearAcceptsEveryIndexSpelling(t *testing.T) {
	for _, key := range []string{"auth_index", "authIndex", "AuthIndex"} {
		t.Run(key, func(t *testing.T) {
			h, manager, index := coolingModelHandler(t, "")
			rec := postModelCooldownClear(h, "", `{"`+key+`":"`+index+`","model":"m-a"}`)
			if rec.Code != http.StatusOK || !clearResult(t, rec) {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			if modelIsCooling(t, manager, "codex-cooling.json", "m-a") {
				t.Fatal("m-a is still cooling")
			}
		})
	}
}

// A model name with surrounding blanks names the same model; states are keyed by
// the trimmed name.
func TestPostAuthFileModelCooldownClearTrimsTheModelName(t *testing.T) {
	h, manager, index := coolingModelHandler(t, "")
	rec := postModelCooldownClear(h, "", `{"auth_index":"`+index+`","model":"  m-a  "}`)
	if rec.Code != http.StatusOK || !clearResult(t, rec) {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if modelIsCooling(t, manager, "codex-cooling.json", "m-a") {
		t.Fatal("m-a is still cooling")
	}
}

// Another tenant's account is indistinguishable from one that does not exist:
// the answer must not confirm the account is there, and nothing may change.
func TestPostAuthFileModelCooldownClearIsTenantScoped(t *testing.T) {
	const owner = "00000000-0000-0000-0000-00000000000a"
	const stranger = "00000000-0000-0000-0000-00000000000b"
	h, manager, index := coolingModelHandler(t, owner)
	body := `{"auth_index":"` + index + `","model":"m-a"}`

	for _, tenant := range []string{stranger, identity.SystemTenantID, ""} {
		rec := postModelCooldownClear(h, tenant, body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("tenant %q: status = %d, want 404; body=%s", tenant, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "not found") {
			t.Fatalf("tenant %q: body = %s, want a not-found message", tenant, rec.Body.String())
		}
		if !modelIsCooling(t, manager, "codex-cooling.json", "m-a") {
			t.Fatalf("tenant %q released another tenant's model", tenant)
		}
	}

	rec := postModelCooldownClear(h, owner, body)
	if rec.Code != http.StatusOK || !clearResult(t, rec) {
		t.Fatalf("owner: status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if modelIsCooling(t, manager, "codex-cooling.json", "m-a") {
		t.Fatal("the owner could not release its own model")
	}
}

func TestPostAuthFileModelCooldownClearRejectsBadRequests(t *testing.T) {
	h, manager, index := coolingModelHandler(t, "")
	cases := []struct {
		name string
		body string
		want int
		text string
	}{
		{"malformed json", `{"auth_index":`, http.StatusBadRequest, "invalid body"},
		{"not an object", `["m-a"]`, http.StatusBadRequest, "invalid body"},
		{"no model", `{"auth_index":"` + index + `"}`, http.StatusBadRequest, "model is required"},
		{"blank model", `{"auth_index":"` + index + `","model":"   "}`, http.StatusBadRequest, "model is required"},
		{"no auth index", `{"model":"m-a"}`, http.StatusNotFound, "auth not found"},
		{"blank auth index", `{"auth_index":"  ","model":"m-a"}`, http.StatusNotFound, "auth not found"},
		{"unknown auth index", `{"auth_index":"0000000000000000","model":"m-a"}`, http.StatusNotFound, "auth not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postModelCooldownClear(h, "", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.text) {
				t.Fatalf("body = %s, want it to say %q", rec.Body.String(), tc.text)
			}
		})
	}
	for _, model := range []string{"m-a", "m-b"} {
		if !modelIsCooling(t, manager, "codex-cooling.json", model) {
			t.Fatalf("a rejected request released %s", model)
		}
	}
}

// A model with no recorded state is not an error either: the panel may offer
// the action for a row another operator has already cleared.
func TestPostAuthFileModelCooldownClearUnknownModelChangesNothing(t *testing.T) {
	h, manager, index := coolingModelHandler(t, "")
	rec := postModelCooldownClear(h, "", `{"auth_index":"`+index+`","model":"never-used"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if clearResult(t, rec) {
		t.Fatalf("reported a change for a model with no state; body=%s", rec.Body.String())
	}
	for _, model := range []string{"m-a", "m-b"} {
		if !modelIsCooling(t, manager, "codex-cooling.json", model) {
			t.Fatalf("%s was released by a request for another model", model)
		}
	}
}

// The postgres route smoke posts `{}` to every route and tolerates a 503 only
// with one of a fixed set of messages (postgresSmokeAllowsStatus). This route has
// no path parameter, so that 503 text is the one thing keeping the smoke green
// when no auth manager is wired; the check comes before the body is read.
func TestPostAuthFileModelCooldownClearWithoutAuthManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, h := range map[string]*Handler{"nil handler": nil, "no manager": {cfg: &config.Config{}}} {
		t.Run(name, func(t *testing.T) {
			rec := postModelCooldownClear(h, "", `{}`)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "auth manager unavailable") {
				t.Fatalf("body = %s, want the message the route smoke allows", rec.Body.String())
			}
		})
	}
}

// With a manager but the smoke's empty body the route answers 400, which the
// smoke accepts (only 404, 405 and 5xx count as a broken route).
func TestPostAuthFileModelCooldownClearEmptyObjectIsABadRequest(t *testing.T) {
	h, _, _ := coolingModelHandler(t, "")
	rec := postModelCooldownClear(h, "", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// The action rewrites an account's runtime state, so it needs the same
// permission as the other auth-file writes, and a business tenant must be able
// to reach it for its own accounts.
func TestModelCooldownClearRoutePermissionAndTenantScope(t *testing.T) {
	if got := ManagementRequestPermission(http.MethodPost, modelCooldownClearPath); got != "auth_files.write" {
		t.Fatalf("permission = %q, want auth_files.write", got)
	}
	if !isTenantScopedManagementPath(modelCooldownClearPath) {
		t.Fatal("a business tenant is denied the route for its own accounts")
	}
	tenant := identity.Principal{EffectiveTenant: identity.Tenant{ID: "00000000-0000-0000-0000-00000000000a"}}
	if deniesTenantResourceScope(tenant, modelCooldownClearPath) {
		t.Fatal("tenant resource scope blocks the route")
	}
}

// The list tells the panel what this backend can do and shows each account's
// cooling models with a reason. The panel is updated on its own schedule, so it
// offers "retry now" only when the capability is advertised.
func TestListAuthFilesAdvertisesAndReportsModelCooldowns(t *testing.T) {
	h, manager, index := coolingModelHandler(t, "")
	// A second account with a healthy model must not grow a cooldown list.
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "healthy.json", FileName: "healthy.json", Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"type": "codex", "email": "ok@example.com"},
	}); err != nil {
		t.Fatalf("register healthy auth: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth-files", nil)
	h.ListAuthFiles(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Files []struct {
			ID             string `json:"id"`
			AuthIndex      string `json:"auth_index"`
			ModelCooldowns []struct {
				Model      string `json:"model"`
				Reason     string `json:"reason"`
				Until      string `json:"until"`
				HTTPStatus int    `json:"http_status"`
			} `json:"model_cooldowns"`
			Restrictions []struct {
				Scope          string `json:"scope"`
				CooldownReason string `json:"cooldown_reason"`
			} `json:"restrictions"`
		} `json:"files"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Capabilities["model-cooldown-clear"] {
		t.Fatalf("capabilities = %v, want model-cooldown-clear", body.Capabilities)
	}
	if !body.Capabilities["disable-cooling"] {
		t.Fatalf("capabilities = %v lost disable-cooling", body.Capabilities)
	}

	seen := 0
	for _, file := range body.Files {
		switch file.ID {
		case "codex-cooling.json":
			seen++
			if file.AuthIndex != index {
				t.Fatalf("auth_index = %q, want the index the clear action takes (%q)", file.AuthIndex, index)
			}
			if len(file.ModelCooldowns) != 2 {
				t.Fatalf("model_cooldowns = %+v, want m-a and m-b", file.ModelCooldowns)
			}
			for i, model := range []string{"m-a", "m-b"} {
				got := file.ModelCooldowns[i]
				if got.Model != model || got.Reason != coreauth.CooldownReasonUpstreamError || got.HTTPStatus != http.StatusServiceUnavailable {
					t.Fatalf("model_cooldowns[%d] = %+v", i, got)
				}
				until, err := time.Parse(time.RFC3339Nano, got.Until)
				if err != nil || !until.After(time.Now()) {
					t.Fatalf("model_cooldowns[%d].until = %q (%v), want a future time", i, got.Until, err)
				}
			}
			reasons := 0
			for _, restriction := range file.Restrictions {
				if restriction.CooldownReason == coreauth.CooldownReasonUpstreamError {
					reasons++
				}
			}
			if reasons == 0 {
				t.Fatalf("restrictions = %+v, want a cooldown_reason on a cooling model", file.Restrictions)
			}
		case "healthy.json":
			seen++
			if len(file.ModelCooldowns) != 0 {
				t.Fatalf("a healthy account lists cooldowns: %+v", file.ModelCooldowns)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("listed %d of the 2 accounts: %s", seen, rec.Body.String())
	}
}
