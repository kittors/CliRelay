package handlers

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestWithAllowedAuthIDsForwardsStrictAllowlist(t *testing.T) {
	t.Parallel()

	if _, ok := requestExecutionMetadata(context.Background())[coreexecutor.AllowedAuthIDsMetadataKey]; ok {
		t.Fatal("allowlist metadata set without WithAllowedAuthIDs")
	}

	ids := []string{" account-a ", "", "account-b"}
	ctx := WithAllowedAuthIDs(context.Background(), ids)
	ids[2] = "mutated"
	got, ok := requestExecutionMetadata(ctx)[coreexecutor.AllowedAuthIDsMetadataKey].([]string)
	if !ok || !reflect.DeepEqual(got, []string{"account-a", "account-b"}) {
		t.Fatalf("allowlist metadata = %#v, want [account-a account-b]", got)
	}
	got[0] = "mutated"
	if again, _ := requestExecutionMetadata(ctx)[coreexecutor.AllowedAuthIDsMetadataKey].([]string); again[0] != "account-a" {
		t.Fatalf("metadata shares backing storage with the context: %#v", again)
	}
}

func TestWithAllowedAuthIDsEmptyListFailsClosed(t *testing.T) {
	t.Parallel()

	for name, ids := range map[string][]string{"nil": nil, "empty": {}, "blank": {" "}} {
		got, ok := requestExecutionMetadata(WithAllowedAuthIDs(context.Background(), ids))[coreexecutor.AllowedAuthIDsMetadataKey].([]string)
		if !ok || got == nil || len(got) != 0 {
			t.Fatalf("%s: allowlist metadata = %#v ok=%v, want present and empty", name, got, ok)
		}
	}
}

// Middleware that runs before the API handlers can attach the allowlist to the
// request context; the handler context derived from it must keep it.
func TestAllowedAuthIDsSurviveHandlerContext(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Request = req.WithContext(WithAllowedAuthIDs(req.Context(), []string{"account-a"}))

	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	ctx, cancel := h.GetContextWithCancel(nil, c, c.Request.Context())
	defer cancel()
	got, ok := requestExecutionMetadata(ctx)[coreexecutor.AllowedAuthIDsMetadataKey].([]string)
	if !ok || !reflect.DeepEqual(got, []string{"account-a"}) {
		t.Fatalf("allowlist metadata = %#v ok=%v, want [account-a]", got, ok)
	}
}
