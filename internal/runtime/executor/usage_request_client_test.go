package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// The request log's Agent column comes from the inbound request: the record a
// reporter publishes names the client that sent it.
func TestUsageReporterNamesTheCaller(t *testing.T) {
	record := capturedGuardRecord(t)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m"}`))
	ua := "Codex Desktop/0.169.0-alpha.11.1 (Mac OS 27.2.1; arm64) unknown (Codex Desktop; 26.1111.21308)"
	c.Request.Header.Set("User-Agent", ua)
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, c)
	reporter := newUsageReporter(ctx, "codex", "client-named", "", nil, true)
	reporter.publish(ctx, coreusage.Detail{InputTokens: 5, OutputTokens: 7})
	got := record("client-named")
	if got.ClientName != "codex-desktop" || got.ClientVersion != "0.169.0-alpha.11.1" || got.UserAgent != ua {
		t.Fatalf("record = %q %q %q", got.ClientName, got.ClientVersion, got.UserAgent)
	}

	// Work without a gateway request behind it has no caller to name.
	bare := context.Background()
	reporter = newUsageReporter(bare, "codex", "client-none", "", nil, true)
	reporter.publish(bare, coreusage.Detail{InputTokens: 5, OutputTokens: 7})
	if got := record("client-none"); got.ClientName != "" || got.ClientVersion != "" || got.UserAgent != "" {
		t.Fatalf("record without a request = %q %q %q", got.ClientName, got.ClientVersion, got.UserAgent)
	}
}
