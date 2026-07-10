package identityfingerprint

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func ResolveClaude(cfg config.ClaudeIdentityFingerprintConfig, learned *LearnedRecord) (config.ClaudeIdentityFingerprintConfig, EffectiveFingerprint) {
	clean := config.CleanClaudeIdentityFingerprint(cfg)
	defaults := config.DefaultClaudeIdentityFingerprint()
	fields := make(map[string]FieldValue)
	pick := func(field, preset, learnedValue, defaultValue string) string {
		value, source := pickField(preset, learnedValue, defaultValue)
		fields[field] = FieldValue{Value: value, Source: source}
		return value
	}

	cliVersion := pick(FieldClaudeCLIVersion, clean.CLIVersion, learnedFieldOrVersion(learned, FieldClaudeCLIVersion), defaults.CLIVersion)
	entrypoint := pick(FieldClaudeEntrypoint, clean.Entrypoint, learnedField(learned, FieldClaudeEntrypoint), defaults.Entrypoint)
	userAgentDefault := config.BuildClaudeFingerprintUserAgent(cliVersion, entrypoint)
	userAgent := pick(FieldUserAgent, clean.UserAgent, learnedField(learned, FieldUserAgent), userAgentDefault)
	anthropicBeta := pick(FieldClaudeAnthropicBeta, clean.AnthropicBeta, learnedField(learned, FieldClaudeAnthropicBeta), defaults.AnthropicBeta)
	stainlessPackage := pick(FieldClaudeStainlessPackage, clean.StainlessPackageVersion, learnedField(learned, FieldClaudeStainlessPackage), defaults.StainlessPackageVersion)
	stainlessRuntime := pick(FieldClaudeStainlessRuntime, clean.StainlessRuntimeVersion, learnedField(learned, FieldClaudeStainlessRuntime), defaults.StainlessRuntimeVersion)
	stainlessTimeout := pick(FieldClaudeStainlessTimeout, clean.StainlessTimeout, learnedField(learned, FieldClaudeStainlessTimeout), defaults.StainlessTimeout)

	sessionMode := clean.SessionMode
	if sessionMode == "" {
		sessionMode = defaults.SessionMode
	}
	resolved := config.ClaudeIdentityFingerprintConfig{
		Enabled:                 clean.Enabled,
		CLIVersion:              cliVersion,
		Entrypoint:              entrypoint,
		UserAgent:               userAgent,
		AnthropicBeta:           anthropicBeta,
		StainlessPackageVersion: stainlessPackage,
		StainlessRuntimeVersion: stainlessRuntime,
		StainlessTimeout:        stainlessTimeout,
		SessionMode:             sessionMode,
		SessionID:               clean.SessionID,
		DeviceID:                clean.DeviceID,
		CustomHeaders:           clean.CustomHeaders,
	}
	effective := effective(ProviderClaude, clean.Enabled, learned, fields)
	effective.Version = cliVersion
	return resolved, effective
}

func ResolveCodex(cfg config.CodexIdentityFingerprintConfig, learned *LearnedRecord) (config.CodexIdentityFingerprintConfig, EffectiveFingerprint) {
	clean := config.CleanCodexIdentityFingerprint(cfg)
	defaults := config.DefaultCodexIdentityFingerprint()
	fields := make(map[string]FieldValue)
	pick := func(field, preset, learnedValue, defaultValue string) string {
		value, source := pickField(preset, learnedValue, defaultValue)
		fields[field] = FieldValue{Value: value, Source: source}
		return value
	}

	userAgent := pick(FieldUserAgent, clean.UserAgent, learnedField(learned, FieldUserAgent), defaults.UserAgent)
	version := pick(FieldCodexVersion, clean.Version, learnedFieldOrVersion(learned, FieldCodexVersion), defaults.Version)
	originator := pick(FieldCodexOriginator, clean.Originator, learnedField(learned, FieldCodexOriginator), defaults.Originator)
	websocketBeta := pick(FieldCodexWebsocketBeta, clean.WebsocketBeta, learnedField(learned, FieldCodexWebsocketBeta), defaults.WebsocketBeta)
	betaFeatures := pick(FieldCodexBetaFeatures, clean.BetaFeatures, learnedField(learned, FieldCodexBetaFeatures), defaults.BetaFeatures)

	sessionMode := clean.SessionMode
	if sessionMode == "" {
		sessionMode = defaults.SessionMode
	}
	resolved := config.CodexIdentityFingerprintConfig{
		Enabled:       clean.Enabled,
		UserAgent:     userAgent,
		Version:       version,
		Originator:    originator,
		WebsocketBeta: websocketBeta,
		BetaFeatures:  betaFeatures,
		SessionMode:   sessionMode,
		SessionID:     clean.SessionID,
		CustomHeaders: clean.CustomHeaders,
	}
	effective := effective(ProviderCodex, clean.Enabled, learned, fields)
	effective.Version = version
	return resolved, effective
}

func ResolveGemini(cfg config.GeminiIdentityFingerprintConfig, learned *LearnedRecord) (config.GeminiIdentityFingerprintConfig, EffectiveFingerprint) {
	clean := config.CleanGeminiIdentityFingerprint(cfg)
	defaults := config.DefaultGeminiIdentityFingerprint()
	fields := make(map[string]FieldValue)
	pick := func(field, preset, learnedValue, defaultValue string) string {
		value, source := pickField(preset, learnedValue, defaultValue)
		fields[field] = FieldValue{Value: value, Source: source}
		return value
	}

	resolved := config.GeminiIdentityFingerprintConfig{
		Enabled:        clean.Enabled,
		UserAgent:      pick(FieldUserAgent, clean.UserAgent, learnedField(learned, FieldUserAgent), defaults.UserAgent),
		APIClient:      pick(FieldGeminiAPIClient, clean.APIClient, learnedField(learned, FieldGeminiAPIClient), defaults.APIClient),
		ClientMetadata: pick(FieldGeminiClientMetadata, clean.ClientMetadata, learnedField(learned, FieldGeminiClientMetadata), defaults.ClientMetadata),
		CustomHeaders:  clean.CustomHeaders,
	}
	return resolved, effective(ProviderGemini, clean.Enabled, learned, fields)
}

func ResolveXAI(cfg config.XAIIdentityFingerprintConfig, learned *LearnedRecord) (config.XAIIdentityFingerprintConfig, EffectiveFingerprint) {
	clean := config.CleanXAIIdentityFingerprint(cfg)
	defaults := config.DefaultXAIIdentityFingerprint()
	fields := make(map[string]FieldValue)
	pick := func(field, preset, learnedValue, defaultValue string) string {
		value, source := pickField(preset, learnedValue, defaultValue)
		fields[field] = FieldValue{Value: value, Source: source}
		return value
	}

	resolved := config.XAIIdentityFingerprintConfig{
		Enabled:            clean.Enabled,
		UserAgent:          pick(FieldUserAgent, clean.UserAgent, learnedField(learned, FieldUserAgent), defaults.UserAgent),
		ClientIdentifier:   pick(FieldXAIClientIdentifier, clean.ClientIdentifier, learnedField(learned, FieldXAIClientIdentifier), defaults.ClientIdentifier),
		ClientVersion:      pick(FieldXAIClientVersion, clean.ClientVersion, learnedFieldOrVersion(learned, FieldXAIClientVersion), defaults.ClientVersion),
		GrokConversationID: strings.TrimSpace(clean.GrokConversationID),
		CustomHeaders:      clean.CustomHeaders,
	}
	effective := effective(ProviderXAI, clean.Enabled, learned, fields)
	if value := strings.TrimSpace(resolved.ClientVersion); value != "" {
		effective.Version = value
	}
	return resolved, effective
}

func pickField(preset, learned, fallback string) (string, FieldSource) {
	if value := strings.TrimSpace(learned); value != "" {
		return value, FieldSourceLearned
	}
	if value := strings.TrimSpace(preset); value != "" {
		return value, FieldSourcePreset
	}
	return strings.TrimSpace(fallback), FieldSourceBuiltinDefault
}

func learnedField(record *LearnedRecord, field string) string {
	if record == nil || len(record.Fields) == 0 {
		return ""
	}
	return strings.TrimSpace(record.Fields[field])
}

func learnedFieldOrVersion(record *LearnedRecord, field string) string {
	if value := learnedField(record, field); value != "" {
		return value
	}
	if record == nil {
		return ""
	}
	return strings.TrimSpace(record.Version)
}

func effective(provider Provider, enabled bool, learned *LearnedRecord, fields map[string]FieldValue) EffectiveFingerprint {
	out := EffectiveFingerprint{
		Provider: provider,
		Enabled:  enabled,
		Fields:   fields,
	}
	if learned != nil {
		out.AccountKey = learned.AccountKey
		out.AuthSubjectID = learned.AuthSubjectID
		out.ClientProduct = learned.ClientProduct
		out.Version = learned.Version
		out.Learned = cloneRecord(learned)
	}
	return out
}
