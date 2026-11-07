package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
)

// inFlightGatewayRequest puts a gateway request of tenantID in flight, the way
// the request logging and auth middleware do, until the test ends.
func inFlightGatewayRequest(t *testing.T, requestID, tenantID string) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	d := diagnostics.EnsureGin(c, requestID)
	t.Cleanup(diagnostics.BeginInFlight(d))
	diagnostics.SetAuth(c, "claude", "sk-ant-secret-value-1234", "key-"+requestID, "name-"+requestID)
	diagnostics.SetTenant(c, tenantID)
	routetrace.FromContext(c.Request.Context()).BeginAttempt(routetrace.AttemptInfo{AuthID: "acct-" + requestID, Provider: "claude"})
}

type inFlightBody struct {
	Requests    []diagnostics.InFlightRequest `json:"requests"`
	Total       int                           `json:"total"`
	GeneratedAt string                        `json:"generated_at"`
}

func serveInFlight(t *testing.T, principal *identity.Principal) inFlightBody {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage/monitor/in-flight", nil)
	if principal != nil {
		c.Set(managementPrincipalKey, *principal)
	}
	(&Handler{cfg: &config.Config{}}).UsageLogs().GetInFlightRequests(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-ant-secret") {
		t.Fatalf("the in-flight view leaks a key: %s", rec.Body.String())
	}
	var body inFlightBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// A tenant sees its own requests in flight; a request without a tenant is the
// system tenant's, as its request log row is.
func TestGetInFlightRequestsIsScopedToEffectiveTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const businessTenant = "aaaaaaaa-bbbb-cccc-dddd-0000000000f1"
	inFlightGatewayRequest(t, "req-business", businessTenant)
	inFlightGatewayRequest(t, "req-system", "")

	business := serveInFlight(t, &identity.Principal{EffectiveTenant: identity.Tenant{ID: businessTenant}})
	if business.Total != 1 || len(business.Requests) != 1 || business.Requests[0].RequestID != "req-business" || business.GeneratedAt == "" {
		t.Fatalf("business tenant sees %+v", business)
	}
	entry := business.Requests[0]
	if entry.Stage != routetrace.StageUpstream || entry.Account == nil || entry.Account.AuthID != "acct-req-business" || entry.APIKeyName != "name-req-business" {
		t.Fatalf("entry = %+v", entry)
	}

	system := serveInFlight(t, nil)
	var ids []string
	for _, request := range system.Requests {
		ids = append(ids, request.RequestID)
	}
	if strings.Contains(strings.Join(ids, ","), "req-business") || !strings.Contains(strings.Join(ids, ","), "req-system") {
		t.Fatalf("system tenant sees %v, want its own request only", ids)
	}
}

// The panel follows the latest release, not this binary, so it shows the
// in-flight table only to a backend that says it has one.
func TestGetUsageMonitorOverviewDeclaresInFlightRequests(t *testing.T) {
	initMonitorHandlerDB(t)
	rec := serveMonitor(t, &Handler{cfg: &config.Config{}}, "/usage/monitor/overview?range=1h", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Range struct {
			Key string `json:"key"`
		} `json:"range"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Capabilities["in-flight-requests"] {
		t.Fatalf("capabilities = %v, want in-flight-requests", body.Capabilities)
	}
	if body.Range.Key != usage.MonitorRange1h {
		t.Fatalf("the overview must keep its own fields at the top level, range = %q", body.Range.Key)
	}
}
