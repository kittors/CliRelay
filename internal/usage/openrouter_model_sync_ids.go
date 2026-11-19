package usage

import "strings"

func openRouterOwnerFromModelID(modelID string) string {
	prefix, _, found := strings.Cut(strings.TrimSpace(modelID), "/")
	if !found {
		return openRouterModelSource
	}
	prefix = strings.TrimLeft(prefix, "~～")
	if strings.TrimSpace(prefix) == "" {
		return openRouterModelSource
	}
	return normalizeModelOwnerValue(prefix)
}

func openRouterLocalModelID(remoteModelID, owner string) string {
	modelID := openRouterProviderlessModelID(remoteModelID)
	if normalizeModelOwnerValue(owner) == "anthropic" {
		modelID = strings.ReplaceAll(modelID, ".", "-")
		modelID = openRouterStripDateSuffix(modelID)
	}
	return modelID
}

func openRouterProviderlessModelID(remoteModelID string) string {
	modelID := strings.TrimSpace(remoteModelID)
	if _, suffix, found := strings.Cut(modelID, "/"); found {
		return strings.TrimSpace(suffix)
	}
	return modelID
}

func openRouterStripDateSuffix(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return modelID
	}
	for {
		stripped := stripTrailing8DigitDate(modelID)
		if stripped == modelID {
			stripped = stripTrailingSegmentedDate(modelID)
		}
		if stripped == modelID {
			break
		}
		modelID = stripped
	}
	return modelID
}

// stripTrailing8DigitDate strips a trailing -YYYYMMDD suffix (8 digits, preceded by a dash).
// Example: "qwen3.5-plus-20260420" -> "qwen3.5-plus", "claude-3-5-haiku-20241022" -> "claude-3-5-haiku"
func stripTrailing8DigitDate(modelID string) string {
	if len(modelID) < 10 {
		return modelID
	}
	dateStart := len(modelID) - 8
	if modelID[dateStart-1] != '-' {
		return modelID
	}
	for _, ch := range modelID[dateStart:] {
		if ch < '0' || ch > '9' {
			return modelID
		}
	}
	return modelID[:dateStart-1]
}

// stripTrailingSegmentedDate strips trailing date patterns like -MM-DD or -YYYY-MM-DD.
// Examples: "qwen3.5-plus-02-15" -> "qwen3.5-plus", "model-2026-04-20" -> "model"
func stripTrailingSegmentedDate(modelID string) string {
	parts := strings.Split(modelID, "-")
	if len(parts) < 3 {
		return modelID
	}
	tail2 := parts[len(parts)-2]
	tail1 := parts[len(parts)-1]
	if len(parts) >= 4 {
		tail3 := parts[len(parts)-3]
		if len(tail3) == 4 && len(tail2) == 2 && len(tail1) == 2 && isAllDigits(tail3) && isAllDigits(tail2) && isAllDigits(tail1) {
			return strings.Join(parts[:len(parts)-3], "-")
		}
	}
	if len(tail2) == 2 && len(tail1) == 2 && isAllDigits(tail2) && isAllDigits(tail1) {
		return strings.Join(parts[:len(parts)-2], "-")
	}
	return modelID
}

func isAllDigits(s string) bool {
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// openRouterStripAnthropicReleaseDate is retained for backward compat in legacy alias detection.
// New code should use openRouterStripDateSuffix instead.
func openRouterStripAnthropicReleaseDate(modelID string) string {
	return openRouterStripDateSuffix(modelID)
}

func openRouterLegacyLocalModelIDs(tenantID, remoteModelID, owner, modelID string) []string {
	var ids []string
	seen := map[string]struct{}{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || id == modelID {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	add(remoteModelID)
	providerless := openRouterProviderlessModelID(remoteModelID)
	add(providerless)
	if normalizeModelOwnerValue(owner) == "anthropic" {
		add(strings.ReplaceAll(providerless, ".", "-"))
	}
	for _, aliasID := range openRouterExistingDateSuffixAliasIDs(tenantID, modelID) {
		add(aliasID)
	}
	return ids
}

func openRouterExistingDateSuffixAliasIDs(tenantID, modelID string) []string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	prefix := modelID + "-"
	var aliases []string
	// IDs by prefix, not the whole catalog: this runs for every listing of a sync.
	for _, id := range modelConfigIDsWithPrefix(tenantID, prefix) {
		aliasID := strings.TrimSpace(id)
		if aliasID == modelID || !strings.HasPrefix(aliasID, prefix) {
			continue
		}
		if openRouterStripDateSuffix(aliasID) == modelID {
			aliases = append(aliases, aliasID)
		}
	}
	return aliases
}

// openRouterExistingAnthropicReleaseDateAliasIDs is retained for backward compat.
// New code should use openRouterExistingDateSuffixAliasIDs instead.
func openRouterExistingAnthropicReleaseDateAliasIDs(modelID string) []string {
	return openRouterExistingDateSuffixAliasIDs(systemTenantID, modelID)
}

func openRouterIsDateSuffixAlias(modelID, baseModelID string) bool {
	modelID = strings.TrimSpace(modelID)
	baseModelID = strings.TrimSpace(baseModelID)
	return modelID != "" && baseModelID != "" && modelID != baseModelID && openRouterStripDateSuffix(modelID) == baseModelID
}

// openRouterIsAnthropicReleaseDateAlias is retained for backward compat.
// New code should use openRouterIsDateSuffixAlias instead.
func openRouterIsAnthropicReleaseDateAlias(modelID, baseModelID string) bool {
	return openRouterIsDateSuffixAlias(modelID, baseModelID)
}

// openRouterCanonicalGroupID computes a unique group key for variant merging.
// It applies the same normalizations as openRouterLocalModelID (Anthropic dot-to-dash)
// and then strips date/version suffixes. Unlike openRouterLocalModelID, this is
// called on raw remote IDs (with provider prefix) and always strips date suffixes
// regardless of owner, so exact matches and date variants of the same model are
// grouped together in the merge pass.
func openRouterCanonicalGroupID(remoteModelID string) string {
	providerless := openRouterProviderlessModelID(remoteModelID)
	if normalizeModelOwnerValue(openRouterOwnerFromModelID(remoteModelID)) == "anthropic" {
		providerless = strings.ReplaceAll(providerless, ".", "-")
	}
	return openRouterStripDateSuffix(providerless)
}
