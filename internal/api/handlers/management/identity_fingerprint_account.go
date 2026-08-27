package management

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type identityFingerprintAccountSummary struct {
	Provider        string         `json:"provider"`
	AccountKey      string         `json:"account_key,omitempty"`
	AuthSubjectID   string         `json:"auth_subject_id,omitempty"`
	Enabled         bool           `json:"enabled"`
	PrimarySource   string         `json:"primary_source"`
	Learned         bool           `json:"learned"`
	LearnedFields   int            `json:"learned_fields"`
	EffectiveFields int            `json:"effective_fields"`
	SourceCounts    map[string]int `json:"source_counts"`
	ProfileKey      string         `json:"profile_key,omitempty"`
	ProfileFamily   string         `json:"profile_family,omitempty"`
	ClientProduct   string         `json:"client_product,omitempty"`
	ClientVariant   string         `json:"client_variant,omitempty"`
	Version         string         `json:"version,omitempty"`
	UpdatedAt       *time.Time     `json:"updated_at,omitempty"`
	LastSeenAt      *time.Time     `json:"last_seen_at,omitempty"`
}

type identityFingerprintProfileDetail struct {
	Summary              identityFingerprintAccountSummary        `json:"summary"`
	Effective            identityfingerprint.EffectiveFingerprint `json:"effective"`
	Learned              *identityfingerprint.LearnedRecord       `json:"learned,omitempty"`
	Selectable           bool                                     `json:"selectable"`
	SelectionBlockReason string                                   `json:"selection_block_reason,omitempty"`
}

type identityFingerprintAccountDetail struct {
	StatusScope               string                                   `json:"status_scope"`
	SubjectScope              string                                   `json:"subject_scope"`
	ShareEligible             bool                                     `json:"share_eligible"`
	CurrentTenantBindingCount int                                      `json:"current_tenant_binding_count"`
	Summary                   identityFingerprintAccountSummary        `json:"summary"`
	Effective                 identityfingerprint.EffectiveFingerprint `json:"effective"`
	Learned                   *identityfingerprint.LearnedRecord       `json:"learned,omitempty"`
	Profiles                  []identityFingerprintProfileDetail       `json:"profiles"`
	Policy                    identityfingerprint.AccountPolicy        `json:"policy"`
	SelectedProfileKey        string                                   `json:"selected_profile_key,omitempty"`
	SelectionReason           string                                   `json:"selection_reason"`
	Preset                    any                                      `json:"preset"`
	BuiltinDefault            any                                      `json:"builtin_default"`
}

func (h *Handler) GetIdentityFingerprintAccount(c *gin.Context) {
	provider, ok := normalizeIdentityFingerprintProvider(c.Query("provider"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider must be one of claude, codex, gemini, xai"})
		return
	}
	accountKey := strings.TrimSpace(c.Query("account_key"))
	if accountKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_key is required"})
		return
	}
	identity, bindingCount, ok := h.identityFingerprintScopeForTenant(effectiveTenantID(c), accountKey)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "account subject is not bound to the current tenant"})
		return
	}
	detail, err := h.identityFingerprintAccountDetail(provider, accountKey, strings.TrimSpace(c.Query("auth_subject_id")))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	applyIdentityFingerprintScope(&detail, identity, bindingCount)
	c.JSON(http.StatusOK, detail)
}

type identityFingerprintAccountPolicyRequest struct {
	Provider         string `json:"provider"`
	AccountKey       string `json:"account_key"`
	Strategy         string `json:"strategy"`
	ActiveProfileKey string `json:"active_profile_key"`
	Revision         int64  `json:"revision"`
}

func (h *Handler) PutIdentityFingerprintAccountPolicy(c *gin.Context) {
	var body identityFingerprintAccountPolicyRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	provider, ok := normalizeIdentityFingerprintProvider(body.Provider)
	if !ok || provider != identityfingerprint.ProviderCodex {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account identity selection is only supported for codex"})
		return
	}
	accountKey := strings.TrimSpace(body.AccountKey)
	if accountKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_key is required"})
		return
	}
	identity, bindingCount, bound := h.identityFingerprintScopeForTenant(effectiveTenantID(c), accountKey)
	if !bound {
		c.JSON(http.StatusNotFound, gin.H{"error": "account subject is not bound to the current tenant"})
		return
	}
	strategy := identityfingerprint.AccountStrategy(strings.TrimSpace(body.Strategy))
	activeProfileKey := strings.TrimSpace(body.ActiveProfileKey)
	switch strategy {
	case identityfingerprint.AccountStrategyCLIPreferred:
		if activeProfileKey != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "active_profile_key must be empty for cli_preferred"})
			return
		}
	case identityfingerprint.AccountStrategyActiveProfile:
		if activeProfileKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "active_profile_key is required for active_profile"})
			return
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "strategy must be cli_preferred or active_profile"})
		return
	}
	policy, err := usage.SaveIdentityFingerprintAccountPolicy(identityfingerprint.AccountPolicy{
		Provider:         provider,
		AccountKey:       accountKey,
		Strategy:         strategy,
		ActiveProfileKey: activeProfileKey,
	}, body.Revision)
	if err != nil {
		switch {
		case errors.Is(err, usage.ErrIdentityFingerprintPolicyConflict):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, usage.ErrIdentityFingerprintProfileMissing),
			errors.Is(err, usage.ErrIdentityFingerprintProfileNotSelectable):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	detail, err := h.identityFingerprintAccountDetail(provider, accountKey, "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	detail.Policy = policy
	applyIdentityFingerprintScope(&detail, identity, bindingCount)
	h.recordAIAccountIdentityAudit(c, "ai_account.identity_policy.update", accountKey, provider, policy.ActiveProfileKey, policy.Revision)
	c.JSON(http.StatusOK, detail)
}

func (h *Handler) DeleteIdentityFingerprintAccountProfile(c *gin.Context) {
	provider, ok := normalizeIdentityFingerprintProvider(c.Query("provider"))
	if !ok || provider != identityfingerprint.ProviderCodex {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account identity profiles are only supported for codex"})
		return
	}
	accountKey := strings.TrimSpace(c.Query("account_key"))
	profileKey := strings.TrimSpace(c.Query("profile_key"))
	if accountKey == "" || profileKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_key and profile_key are required"})
		return
	}
	identity, bindingCount, bound := h.identityFingerprintScopeForTenant(effectiveTenantID(c), accountKey)
	if !bound {
		c.JSON(http.StatusNotFound, gin.H{"error": "account subject is not bound to the current tenant"})
		return
	}
	deleted, policy, err := usage.DeleteIdentityFingerprintProfileAndRepairPolicy(provider, accountKey, profileKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	detail, err := h.identityFingerprintAccountDetail(provider, accountKey, "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	applyIdentityFingerprintScope(&detail, identity, bindingCount)
	if deleted > 0 {
		h.recordAIAccountIdentityAudit(c, "ai_account.identity_profile.delete", accountKey, provider, profileKey, policy.Revision)
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted, "detail": detail})
}

func (h *Handler) recordAIAccountIdentityAudit(c *gin.Context, action, accountKey string, provider identityfingerprint.Provider, profileKey string, revision int64) {
	principal, ok := principalFromContext(c)
	service := h.identity()
	if !ok || service == nil || c == nil || c.Request == nil {
		return
	}
	service.RecordAudit(c.Request.Context(), identity.AuditEvent{
		TenantID:       principal.EffectiveTenant.ID,
		ActorKind:      principal.Kind,
		ActorUserID:    principal.User.ID,
		ActorSessionID: principal.SessionID,
		Action:         action,
		ResourceType:   "ai_account_subject",
		ResourceID:     accountKey,
		Result:         "success",
		Changes: map[string]any{
			"provider":       string(provider),
			"profile_key":    strings.TrimSpace(profileKey),
			"revision":       revision,
			"tenant_context": principal.EffectiveTenant.ID,
		},
	})
}

func (h *Handler) enrichAuthFileIdentityFingerprintSummaries(files []map[string]any, auths []*coreauth.Auth) {
	if len(files) == 0 || len(auths) == 0 {
		return
	}
	summaries := make(map[string]identityFingerprintAccountSummary, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		summary := h.identityFingerprintSummaryForAuth(auth)
		if summary == nil {
			continue
		}
		if id := strings.TrimSpace(auth.ID); id != "" {
			summaries[id] = *summary
		}
	}
	if len(summaries) == 0 {
		return
	}
	for _, file := range files {
		id, _ := file["id"].(string)
		if summary, ok := summaries[strings.TrimSpace(id)]; ok {
			file["identity_fingerprint_summary"] = summary
		}
	}
}

func (h *Handler) identityFingerprintSummaryForAuth(auth *coreauth.Auth) *identityFingerprintAccountSummary {
	provider, ok := normalizeIdentityFingerprintProvider("")
	if auth != nil {
		provider, ok = normalizeIdentityFingerprintProvider(auth.Provider)
	}
	if !ok {
		return nil
	}
	accountKey, authSubjectID := identityFingerprintAccountForAuth(auth)
	if accountKey == "" {
		return nil
	}
	detail, err := h.identityFingerprintAccountDetail(provider, accountKey, authSubjectID)
	if err != nil {
		return nil
	}
	identity := usage.ResolveAuthSubjectIdentity(auth)
	count := 1
	if auth != nil {
		if counts, err := usage.CountAIAccountTenantBindings(auth.TenantID, []string{accountKey}); err == nil && counts[accountKey] > 0 {
			count = counts[accountKey]
		}
	}
	applyIdentityFingerprintScope(&detail, identity, count)
	return &detail.Summary
}

func (h *Handler) identityFingerprintAccountDetail(provider identityfingerprint.Provider, accountKey, authSubjectID string) (identityFingerprintAccountDetail, error) {
	current := h.currentIdentityFingerprintConfig()
	if provider == identityfingerprint.ProviderCodex {
		records, err := usage.ListIdentityFingerprintProfiles(provider, accountKey)
		if err != nil {
			return identityFingerprintAccountDetail{}, err
		}
		policy, err := usage.GetIdentityFingerprintAccountPolicy(provider, accountKey)
		if err != nil {
			return identityFingerprintAccountDetail{}, err
		}
		selection := identityfingerprint.SelectCodexProfile(records, policy)
		profiles := make([]identityFingerprintProfileDetail, 0, len(records))
		for i := range records {
			record := records[i]
			_, effective := identityfingerprint.ResolveCodexProfile(current.Codex, &record)
			effective.AccountKey = accountKey
			if effective.AuthSubjectID == "" {
				effective.AuthSubjectID = authSubjectID
			}
			selectable, blockReason := identityfingerprint.CodexProfileOutboundEligibility(&record)
			profiles = append(profiles, identityFingerprintProfileDetail{
				Summary:              *buildIdentityFingerprintSummary(provider, accountKey, effective.AuthSubjectID, &record, effective),
				Effective:            effective,
				Learned:              &record,
				Selectable:           selectable,
				SelectionBlockReason: blockReason,
			})
		}
		var learned *identityfingerprint.LearnedRecord
		var effective identityfingerprint.EffectiveFingerprint
		if selection.Profile != nil {
			learned = selection.Profile
			_, effective = identityfingerprint.ResolveCodexProfile(current.Codex, learned)
		} else {
			_, effective = identityfingerprint.ResolveCodexSafeFallback(current.Codex)
		}
		effective.AccountKey = accountKey
		if effective.AuthSubjectID == "" {
			effective.AuthSubjectID = authSubjectID
		}
		selectedProfileKey := ""
		if learned != nil {
			selectedProfileKey = learned.ProfileKey
		}
		return identityFingerprintAccountDetail{
			Summary:            *buildIdentityFingerprintSummary(provider, accountKey, effective.AuthSubjectID, learned, effective),
			Effective:          effective,
			Learned:            learned,
			Profiles:           profiles,
			Policy:             selection.Policy,
			SelectedProfileKey: selectedProfileKey,
			SelectionReason:    selection.Reason,
			Preset:             current.Codex,
			BuiltinDefault:     config.DefaultCodexIdentityFingerprint(),
		}, nil
	}

	learned, err := usage.GetIdentityFingerprint(provider, accountKey)
	if err != nil {
		return identityFingerprintAccountDetail{}, err
	}
	effective, preset, builtinDefault := resolveIdentityFingerprint(current, provider, learned)
	effective.AccountKey = accountKey
	if effective.AuthSubjectID == "" {
		effective.AuthSubjectID = authSubjectID
	}
	profiles := make([]identityFingerprintProfileDetail, 0, 1)
	if learned != nil {
		profiles = append(profiles, identityFingerprintProfileDetail{
			Summary:    *buildIdentityFingerprintSummary(provider, accountKey, effective.AuthSubjectID, learned, effective),
			Effective:  effective,
			Learned:    learned,
			Selectable: true,
		})
	}
	return identityFingerprintAccountDetail{
		Summary:         *buildIdentityFingerprintSummary(provider, accountKey, effective.AuthSubjectID, learned, effective),
		Effective:       effective,
		Learned:         learned,
		Profiles:        profiles,
		Policy:          identityfingerprint.NormalizeAccountPolicy(provider, accountKey, identityfingerprint.AccountPolicy{}),
		SelectionReason: "single_profile",
		Preset:          preset,
		BuiltinDefault:  builtinDefault,
	}, nil
}

func (h *Handler) identityFingerprintScopeForTenant(tenantID, accountKey string) (*usage.AuthSubjectIdentity, int, bool) {
	if h == nil || h.authManager == nil {
		return nil, 0, false
	}
	tenantID = strings.TrimSpace(tenantID)
	accountKey = strings.TrimSpace(accountKey)
	matchedAuths := make([]*coreauth.Auth, 0)
	authIDs := make([]string, 0)
	var matched *usage.AuthSubjectIdentity
	for _, auth := range h.authManager.ListForTenant(tenantID) {
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil || identity.ID != accountKey {
			continue
		}
		matched = identity
		matchedAuths = append(matchedAuths, auth)
		if authID := strings.TrimSpace(auth.ID); authID != "" {
			authIDs = append(authIDs, authID)
		}
	}
	if matched == nil || len(matchedAuths) == 0 {
		return nil, 0, false
	}

	rows, err := usage.ListAIAccountBindingsForTenantAuths(tenantID, authIDs)
	if err != nil {
		return nil, 0, false
	}
	byAuthID := make(map[string]usage.AIAccountTenantBinding, len(rows))
	for _, row := range rows {
		byAuthID[row.AuthID] = row
	}
	count := 0
	for _, auth := range matchedAuths {
		identity := usage.ResolveAuthSubjectIdentity(auth)
		if identity == nil {
			continue
		}
		row, exists := byAuthID[auth.ID]
		bindingCurrent := exists && row.BindingState == "active" &&
			row.AuthSubjectID == identity.ID && row.AuthIndex == auth.EnsureIndex() &&
			row.BindingSeedHash == identity.SeedHash
		if !bindingCurrent {
			// Management reads may repair a missing/stale lifecycle binding, but a
			// healthy binding remains read-only and never becomes a per-request UPSERT.
			if err := usage.UpsertAIAccountTenantBinding(auth, identity); err != nil {
				return nil, 0, false
			}
		}
		count++
	}
	return matched, count, count > 0
}

func applyIdentityFingerprintScope(detail *identityFingerprintAccountDetail, identity *usage.AuthSubjectIdentity, bindingCount int) {
	if detail == nil || identity == nil {
		return
	}
	detail.StatusScope = usage.AIAccountStatusScopeShared
	detail.SubjectScope = identity.SubjectScope
	detail.ShareEligible = identity.ShareEligible
	detail.CurrentTenantBindingCount = bindingCount
}

func (h *Handler) currentIdentityFingerprintConfig() config.IdentityFingerprintConfig {
	current := config.IdentityFingerprintConfig{}
	if h != nil {
		h.mu.Lock()
		if h.cfg != nil {
			current = h.cfg.IdentityFingerprint
		}
		h.mu.Unlock()
	}
	return config.NormalizeIdentityFingerprintConfig(current)
}

func resolveIdentityFingerprint(current config.IdentityFingerprintConfig, provider identityfingerprint.Provider, learned *identityfingerprint.LearnedRecord) (identityfingerprint.EffectiveFingerprint, any, any) {
	switch provider {
	case identityfingerprint.ProviderClaude:
		_, effective := identityfingerprint.ResolveClaude(current.Claude, learned)
		return effective, current.Claude, config.DefaultClaudeIdentityFingerprint()
	case identityfingerprint.ProviderCodex:
		_, effective := identityfingerprint.ResolveCodex(current.Codex, learned)
		return effective, current.Codex, config.DefaultCodexIdentityFingerprint()
	case identityfingerprint.ProviderGemini:
		_, effective := identityfingerprint.ResolveGemini(current.Gemini, learned)
		return effective, current.Gemini, config.DefaultGeminiIdentityFingerprint()
	case identityfingerprint.ProviderXAI:
		_, effective := identityfingerprint.ResolveXAI(current.XAI, learned)
		return effective, current.XAI, config.DefaultXAIIdentityFingerprint()
	case identityfingerprint.ProviderKimi:
		_, effective := identityfingerprint.ResolveKimi(current.Kimi, learned)
		return effective, current.Kimi, config.DefaultKimiIdentityFingerprint()
	default:
		return identityfingerprint.EffectiveFingerprint{}, nil, nil
	}
}

func buildIdentityFingerprintSummary(provider identityfingerprint.Provider, accountKey, authSubjectID string, learned *identityfingerprint.LearnedRecord, effective identityfingerprint.EffectiveFingerprint) *identityFingerprintAccountSummary {
	counts := identityFingerprintSourceCounts(effective.Fields)
	learnedFields := 0
	if learned != nil {
		for _, value := range learned.Fields {
			if strings.TrimSpace(value) != "" {
				learnedFields++
			}
		}
	}
	summary := &identityFingerprintAccountSummary{
		Provider:        string(provider),
		AccountKey:      strings.TrimSpace(accountKey),
		AuthSubjectID:   strings.TrimSpace(authSubjectID),
		Enabled:         effective.Enabled,
		PrimarySource:   primaryIdentityFingerprintSource(counts),
		Learned:         learned != nil && learnedFields > 0,
		LearnedFields:   learnedFields,
		EffectiveFields: sumIdentityFingerprintCounts(counts),
		SourceCounts:    counts,
		ProfileKey:      effective.ProfileKey,
		ProfileFamily:   effective.ProfileFamily,
		ClientProduct:   effective.ClientProduct,
		Version:         identityFingerprintSummaryVersion(provider, effective),
	}
	if learned != nil {
		summary.ClientVariant = learned.ClientVariant
		if !learned.UpdatedAt.IsZero() {
			updatedAt := learned.UpdatedAt
			summary.UpdatedAt = &updatedAt
		}
		if !learned.LastSeenAt.IsZero() {
			lastSeenAt := learned.LastSeenAt
			summary.LastSeenAt = &lastSeenAt
		}
	}
	return summary
}

func identityFingerprintSummaryVersion(provider identityfingerprint.Provider, effective identityfingerprint.EffectiveFingerprint) string {
	switch provider {
	case identityfingerprint.ProviderClaude:
		if value := identityFingerprintEffectiveField(effective, identityfingerprint.FieldClaudeCLIVersion); value != "" {
			return value
		}
	case identityfingerprint.ProviderCodex:
		if value := identityFingerprintEffectiveField(effective, identityfingerprint.FieldCodexVersion); value != "" {
			return value
		}
	case identityfingerprint.ProviderXAI:
		return strings.TrimSpace(effective.Version)
	case identityfingerprint.ProviderKimi:
		if value := identityFingerprintEffectiveField(effective, identityfingerprint.FieldKimiVersion); value != "" {
			return value
		}
	}
	return strings.TrimSpace(effective.Version)
}

func identityFingerprintEffectiveField(effective identityfingerprint.EffectiveFingerprint, field string) string {
	if effective.Fields == nil {
		return ""
	}
	return strings.TrimSpace(effective.Fields[field].Value)
}

func identityFingerprintSourceCounts(fields map[string]identityfingerprint.FieldValue) map[string]int {
	counts := map[string]int{
		string(identityfingerprint.FieldSourceLearned):        0,
		string(identityfingerprint.FieldSourcePreset):         0,
		string(identityfingerprint.FieldSourceBuiltinDefault): 0,
	}
	for _, field := range fields {
		if strings.TrimSpace(field.Value) == "" {
			continue
		}
		source := strings.TrimSpace(string(field.Source))
		if source == "" {
			source = string(identityfingerprint.FieldSourceBuiltinDefault)
		}
		counts[source]++
	}
	return counts
}

func primaryIdentityFingerprintSource(counts map[string]int) string {
	for _, source := range []identityfingerprint.FieldSource{
		identityfingerprint.FieldSourceLearned,
		identityfingerprint.FieldSourcePreset,
		identityfingerprint.FieldSourceBuiltinDefault,
	} {
		if counts[string(source)] > 0 {
			return string(source)
		}
	}
	return string(identityfingerprint.FieldSourceBuiltinDefault)
}

func sumIdentityFingerprintCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}

func identityFingerprintAccountForAuth(auth *coreauth.Auth) (string, string) {
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity != nil {
		id := strings.TrimSpace(identity.ID)
		return id, id
	}
	if auth == nil {
		return "", ""
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return id, ""
	}
	if idx := strings.TrimSpace(auth.EnsureIndex()); idx != "" {
		return idx, ""
	}
	return "", ""
}

func normalizeIdentityFingerprintProvider(value string) (identityfingerprint.Provider, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(identityfingerprint.ProviderClaude):
		return identityfingerprint.ProviderClaude, true
	case string(identityfingerprint.ProviderCodex), "openai":
		return identityfingerprint.ProviderCodex, true
	case string(identityfingerprint.ProviderGemini), "google":
		return identityfingerprint.ProviderGemini, true
	case string(identityfingerprint.ProviderXAI), "grok":
		return identityfingerprint.ProviderXAI, true
	case string(identityfingerprint.ProviderKimi), "moonshot":
		return identityfingerprint.ProviderKimi, true
	default:
		return "", false
	}
}
