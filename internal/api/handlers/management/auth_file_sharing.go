package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// sharedAuthFileKeys are the fields of an auth-file entry a grantee sees for a
// credential the platform shares with it: enough to recognise the account,
// route to it by channel name and read its health. The file path, ID token
// claims and provider-specific switches stay with the owner, and nothing here
// is a secret; the credential itself never leaves the system tenant.
var sharedAuthFileKeys = []string{
	"id",
	"auth_index",
	// A server-derived hash of the upstream account; lets the grantee tell
	// which of its own credentials are copies of a shared one.
	"auth_subject_id",
	"name",
	"type",
	"provider",
	"label",
	"email",
	"account_type",
	"plan_type",
	"status",
	"status_message",
	"disabled",
	"unavailable",
	"next_retry_after",
	"restrictions",
	"default_tags",
	"custom_tags",
	"display_tags",
	"concurrency_limit",
	"subscription_expired",
	"subscription_expires_at",
	"subscription_expires_at_ms",
	"subscription_period",
	"subscription_remaining_days",
	"subscription_remaining_minutes",
	"subscription_started_at",
	"subscription_started_at_ms",
}

func sharedAuthFileEntries(auths []*coreauth.Auth) []map[string]any {
	byID := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		if auth != nil {
			byID[auth.ID] = auth
		}
	}
	entries := managementauthfiles.ListEntries(auths, managementauthfiles.EntryOptions{})
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		shared := make(map[string]any, len(sharedAuthFileKeys)+1)
		for _, key := range sharedAuthFileKeys {
			if value, ok := entry[key]; ok {
				shared[key] = value
			}
		}
		if auth := byID[fmt.Sprint(entry["id"])]; auth != nil {
			hideSharedAccountIdentity(shared, auth)
		}
		shared["shared"] = true
		out = append(out, shared)
	}
	return out
}

// hideSharedAccountIdentity replaces what identifies a shared account at its
// upstream (the login email, and the credential id and file name built from
// it) with the stand-ins every tenant view uses (coreauth.Auth.SharedName and
// SharedID); status messages that quote the account are rewritten the same way.
func hideSharedAccountIdentity(entry map[string]any, auth *coreauth.Auth) {
	delete(entry, "email")
	if pairs := auth.SharedStandIns(); len(pairs) > 0 {
		args := make([]string, 0, 2*len(pairs))
		for _, pair := range pairs {
			args = append(args, pair[0], pair[1])
		}
		replacer := strings.NewReplacer(args...)
		for key, value := range entry {
			if text, ok := value.(string); ok {
				entry[key] = replacer.Replace(text)
			}
		}
	}
	name := auth.SharedName()
	entry["id"] = auth.SharedID()
	entry["name"] = name
	entry["label"] = name
}

// decorateAuthFilesResponse adds the sharing view to a GET /auth-files body:
// who each of its credentials is shared with for the system tenant, the
// credentials shared with it for any other tenant. files stays the tenant's
// own credentials, the ones it may manage.
func (h *Handler) decorateAuthFilesResponse(c *gin.Context, tenantID string, files []map[string]any, body gin.H) {
	body["sharing"] = sharingCapabilityFor(c)
	if h == nil || h.authManager == nil || !usage.ResourceGrantsAvailable() {
		return
	}
	if tenantID != identity.SystemTenantID {
		shared := sharedAuthFileEntries(h.authManager.ListSharedWithTenant(tenantID))
		body["shared_files"] = shared
		markCopiesOfSharedCredentials(files, shared)
		return
	}
	h.markTenantCopies(c, files)
	grants, err := usage.ListResourceGrants(sharingContext(c), resourcegrants.KindAuth)
	if err != nil || len(grants) == 0 {
		return
	}
	for _, file := range files {
		id, _ := file["id"].(string)
		if sharing := sharingFromGrantees(grants[id]); sharing != nil {
			file["sharing"] = sharing
		}
	}
}

func entrySubject(entry map[string]any) string {
	subject, _ := entry["auth_subject_id"].(string)
	return strings.TrimSpace(subject)
}

// markCopiesOfSharedCredentials flags the tenant's own credentials that log in
// to the same upstream account as a credential shared with it. Two refresh
// tokens of one account revoke each other on every refresh, so the copy should
// go once the shared one serves the tenant.
func markCopiesOfSharedCredentials(files, shared []map[string]any) {
	if len(files) == 0 || len(shared) == 0 {
		return
	}
	sharedSubjects := make(map[string]struct{}, len(shared))
	for _, entry := range shared {
		if subject := entrySubject(entry); subject != "" {
			sharedSubjects[subject] = struct{}{}
		}
	}
	for _, file := range files {
		if _, ok := sharedSubjects[entrySubject(file)]; ok && entrySubject(file) != "" {
			file["duplicates_shared"] = true
		}
	}
}

// markTenantCopies lists, on each of the system tenant's credentials, the
// tenants that hold their own credential for the same upstream account: the
// copies to replace with a grant of this one. Read-only on purpose: removing a
// tenant's copy also removes its channel references from that tenant's
// routing, which is the tenant administrator's call.
func (h *Handler) markTenantCopies(c *gin.Context, files []map[string]any) {
	if h == nil || h.authManager == nil || len(files) == 0 {
		return
	}
	bySubject := make(map[string][]map[string]any, len(files))
	for _, file := range files {
		if subject := entrySubject(file); subject != "" {
			bySubject[subject] = append(bySubject[subject], file)
		}
	}
	if len(bySubject) == 0 {
		return
	}
	holders := make(map[string]map[string]struct{})
	for _, auth := range h.authManager.List() {
		if auth == nil || coreauth.NormalizedTenantID(auth.TenantID) == identity.SystemTenantID {
			continue
		}
		subject := usage.ResolveAuthSubjectIdentity(auth)
		if subject == nil || strings.TrimSpace(subject.ID) == "" {
			continue
		}
		if _, ok := bySubject[subject.ID]; !ok {
			continue
		}
		if holders[subject.ID] == nil {
			holders[subject.ID] = make(map[string]struct{})
		}
		holders[subject.ID][coreauth.NormalizedTenantID(auth.TenantID)] = struct{}{}
	}
	ctx := sharingContext(c)
	for subject, tenants := range holders {
		ids := make([]string, 0, len(tenants))
		for tenant := range tenants {
			ids = append(ids, tenant)
		}
		refs := tenantRefs(ctx, ids)
		for _, file := range bySubject[subject] {
			file["tenant_copies"] = refs
		}
	}
}

// PutAuthFileSharing sets which tenants may use some of the system tenant's
// credentials: {"ids": [...], "all": true} for every tenant,
// {"ids": [...], "tenant_ids": [...]} for some, neither to stop sharing them.
func (h *Handler) PutAuthFileSharing(c *gin.Context) {
	if !requireSharingManager(c) {
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var req struct {
		IDs []string `json:"ids"`
		sharingRequest
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	owned := make(map[string]struct{})
	for _, auth := range h.authManager.ListForTenant(identity.SystemTenantID) {
		if auth != nil {
			owned[auth.ID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(req.IDs))
	var unknown []string
	for _, id := range req.IDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := owned[id]; !ok {
			unknown = append(unknown, id)
			continue
		}
		ids = append(ids, id)
	}
	if len(unknown) > 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("not a credential of the system tenant: %s", strings.Join(unknown, ", "))})
		return
	}
	if len(ids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids is required"})
		return
	}
	grantees, err := resolveGrantees(sharingContext(c), req.sharingRequest)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, err := usage.ReplaceResourceGrants(sharingContext(c), resourcegrants.KindAuth, ids, grantees); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save credential sharing: %v", err)})
		return
	}
	h.reloadSharedAuthGrants()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "ids": ids, "sharing": sharingFromGrantees(grantees)})
}

// dropSharedAuthGrants revokes the grants of system credentials that were just
// deleted. Credential IDs come from file names, so a grant left behind would
// share whatever credential is uploaded later under the same name.
func (h *Handler) dropSharedAuthGrants(ctx context.Context, tenantID string, authIDs []string) {
	if tenantID != identity.SystemTenantID || len(authIDs) == 0 || !usage.ResourceGrantsAvailable() {
		return
	}
	grants, err := usage.ListResourceGrants(ctx, resourcegrants.KindAuth)
	if err != nil {
		log.WithError(err).Warn("shared resources: read credential grants after delete")
		return
	}
	var granted []string
	for _, id := range authIDs {
		if len(grants[id]) > 0 {
			granted = append(granted, id)
		}
	}
	if len(granted) == 0 {
		return
	}
	if _, err := usage.ReplaceResourceGrants(ctx, resourcegrants.KindAuth, granted, nil); err != nil {
		log.WithError(err).Warn("shared resources: revoke grants of deleted credentials")
		return
	}
	h.reloadSharedAuthGrants()
}
