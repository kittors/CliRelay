package identityfingerprint

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	FieldUserAgent              = "user-agent"
	FieldClaudeCLIVersion       = "cli-version"
	FieldClaudeEntrypoint       = "entrypoint"
	FieldClaudeAnthropicBeta    = "anthropic-beta"
	FieldClaudeStainlessPackage = "stainless-package-version"
	FieldClaudeStainlessRuntime = "stainless-runtime-version"
	FieldClaudeStainlessTimeout = "stainless-timeout"
	FieldCodexVersion           = "version"
	FieldCodexOriginator        = "originator"
	FieldCodexWebsocketBeta     = "websocket-beta"
	FieldCodexBetaFeatures      = "x-codex-beta-features"
	FieldGeminiAPIClient        = "x-goog-api-client"
	FieldGeminiClientMetadata   = "client-metadata"
	FieldXAIGrokConversationID  = "x-grok-conv-id"
	FieldXAIClientIdentifier    = "x-grok-client-identifier"
	FieldXAIClientVersion       = "x-grok-client-version"
)

var (
	claudeUARe       = regexp.MustCompile(`(?i)^claude-cli/([0-9]+(?:\.[0-9]+){0,3})\s+\(external,\s*([^)]+)\)`)
	codexDesktopUARe = regexp.MustCompile(`(?i)\bcodex\s+desktop/([0-9]+(?:\.[0-9]+){0,3})(?:[-+][a-z0-9_.-]+)?`)
	tokenVerRe       = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*codex[a-z0-9_.-]*)/([0-9]+(?:\.[0-9]+){0,3})(?:[-+][a-z0-9_.-]+)?`)
	geminiUARe       = regexp.MustCompile(`(?i)^google-api-nodejs-client/([0-9]+(?:\.[0-9]+){0,3})`)
	xaiTokenVerRe    = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:grok|xai)[a-z0-9_.-]*)/([0-9]+(?:\.[0-9]+){0,3})(?:[-+][a-z0-9_.-]+)?`)
)

func ExtractObservation(input LearnInput) (Observation, bool) {
	observedAt := input.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	headers := http.Header(input.Headers)
	switch input.Provider {
	case ProviderClaude:
		return extractClaudeObservation(input, headers, observedAt)
	case ProviderCodex:
		return extractCodexObservation(input, headers, observedAt)
	case ProviderGemini:
		return extractGeminiObservation(input, headers, observedAt)
	case ProviderXAI:
		return extractXAIObservation(input, headers, observedAt)
	default:
		return Observation{}, false
	}
}

func extractClaudeObservation(input LearnInput, headers http.Header, observedAt time.Time) (Observation, bool) {
	ua := strings.TrimSpace(headers.Get("User-Agent"))
	matches := claudeUARe.FindStringSubmatch(ua)
	if len(matches) == 0 {
		return Observation{}, false
	}
	entrypoint := strings.TrimSpace(headers.Get("X-App"))
	if entrypoint == "" {
		entrypoint = strings.TrimSpace(matches[2])
	}
	fields := map[string]string{
		FieldUserAgent:        ua,
		FieldClaudeCLIVersion: strings.TrimSpace(matches[1]),
		FieldClaudeEntrypoint: entrypoint,
	}
	addHeaderField(fields, headers, "Anthropic-Beta", FieldClaudeAnthropicBeta)
	addHeaderField(fields, headers, "X-Stainless-Package-Version", FieldClaudeStainlessPackage)
	addHeaderField(fields, headers, "X-Stainless-Runtime-Version", FieldClaudeStainlessRuntime)
	addHeaderField(fields, headers, "X-Stainless-Timeout", FieldClaudeStainlessTimeout)
	return Observation{
		Provider:        ProviderClaude,
		AccountKey:      strings.TrimSpace(input.AccountKey),
		ProfileKey:      ProfileKeyDefault,
		AuthSubjectID:   strings.TrimSpace(input.AuthSubjectID),
		ClientProduct:   "claude-cli",
		ClientVariant:   entrypoint,
		Version:         strings.TrimSpace(matches[1]),
		Fields:          fields,
		ObservedHeaders: observedHeaders(headers, []string{"User-Agent", "X-App", "Anthropic-Beta", "X-Stainless-Package-Version", "X-Stainless-Runtime-Version", "X-Stainless-Timeout"}),
		ObservedAt:      observedAt.UTC(),
	}, true
}

func extractCodexObservation(input LearnInput, headers http.Header, observedAt time.Time) (Observation, bool) {
	ua := strings.TrimSpace(headers.Get("User-Agent"))
	originator := strings.TrimSpace(headers.Get("Originator"))
	product, uaVersion := codexProductVersion(ua)
	profileKey, profileFamily, profileOK := CodexProfileKey(ua, originator)
	if !profileOK {
		return Observation{}, false
	}
	if product == "" {
		product = profileKey
	}
	version := strings.TrimSpace(headers.Get("Version"))
	if version == "" {
		version = uaVersion
	}
	if product == "" {
		return Observation{}, false
	}
	fields := map[string]string{}
	if ua != "" {
		fields[FieldUserAgent] = ua
	}
	if version != "" {
		fields[FieldCodexVersion] = version
	}
	if originator != "" {
		fields[FieldCodexOriginator] = originator
	}
	if beta := strings.TrimSpace(headers.Get("OpenAI-Beta")); beta != "" && strings.Contains(beta, "responses_websockets=") {
		fields[FieldCodexWebsocketBeta] = beta
	}
	addHeaderField(fields, headers, "X-Codex-Beta-Features", FieldCodexBetaFeatures)
	if len(fields) == 0 {
		return Observation{}, false
	}
	return Observation{
		Provider:        ProviderCodex,
		AccountKey:      strings.TrimSpace(input.AccountKey),
		ProfileKey:      profileKey,
		ProfileFamily:   profileFamily,
		AuthSubjectID:   strings.TrimSpace(input.AuthSubjectID),
		ClientProduct:   product,
		ClientVariant:   originator,
		Version:         version,
		Fields:          fields,
		ObservedHeaders: observedHeaders(headers, []string{"User-Agent", "Version", "Originator", "OpenAI-Beta", "X-Codex-Beta-Features"}),
		ObservedAt:      observedAt.UTC(),
	}, true
}

func extractGeminiObservation(input LearnInput, headers http.Header, observedAt time.Time) (Observation, bool) {
	ua := strings.TrimSpace(headers.Get("User-Agent"))
	apiClient := strings.TrimSpace(headers.Get("X-Goog-Api-Client"))
	clientMetadata := strings.TrimSpace(headers.Get("Client-Metadata"))
	product := ""
	version := ""
	if matches := geminiUARe.FindStringSubmatch(ua); len(matches) > 0 {
		product = "google-api-nodejs-client"
		version = matches[1]
	}
	if product == "" && (strings.Contains(strings.ToLower(apiClient), "gl-node") || strings.Contains(strings.ToLower(clientMetadata), "gemini")) {
		product = "gemini-cli"
	}
	if product == "" {
		return Observation{}, false
	}
	fields := map[string]string{}
	if ua != "" {
		fields[FieldUserAgent] = ua
	}
	if apiClient != "" {
		fields[FieldGeminiAPIClient] = apiClient
	}
	if clientMetadata != "" {
		fields[FieldGeminiClientMetadata] = clientMetadata
	}
	if len(fields) == 0 {
		return Observation{}, false
	}
	return Observation{
		Provider:        ProviderGemini,
		AccountKey:      strings.TrimSpace(input.AccountKey),
		ProfileKey:      ProfileKeyDefault,
		AuthSubjectID:   strings.TrimSpace(input.AuthSubjectID),
		ClientProduct:   product,
		ClientVariant:   "cli",
		Version:         version,
		Fields:          fields,
		ObservedHeaders: observedHeaders(headers, []string{"User-Agent", "X-Goog-Api-Client", "Client-Metadata"}),
		ObservedAt:      observedAt.UTC(),
	}, true
}

func extractXAIObservation(input LearnInput, headers http.Header, observedAt time.Time) (Observation, bool) {
	ua := strings.TrimSpace(headers.Get("User-Agent"))
	clientIdentifier := strings.TrimSpace(headers.Get("X-Grok-Client-Identifier"))
	clientVersion := strings.TrimSpace(headers.Get("X-Grok-Client-Version"))
	product, version := xaiProductVersion(ua)
	if product == "" && clientIdentifier != "" {
		product = clientIdentifier
	}
	if version == "" {
		version = clientVersion
	}
	if product == "" {
		return Observation{}, false
	}
	fields := map[string]string{}
	if ua != "" {
		fields[FieldUserAgent] = ua
	}
	if clientIdentifier != "" {
		fields[FieldXAIClientIdentifier] = clientIdentifier
	}
	if clientVersion != "" {
		fields[FieldXAIClientVersion] = clientVersion
	}
	if len(fields) == 0 {
		return Observation{}, false
	}
	return Observation{
		Provider:        ProviderXAI,
		AccountKey:      strings.TrimSpace(input.AccountKey),
		ProfileKey:      ProfileKeyDefault,
		AuthSubjectID:   strings.TrimSpace(input.AuthSubjectID),
		ClientProduct:   product,
		ClientVariant:   "cli",
		Version:         version,
		Fields:          fields,
		ObservedHeaders: observedHeaders(headers, []string{"User-Agent", "X-Grok-Client-Identifier", "X-Grok-Client-Version"}),
		ObservedAt:      observedAt.UTC(),
	}, true
}

func MergeObservation(existing *LearnedRecord, obs Observation) MergeResult {
	if strings.TrimSpace(obs.AccountKey) == "" {
		return MergeResult{Reason: "missing_account_key"}
	}
	if len(obs.Fields) == 0 {
		return MergeResult{Record: existing, Reason: "empty_observation"}
	}
	now := obs.ObservedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if existing == nil {
		profileKey := strings.TrimSpace(obs.ProfileKey)
		if profileKey == "" {
			profileKey = DefaultProfileKey(obs.Provider)
		}
		record := &LearnedRecord{
			Provider:        obs.Provider,
			AccountKey:      strings.TrimSpace(obs.AccountKey),
			ProfileKey:      profileKey,
			ProfileFamily:   strings.TrimSpace(obs.ProfileFamily),
			AuthSubjectID:   strings.TrimSpace(obs.AuthSubjectID),
			ClientProduct:   strings.TrimSpace(obs.ClientProduct),
			ClientVariant:   strings.TrimSpace(obs.ClientVariant),
			Version:         strings.TrimSpace(obs.Version),
			Fields:          cloneStringMap(obs.Fields),
			ObservedHeaders: cloneStringMap(obs.ObservedHeaders),
			CreatedAt:       now.UTC(),
			UpdatedAt:       now.UTC(),
			LastSeenAt:      now.UTC(),
		}
		return MergeResult{Record: record, Changed: true, Reason: "created"}
	}

	record := cloneRecord(existing)
	profileKey := strings.TrimSpace(obs.ProfileKey)
	if profileKey == "" {
		profileKey = DefaultProfileKey(obs.Provider)
	}
	if existingKey := strings.TrimSpace(record.ProfileKey); existingKey != "" && existingKey != profileKey {
		return MergeResult{Record: existing, Reason: "different_profile"}
	}
	record.ProfileKey = profileKey
	if strings.TrimSpace(obs.ProfileFamily) != "" {
		record.ProfileFamily = strings.TrimSpace(obs.ProfileFamily)
	}
	record.LastSeenAt = now.UTC()
	if strings.TrimSpace(record.AuthSubjectID) == "" {
		record.AuthSubjectID = strings.TrimSpace(obs.AuthSubjectID)
	}
	if strings.TrimSpace(record.Version) != "" && strings.TrimSpace(obs.Version) != "" &&
		isNewerVersion(record.Version, obs.Version) {
		return MergeResult{Record: record, Changed: true, Reason: "older_version_last_seen"}
	}
	if strings.TrimSpace(obs.ClientProduct) != "" {
		record.ClientProduct = strings.TrimSpace(obs.ClientProduct)
	}
	if strings.TrimSpace(obs.Version) != "" {
		record.Version = strings.TrimSpace(obs.Version)
	}
	if strings.TrimSpace(obs.ClientVariant) != "" {
		record.ClientVariant = strings.TrimSpace(obs.ClientVariant)
	}
	if record.Fields == nil {
		record.Fields = map[string]string{}
	}
	for key, value := range obs.Fields {
		if key = strings.TrimSpace(key); key != "" {
			if value = strings.TrimSpace(value); value != "" {
				record.Fields[key] = value
			}
		}
	}
	record.ObservedHeaders = cloneStringMap(obs.ObservedHeaders)
	record.UpdatedAt = now.UTC()
	return MergeResult{Record: record, Changed: true, Reason: "merged_profile"}
}

func MergeObservationChangedExceptLastSeen(existing, merged *LearnedRecord) bool {
	if existing == nil || merged == nil {
		return existing != merged
	}
	if existing.Provider != merged.Provider ||
		strings.TrimSpace(existing.AccountKey) != strings.TrimSpace(merged.AccountKey) ||
		strings.TrimSpace(existing.ProfileKey) != strings.TrimSpace(merged.ProfileKey) ||
		strings.TrimSpace(existing.ProfileFamily) != strings.TrimSpace(merged.ProfileFamily) ||
		strings.TrimSpace(existing.AuthSubjectID) != strings.TrimSpace(merged.AuthSubjectID) ||
		strings.TrimSpace(existing.ClientProduct) != strings.TrimSpace(merged.ClientProduct) ||
		strings.TrimSpace(existing.ClientVariant) != strings.TrimSpace(merged.ClientVariant) ||
		strings.TrimSpace(existing.Version) != strings.TrimSpace(merged.Version) ||
		!sameStringMap(existing.Fields, merged.Fields) ||
		!sameStringMap(existing.ObservedHeaders, merged.ObservedHeaders) {
		return true
	}
	return false
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		if right[key] != leftValue {
			return false
		}
	}
	return true
}

func codexProductVersion(ua string) (string, string) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "", ""
	}

	if matches := codexDesktopUARe.FindStringSubmatch(ua); len(matches) > 0 {
		return "codex", matches[1]
	}

	if matches := tokenVerRe.FindStringSubmatch(ua); len(matches) > 0 {
		return strings.ToLower(matches[1]), matches[2]
	}

	if strings.Contains(strings.ToLower(ua), "codex") {
		return "codex", ""
	}
	return "", ""
}

func xaiProductVersion(ua string) (string, string) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "", ""
	}
	if matches := xaiTokenVerRe.FindStringSubmatch(ua); len(matches) > 0 {
		return strings.ToLower(matches[1]), matches[2]
	}
	lower := strings.ToLower(ua)
	if strings.Contains(lower, "grok") {
		return "grok-cli", ""
	}
	if strings.Contains(lower, "xai") {
		return "xai-cli", ""
	}
	return "", ""
}

func addHeaderField(fields map[string]string, headers http.Header, headerName, fieldName string) {
	if value := strings.TrimSpace(headers.Get(headerName)); value != "" {
		fields[fieldName] = value
	}
}

func observedHeaders(headers http.Header, names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			out[http.CanonicalHeaderKey(name)] = value
		}
	}
	return out
}

func isNewerVersion(next, current string) bool {
	nextParts := parseVersionParts(next)
	currentParts := parseVersionParts(current)
	if len(nextParts) == 0 || len(currentParts) == 0 {
		return false
	}
	max := len(nextParts)
	if len(currentParts) > max {
		max = len(currentParts)
	}
	for i := 0; i < max; i++ {
		var a, b int
		if i < len(nextParts) {
			a = nextParts[i]
		}
		if i < len(currentParts) {
			b = currentParts[i]
		}
		if a > b {
			return true
		}
		if a < b {
			return false
		}
	}
	return false
}

func parseVersionParts(version string) []int {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil
	}
	chunks := strings.Split(version, ".")
	parts := make([]int, 0, len(chunks))
	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			return nil
		}
		value, err := strconv.Atoi(chunk)
		if err != nil {
			return nil
		}
		parts = append(parts, value)
	}
	return parts
}

func cloneRecord(record *LearnedRecord) *LearnedRecord {
	if record == nil {
		return nil
	}
	out := *record
	out.Fields = cloneStringMap(record.Fields)
	out.ObservedHeaders = cloneStringMap(record.ObservedHeaders)
	return &out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			out[key] = value
		}
	}
	return out
}
