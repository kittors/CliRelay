package accessfreeze

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

// subjectFromContext reads the requester the API key authentication found:
// the portal account that owns the key, or the key itself when no portal
// account owns it. A request that did not come in with an API key (a panel
// probe, a model test) has none, and is never counted against anyone.
func subjectFromContext(ctx context.Context) (Subject, bool) {
	metadata := accessMetadataFromContext(ctx)
	if metadata == nil {
		return Subject{}, false
	}
	return subjectFromAccess(metadata["tenant-id"], metadata["end-user-id"], metadata["api-key-id"])
}

func subjectFromAccess(tenantID, endUserID, apiKeyID string) (Subject, bool) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return Subject{}, false
	}
	if id := strings.TrimSpace(endUserID); id != "" {
		return Subject{TenantID: tenantID, Kind: SubjectEndUser, ID: id}, true
	}
	if id := strings.TrimSpace(apiKeyID); id != "" {
		return Subject{TenantID: tenantID, Kind: SubjectAPIKey, ID: id}, true
	}
	return Subject{}, false
}

func accessMetadataFromContext(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	ginCtx, _ := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if ginCtx == nil {
		return nil
	}
	value, exists := ginCtx.Get("accessMetadata")
	if !exists {
		return nil
	}
	metadata, _ := value.(map[string]string)
	return metadata
}
