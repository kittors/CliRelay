package aiaccountstatus

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func flightKey(_ string, subjectID string) string {
	return strings.TrimSpace(subjectID)
}

func reconcileTenantBindings(auths []*coreauth.Auth) error {
	if len(auths) == 0 {
		return nil
	}
	tenantID := ""
	authIDs := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth != nil {
			tenantID = auth.TenantID
			if id := strings.TrimSpace(auth.ID); id != "" {
				authIDs = append(authIDs, id)
			}
		}
	}
	rows, err := usage.ListAIAccountBindingsForTenantAuths(tenantID, authIDs)
	if err != nil {
		return err
	}
	byID := make(map[string]usage.AIAccountTenantBinding, len(rows))
	for _, row := range rows {
		byID[row.AuthID] = row
	}
	// Best-effort per auth: one bad binding row must not 500 the whole status list.
	var firstErr error
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil {
			continue
		}
		row, ok := byID[auth.ID]
		if ok && row.BindingState == "active" && row.AuthSubjectID == identity.ID && row.AuthIndex == auth.EnsureIndex() && row.BindingSeedHash == identity.SeedHash {
			continue
		}
		if err := usage.UpsertAIAccountTenantBinding(auth, identity); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func sanitizeMsg(msg string) string {
	msg = strings.TrimSpace(msg)
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "authorization:") {
		return "upstream request failed"
	}
	if len(msg) > 200 {
		return msg[:200]
	}
	return msg
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func metadataString(auth *coreauth.Auth, keys ...string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := auth.Metadata[key]; ok {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}
