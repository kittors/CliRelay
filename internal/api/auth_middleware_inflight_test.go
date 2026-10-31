package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
)

type tenantKeyProvider struct{ result sdkaccess.Result }

func (p tenantKeyProvider) Identifier() string { return "tenant-key" }

func (p tenantKeyProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	result := p.result
	return &result, nil
}

// The in-flight view shows a request to the tenant its key belongs to, which
// only the auth middleware knows.
func TestAuthMiddlewareGivesTheInFlightRequestItsTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const tenant = "aaaaaaaa-bbbb-cccc-dddd-0000000000a1"
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{tenantKeyProvider{result: sdkaccess.Result{
		Provider: "tenant-key", Principal: "sk-tenant-secret", TenantID: tenant, APIKeyID: "key-a1", APIKeyName: "tenant key",
	}}})

	listed := make(chan []diagnostics.InFlightRequest, 1)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		end := diagnostics.BeginInFlight(diagnostics.EnsureGin(c, "req-tenant-key"))
		defer end()
		c.Next()
	})
	r.Use(AuthMiddleware(manager))
	r.POST("/v1/messages", func(c *gin.Context) {
		requests, _ := diagnostics.InFlight(func(id string) bool { return id == tenant }, time.Now())
		listed <- requests
		c.Status(http.StatusNoContent)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	requests := <-listed
	if len(requests) != 1 || requests[0].RequestID != "req-tenant-key" || requests[0].APIKeyID != "key-a1" || requests[0].APIKeyName != "tenant key" {
		t.Fatalf("the tenant's in-flight requests = %+v", requests)
	}
}
