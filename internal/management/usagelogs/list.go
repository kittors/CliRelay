package usagelogs

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type authChannelMeta struct {
	label    string
	provider string
	authType string
}

type nameMaps struct {
	// keyNameMap is the display name of a key (its account's name when owned),
	// keyed by the key's stable id.
	keyNameMap                   map[string]string
	channelNameMap               map[string]string
	authIndexChannelMap          map[string]string
	ambiguousAuthIndexChannelMap map[string][]string
	authMetaByIndex              map[string]authChannelMeta
	authIndexGroup               map[string][]string
	authSubjectByIndex           map[string]string
	authIndexesBySubject         map[string][]string
	authMetaBySubject            map[string]authChannelMeta
}

func channelFilterSelectors(
	channels []string,
	channelNameMap, authIndexChannelMap map[string]string,
	ambiguousAuthIndexChannelMap map[string][]string,
	authMetaByIndex map[string]authChannelMeta,
	authIndexGroup map[string][]string,
	authSubjectByIndex map[string]string,
	authIndexesBySubject map[string][]string,
	authMetaBySubject map[string]authChannelMeta,
) ([]string, []string, []string, map[string][]string) {
	// Preserve original selected values. Only use lower-case keys for label matching.
	selectedRaw := make([]string, 0, len(channels))
	selectedLabelKeys := make(map[string]struct{})
	for _, part := range channels {
		raw := strings.TrimSpace(part)
		if raw == "" {
			continue
		}
		selectedRaw = append(selectedRaw, raw)
		selectedLabelKeys[strings.ToLower(raw)] = struct{}{}
	}
	if len(selectedRaw) == 0 {
		return nil, nil, nil, nil
	}

	var authSubjectIDs []string
	var authIndexes []string
	var channelNames []string
	authIndexChannelNames := make(map[string][]string)
	seenSubject := make(map[string]struct{})
	seenAuthIndex := make(map[string]struct{})
	seenChannelName := make(map[string]struct{})

	appendAuthIndex := func(idx string) {
		idx = strings.TrimSpace(idx)
		if idx == "" {
			return
		}
		if _, ok := seenAuthIndex[idx]; ok {
			return
		}
		seenAuthIndex[idx] = struct{}{}
		authIndexes = append(authIndexes, idx)
	}
	appendSubject := func(subjectID string) {
		subjectID = strings.TrimSpace(subjectID)
		if subjectID == "" {
			return
		}
		if _, ok := seenSubject[subjectID]; ok {
			return
		}
		seenSubject[subjectID] = struct{}{}
		authSubjectIDs = append(authSubjectIDs, subjectID)
		// Also match historical credential-instance rows that predate subject
		// population or still only carry auth_index.
		for _, member := range authIndexesBySubject[subjectID] {
			appendAuthIndex(member)
		}
	}
	// Prefer subject when the index maps uniquely; otherwise expand known index groups.
	appendAuthIndexOrSubject := func(idx string) {
		idx = strings.TrimSpace(idx)
		if idx == "" {
			return
		}
		if subjectID := strings.TrimSpace(authSubjectByIndex[idx]); subjectID != "" {
			appendSubject(subjectID)
			return
		}
		if group := authIndexGroup[idx]; len(group) > 0 {
			for _, member := range group {
				if subjectID := strings.TrimSpace(authSubjectByIndex[member]); subjectID != "" {
					appendSubject(subjectID)
					continue
				}
				appendAuthIndex(member)
			}
			return
		}
		appendAuthIndex(idx)
	}
	appendChannelName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seenChannelName[key]; ok {
			return
		}
		seenChannelName[key] = struct{}{}
		channelNames = append(channelNames, name)
	}
	attachLegacyNames := func(idx string) {
		if legacyChannels := ambiguousAuthIndexChannelMap[idx]; len(legacyChannels) > 0 {
			authIndexChannelNames[idx] = append(authIndexChannelNames[idx], legacyChannels...)
		}
		for _, member := range authIndexGroup[idx] {
			if legacyChannels := ambiguousAuthIndexChannelMap[member]; len(legacyChannels) > 0 {
				authIndexChannelNames[member] = append(authIndexChannelNames[member], legacyChannels...)
			}
		}
		if subjectID := strings.TrimSpace(authSubjectByIndex[idx]); subjectID != "" {
			for _, member := range authIndexesBySubject[subjectID] {
				if legacyChannels := ambiguousAuthIndexChannelMap[member]; len(legacyChannels) > 0 {
					authIndexChannelNames[member] = append(authIndexChannelNames[member], legacyChannels...)
				}
			}
		}
	}

	for _, raw := range selectedRaw {
		// Account-level subject token from channel_options.value.
		if _, ok := authMetaBySubject[raw]; ok || looksLikeAuthSubjectID(raw) {
			appendSubject(raw)
			continue
		}
		// Prefer exact auth_index matches so multi-provider same-email accounts
		// filter independently when clients send auth_index as the value.
		if _, ok := authIndexChannelMap[raw]; ok {
			appendAuthIndexOrSubject(raw)
			attachLegacyNames(raw)
			continue
		}
		if _, ok := authMetaByIndex[raw]; ok {
			appendAuthIndexOrSubject(raw)
			continue
		}
		matchedAuthIndex := false
		for idx := range authIndexChannelMap {
			if strings.EqualFold(strings.TrimSpace(idx), raw) {
				appendAuthIndexOrSubject(idx)
				attachLegacyNames(idx)
				matchedAuthIndex = true
			}
		}
		if matchedAuthIndex {
			continue
		}
		for idx := range authMetaByIndex {
			if strings.EqualFold(strings.TrimSpace(idx), raw) {
				appendAuthIndexOrSubject(idx)
				matchedAuthIndex = true
			}
		}
		if matchedAuthIndex {
			continue
		}
		// Historical channel_options values are auth_index hashes even when the
		// auth file was later reindexed (file: vs id: seed) or deleted. Treat
		// stable auth_index tokens as AuthIndexes; never fall back to
		// channel_name matching for them (that path yields 0 rows).
		if looksLikeAuthIndex(raw) {
			appendAuthIndexOrSubject(raw)
			continue
		}
		// Legacy clients still send display labels / emails.
		appendChannelName(raw)
	}

	for raw, name := range channelNameMap {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		if _, ok := selectedLabelKeys[key]; ok {
			appendChannelName(raw)
		}
	}
	for idx, name := range authIndexChannelMap {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		if _, ok := selectedLabelKeys[key]; ok {
			appendAuthIndexOrSubject(idx)
			attachLegacyNames(idx)
		}
	}
	if len(authSubjectIDs) == 0 && len(authIndexes) == 0 && len(channelNames) == 0 {
		authIndexes = []string{""}
	}
	return authSubjectIDs, authIndexes, channelNames, authIndexChannelNames
}

// looksLikeAuthIndex reports whether value matches the stable auth index format
// produced by coreauth.stableAuthIndex (first 8 bytes of SHA-256 as 16 hex chars).
// Channel facet values use this token so historical rows remain filterable after
// live auth reindex or deletion.
func looksLikeAuthIndex(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 16 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// looksLikeAuthSubjectID reports whether value matches stableAuthSubjectID output.
func looksLikeAuthSubjectID(value string) bool {
	value = strings.TrimSpace(value)
	const prefix = "authsub_"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	hexPart := value[len(prefix):]
	if len(hexPart) != 16 {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func enrichChannelFilterOptions(
	options []usage.ChannelFilterOption,
	channelNameMap, authIndexChannelMap map[string]string,
	authMetaByIndex map[string]authChannelMeta,
	authIndexGroup map[string][]string,
	authSubjectByIndex map[string]string,
	authMetaBySubject map[string]authChannelMeta,
) []usage.ChannelFilterOption {
	if len(options) == 0 {
		return make([]usage.ChannelFilterOption, 0)
	}

	// Prefer one option per live auth_subject_id. When multiple facet rows share
	// one canonical auth_index but no live subject mapping, use the auth_index as
	// the option identity so selecting it matches every historical subject row.
	// Collapse pure name-only rows when the same label already has auth-backed options.
	out := make([]usage.ChannelFilterOption, 0, len(options))
	seenValue := make(map[string]struct{}, len(options))
	authBackedLabels := make(map[string]struct{})

	resolveSubject := func(option usage.ChannelFilterOption, authIndex string) string {
		// Live auth metadata is the canonical subject. Persisted request-log rows
		// may still carry an older subject ID after the identity seed changes.
		if subjectID := strings.TrimSpace(authSubjectByIndex[authIndex]); subjectID != "" {
			return subjectID
		}
		if subjectID := strings.TrimSpace(option.AuthSubjectID); subjectID != "" {
			return subjectID
		}
		if value := strings.TrimSpace(option.Value); looksLikeAuthSubjectID(value) {
			return value
		}
		return ""
	}

	facetIdentitiesByAuthIndex := channelFacetIdentitiesByAuthIndex(options, authIndexGroup)

	for _, option := range options {
		authIndex := strings.TrimSpace(option.AuthIndex)
		if authIndex == "" && !looksLikeAuthSubjectID(option.Value) {
			authIndex = strings.TrimSpace(option.Value)
		}
		authIndex = canonicalChannelAuthIndex(authIndex, authIndexGroup)
		subjectID := resolveSubject(option, authIndex)
		if subjectID != "" {
			if meta, ok := authMetaBySubject[subjectID]; ok && meta.label != "" {
				authBackedLabels[strings.ToLower(meta.label)] = struct{}{}
			}
		}
		if authIndex != "" {
			if name := strings.TrimSpace(authIndexChannelMap[authIndex]); name != "" {
				authBackedLabels[strings.ToLower(name)] = struct{}{}
			}
			if meta, ok := authMetaByIndex[authIndex]; ok && meta.label != "" {
				authBackedLabels[strings.ToLower(meta.label)] = struct{}{}
			}
		}
	}

	for _, option := range options {
		label := strings.TrimSpace(option.Label)
		authIndex := strings.TrimSpace(option.AuthIndex)
		if authIndex == "" && !looksLikeAuthSubjectID(option.Value) {
			authIndex = strings.TrimSpace(option.Value)
		}
		authIndex = canonicalChannelAuthIndex(authIndex, authIndexGroup)
		subjectID := resolveSubject(option, authIndex)

		if label == "" && authIndex != "" {
			if name, ok := authIndexChannelMap[authIndex]; ok && strings.TrimSpace(name) != "" {
				label = strings.TrimSpace(name)
			}
		}
		if label == "" && subjectID != "" {
			if meta, ok := authMetaBySubject[subjectID]; ok && meta.label != "" {
				label = meta.label
			}
		}
		if label == "" {
			if name, ok := channelNameMap[strings.TrimSpace(option.Value)]; ok && strings.TrimSpace(name) != "" {
				label = strings.TrimSpace(name)
			}
		}
		if label == "" {
			label = strings.TrimSpace(option.Value)
		}
		if label == "" {
			continue
		}

		provider := strings.TrimSpace(option.Provider)
		authType := strings.TrimSpace(option.AuthType)
		value := strings.TrimSpace(option.Value)
		hasLiveMeta := false

		// Without a live canonical subject, a repeated canonical auth_index is the
		// only selector that covers every historical subject produced by the facet.
		if authIndex != "" && strings.TrimSpace(authSubjectByIndex[authIndex]) == "" && len(facetIdentitiesByAuthIndex[authIndex]) > 1 {
			subjectID = ""
		}

		if subjectID != "" {
			value = subjectID
			if meta, ok := authMetaBySubject[subjectID]; ok {
				hasLiveMeta = true
				if meta.label != "" {
					label = meta.label
				}
				if meta.provider != "" {
					provider = meta.provider
				}
				if meta.authType != "" {
					authType = meta.authType
				}
			}
			if authIndex != "" {
				if meta, ok := authMetaByIndex[authIndex]; ok {
					if !hasLiveMeta {
						hasLiveMeta = true
					}
					if provider == "" && meta.provider != "" {
						provider = meta.provider
					}
					if authType == "" && meta.authType != "" {
						authType = meta.authType
					}
					if meta.label != "" && label == "" {
						label = meta.label
					}
				}
			}
		} else if meta, ok := authMetaByIndex[authIndex]; ok {
			hasLiveMeta = true
			if meta.label != "" {
				label = meta.label
			}
			if meta.provider != "" {
				provider = meta.provider
			}
			if meta.authType != "" {
				authType = meta.authType
			}
			value = authIndex
		} else if authIndex != "" {
			// Keep auth_index as the filter value even without live meta so
			// historical rows for deleted auths stay independently selectable.
			value = authIndex
			if mapped, ok := authIndexChannelMap[authIndex]; ok && strings.TrimSpace(mapped) != "" {
				label = strings.TrimSpace(mapped)
			}
		} else if mapped, ok := channelNameMap[label]; ok && strings.TrimSpace(mapped) != "" {
			label = strings.TrimSpace(mapped)
		}

		if value == "" {
			value = authIndex
		}
		if value == "" {
			value = label
		}

		// Historical / deleted credentials still need provider icon + OAuth/API badge.
		if provider == "" || authType == "" {
			inferredProvider, inferredAuthType := usage.InferChannelDisplayMeta(label, "", "", provider)
			if provider == "" {
				provider = inferredProvider
			}
			if authType == "" {
				authType = inferredAuthType
			}
		}

		// Drop name-only rows that would re-merge same-email multi-provider
		// accounts already represented by subject/index-backed options.
		if !hasLiveMeta && subjectID == "" && authIndex == "" {
			if _, ok := authBackedLabels[strings.ToLower(label)]; ok {
				continue
			}
		}

		dedupeKey := strings.ToLower(value)
		if _, ok := seenValue[dedupeKey]; ok {
			continue
		}
		seenValue[dedupeKey] = struct{}{}

		out = append(out, usage.ChannelFilterOption{
			Value:         value,
			Label:         label,
			Provider:      provider,
			AuthType:      normalizeAuthType(authType),
			AuthIndex:     authIndex,
			AuthSubjectID: subjectID,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		li := strings.ToLower(out[i].Label)
		lj := strings.ToLower(out[j].Label)
		if li != lj {
			return li < lj
		}
		pi := strings.ToLower(out[i].Provider)
		pj := strings.ToLower(out[j].Provider)
		if pi != pj {
			return pi < pj
		}
		return strings.ToLower(out[i].Value) < strings.ToLower(out[j].Value)
	})
	return out
}

func channelLabelsFromOptions(options []usage.ChannelFilterOption) []string {
	if len(options) == 0 {
		return make([]string, 0)
	}
	// Keep legacy channels as unique display labels (may collapse same-email providers).
	// New clients should use channel_options.
	seen := make(map[string]struct{}, len(options))
	labels := make([]string, 0, len(options))
	for _, option := range options {
		label := strings.TrimSpace(option.Label)
		if label == "" {
			label = strings.TrimSpace(option.Value)
		}
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func enrichLogRowChannelMeta(item *usage.LogRow, authMetaByIndex, authMetaBySubject map[string]authChannelMeta) {
	if item == nil {
		return
	}
	if meta, ok := authMetaBySubject[strings.TrimSpace(item.AuthSubjectID)]; ok {
		if meta.provider != "" {
			item.Provider = meta.provider
		}
		if meta.authType != "" {
			item.AuthType = normalizeAuthType(meta.authType)
		}
	} else if meta, ok := authMetaByIndex[strings.TrimSpace(item.AuthIndex)]; ok {
		if meta.provider != "" {
			item.Provider = meta.provider
		}
		if meta.authType != "" {
			item.AuthType = normalizeAuthType(meta.authType)
		}
	}
	if item.Provider == "" || item.AuthType == "" {
		provider, authType := usage.InferChannelDisplayMeta(item.ChannelName, item.Source, item.Model, item.Provider)
		if item.Provider == "" {
			item.Provider = provider
		}
		if item.AuthType == "" {
			item.AuthType = normalizeAuthType(authType)
		}
	}
}

func normalizeAuthType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "oauth":
		return "oauth"
	case "api", "api_key", "apikey":
		return "api"
	default:
		return ""
	}
}

func (s *Service) publicAPIKeyName(apiKey string) string {
	// Prefer any-tenant resolve first: public lookup already authenticated the secret,
	// and tenant-scoped GetRow can miss when service tenant is empty/stale in tests.
	if name := usage.ResolveAPIKeyOwnName(apiKey); name != "" {
		return name
	}
	row := apikeysettings.NewService(nil, apikeysettings.WithTenantID(s.tenantID)).GetRow(apiKey)
	if row == nil {
		return ""
	}
	return strings.TrimSpace(row.Name)
}

func displayChannelNameForLog(item usage.LogRow, channelNameMap, authIndexChannelMap map[string]string, ambiguousAuthIndexChannelMap map[string][]string) string {
	if channel := strings.TrimSpace(item.ChannelName); channel != "" {
		if name, ok := authIndexChannelMap[item.AuthIndex]; ok && strings.TrimSpace(name) != "" {
			if _, legacy := channelNameMap[channel]; legacy || containsFold(ambiguousAuthIndexChannelMap[item.AuthIndex], channel) {
				return strings.TrimSpace(name)
			}
		}
		if name, ok := channelNameMap[channel]; ok && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
		return channel
	}
	if name, ok := authIndexChannelMap[item.AuthIndex]; ok && strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	if name, ok := channelNameMap[item.Source]; ok {
		return strings.TrimSpace(name)
	}
	return ""
}

func (s *Service) buildNameMaps() nameMaps {
	maps := nameMaps{
		keyNameMap:                   make(map[string]string),
		channelNameMap:               make(map[string]string),
		authIndexChannelMap:          make(map[string]string),
		ambiguousAuthIndexChannelMap: make(map[string][]string),
		authMetaByIndex:              make(map[string]authChannelMeta),
		authIndexGroup:               make(map[string][]string),
		authSubjectByIndex:           make(map[string]string),
		authIndexesBySubject:         make(map[string][]string),
		authMetaBySubject:            make(map[string]authChannelMeta),
	}

	for _, row := range apikeysettings.NewService(nil, apikeysettings.WithTenantID(s.tenantID)).ListRows() {
		// Keyed by the stable id: log rows and facet options carry the id, and
		// the secret is not part of any response.
		id := strings.TrimSpace(row.ID)
		if id == "" {
			continue
		}
		// Owned multi-key accounts: filter/list labels are end-user display names,
		// never the individual key name (e.g. LYDing1 must not appear as a user).
		if eu := strings.TrimSpace(row.EndUserID); eu != "" {
			if label := usage.DisplayNameForEndUser(eu); label != "" {
				maps.keyNameMap[id] = label
				continue
			}
		}
		if name := strings.TrimSpace(row.Name); name != "" {
			maps.keyNameMap[id] = name
		}
	}

	cfg := s.cfg
	if cfg != nil {
		for _, k := range cfg.GeminiKey {
			if k.APIKey != "" && k.Name != "" {
				maps.channelNameMap[k.APIKey] = k.Name
			}
		}
		for _, k := range cfg.ClaudeKey {
			if k.APIKey != "" && k.Name != "" {
				maps.channelNameMap[k.APIKey] = k.Name
			}
		}
		for _, k := range cfg.CodexKey {
			if k.APIKey != "" && k.Name != "" {
				maps.channelNameMap[k.APIKey] = k.Name
			}
		}
		for _, provider := range cfg.OpenAICompatibility {
			if provider.Name == "" {
				continue
			}
			for _, entry := range provider.APIKeyEntries {
				if entry.APIKey != "" {
					maps.channelNameMap[entry.APIKey] = provider.Name
				}
			}
		}
	}

	type legacyChannelCandidate struct {
		key       string
		channel   string
		authIndex string
	}
	var legacyCandidates []legacyChannelCandidate

	if s.authManager != nil {
		for _, auth := range s.authManager.ListUsableForTenant(s.tenantID) { // incl. shared platform credentials
			if auth == nil {
				continue
			}
			channel := strings.TrimSpace(auth.ChannelName())
			if channel == "" {
				continue
			}
			auth.EnsureIndex()
			idx := strings.TrimSpace(auth.Index)
			meta := authChannelMeta{
				label:    channel,
				provider: normalizeProviderKey(auth.Provider),
				authType: resolveAuthType(auth),
			}
			subjectID := ""
			if identity := usage.ResolveAuthSubjectIdentity(auth); identity != nil {
				subjectID = strings.TrimSpace(identity.ID)
			}
			// xAI/Grok OAuth historically flipped between id: and file: seeds for
			// the same credential. Register the full index group so filters and
			// channel_options collapse onto one live option while still matching
			// historical rows. Also expand basename/tenant-relative file seeds for
			// all providers so path-format churn does not split one account.
			group := authIndexAliasGroup(auth)
			if len(group) == 0 && idx != "" {
				group = []string{idx}
			}
			for _, member := range group {
				member = strings.TrimSpace(member)
				if member == "" {
					continue
				}
				maps.authIndexChannelMap[member] = channel
				maps.authMetaByIndex[member] = meta
				maps.authIndexGroup[member] = group
				if subjectID != "" {
					maps.authSubjectByIndex[member] = subjectID
				}
			}
			if subjectID != "" {
				if _, ok := maps.authMetaBySubject[subjectID]; !ok || !auth.Disabled {
					// Prefer an enabled auth as the subject representative.
					maps.authMetaBySubject[subjectID] = meta
				}
				for _, member := range group {
					member = strings.TrimSpace(member)
					if member == "" {
						continue
					}
					maps.authIndexesBySubject[subjectID] = appendUniqueString(maps.authIndexesBySubject[subjectID], member)
				}
			}
			if accountType, account := auth.AccountInfo(); strings.EqualFold(accountType, "oauth") {
				if source := strings.TrimSpace(account); source != "" {
					legacyCandidates = append(legacyCandidates, legacyChannelCandidate{key: source, channel: channel, authIndex: idx})
				}
			}
			if email := strings.TrimSpace(managementauthfiles.Email(auth)); email != "" {
				legacyCandidates = append(legacyCandidates, legacyChannelCandidate{key: email, channel: channel, authIndex: idx})
			}
		}
	}

	legacyChannelsByKey := make(map[string]map[string]struct{})
	for _, candidate := range legacyCandidates {
		key := strings.TrimSpace(candidate.key)
		channel := strings.TrimSpace(candidate.channel)
		if key == "" || channel == "" {
			continue
		}
		if legacyChannelsByKey[key] == nil {
			legacyChannelsByKey[key] = make(map[string]struct{})
		}
		legacyChannelsByKey[key][strings.ToLower(channel)] = struct{}{}
	}
	for _, candidate := range legacyCandidates {
		key := strings.TrimSpace(candidate.key)
		if key == "" {
			continue
		}
		if len(legacyChannelsByKey[key]) > 1 {
			if candidate.authIndex != "" {
				maps.ambiguousAuthIndexChannelMap[candidate.authIndex] = append(maps.ambiguousAuthIndexChannelMap[candidate.authIndex], key)
			}
			continue
		}
		maps.channelNameMap[key] = strings.TrimSpace(candidate.channel)
	}

	return maps
}

func appendUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// authIndexAliasGroup returns the live EnsureIndex first, followed by known
// historical seeds for the same credential file. Path-format churn
// (basename vs tenant-relative FileName, id: vs file: seed) must not split one
// account across multiple channel options.
func authIndexAliasGroup(auth *coreauth.Auth) []string {
	if auth == nil {
		return nil
	}
	primary := strings.TrimSpace(auth.EnsureIndex())
	if primary == "" {
		return nil
	}

	seen := map[string]struct{}{primary: {}}
	out := []string{primary}
	addSeed := func(seed string) {
		seed = strings.TrimSpace(seed)
		if seed == "" {
			return
		}
		idx := authIndexFromSeed(seed)
		if idx == "" {
			return
		}
		if _, ok := seen[idx]; ok {
			return
		}
		seen[idx] = struct{}{}
		out = append(out, idx)
	}

	fileName := strings.TrimSpace(auth.FileName)
	if fileName != "" {
		base := filepath.Base(fileName)
		// Current and historical FileName forms.
		addSeed("file:" + fileName)
		addSeed("file:" + base)
		// Historical path: ID was set to the auth file name (including .json)
		// before FileName was populated, so EnsureIndex used id:<filename>.
		addSeed("id:" + fileName)
		addSeed("id:" + base)
		// Path-format churn: basename <-> tenant-relative.
		if tenantID := strings.TrimSpace(auth.TenantID); tenantID != "" && base != "" {
			addSeed("file:" + tenantID + "/" + base)
			addSeed("id:" + tenantID + "/" + base)
		}
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		addSeed("id:" + id)
		base := filepath.Base(id)
		addSeed("id:" + base)
		// If ID is a bare filename / tenant-relative path, also cover file: variants.
		if strings.HasSuffix(strings.ToLower(base), ".json") {
			addSeed("file:" + id)
			addSeed("file:" + base)
			if tenantID := strings.TrimSpace(auth.TenantID); tenantID != "" {
				addSeed("file:" + tenantID + "/" + base)
				addSeed("id:" + tenantID + "/" + base)
			}
		}
	}
	return out
}

// xaiOAuthAuthIndexGroup is kept for tests that assert the historical xAI
// id:/file: seed merge; production uses authIndexAliasGroup for all providers.
func xaiOAuthAuthIndexGroup(auth *coreauth.Auth) []string {
	return authIndexAliasGroup(auth)
}

func isXAIProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "xai", "grok":
		return true
	default:
		return false
	}
}

// authIndexFromSeed mirrors coreauth.stableAuthIndex (sha256 first 8 bytes hex).
func authIndexFromSeed(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

func resolveAuthType(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	accountType, _ := auth.AccountInfo()
	return normalizeAuthType(accountType)
}

func normalizeProviderKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func containsFold(values []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), needle) {
			return true
		}
	}
	return false
}

func IntQueryDefault(raw string, def int) int {
	v := strings.TrimSpace(raw)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return n
}
