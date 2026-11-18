// Package config hosts SDK bridge aliases for server configuration types.
package config

import internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"

type SDKConfig = internalconfig.SDKConfig
type ClaudeCodeConfig = internalconfig.ClaudeCodeConfig
type DisableImageGenerationMode = internalconfig.DisableImageGenerationMode
type ClientConfig = internalconfig.ClientConfig
type CodexClientConfig = internalconfig.CodexClientConfig
type ClaudeClientConfig = internalconfig.ClaudeClientConfig

type Config = internalconfig.Config
type APIKeyEntry = internalconfig.APIKeyEntry
type ModelCatalogs = internalconfig.ModelCatalogs

type StreamingConfig = internalconfig.StreamingConfig
type ResponsesWebsocketConfig = internalconfig.ResponsesWebsocketConfig
type RoutingConfig = internalconfig.RoutingConfig
type RoutingChannelGroup = internalconfig.RoutingChannelGroup
type RoutingPathRoute = internalconfig.RoutingPathRoute
type ChannelGroupMatch = internalconfig.ChannelGroupMatch
type GroupScheduling = internalconfig.GroupScheduling
type GroupStickyConfig = internalconfig.GroupStickyConfig
type TLSConfig = internalconfig.TLSConfig
type PprofConfig = internalconfig.PprofConfig
type RemoteManagement = internalconfig.RemoteManagement
type AmpCode = internalconfig.AmpCode
type OAuthModelAlias = internalconfig.OAuthModelAlias
type PayloadConfig = internalconfig.PayloadConfig
type PayloadRule = internalconfig.PayloadRule
type PayloadFilterRule = internalconfig.PayloadFilterRule
type PayloadModelRule = internalconfig.PayloadModelRule
type PayloadReplaceRule = internalconfig.PayloadReplaceRule
type PayloadReplaceTarget = internalconfig.PayloadReplaceTarget
type PayloadReplacement = internalconfig.PayloadReplacement

type GeminiKey = internalconfig.GeminiKey
type CodexKey = internalconfig.CodexKey
type ClaudeKey = internalconfig.ClaudeKey
type BedrockKey = internalconfig.BedrockKey
type XAIKey = internalconfig.XAIKey
type XAIModel = internalconfig.XAIModel
type MetaKey = internalconfig.MetaKey
type BedrockModel = internalconfig.BedrockModel
type OpenCodeGoKey = internalconfig.OpenCodeGoKey
type OpenCodeGoModel = internalconfig.OpenCodeGoModel
type ClineKey = internalconfig.ClineKey
type ClineModel = internalconfig.ClineModel
type OllamaCloudKey = internalconfig.OllamaCloudKey
type OllamaCloudModel = internalconfig.OllamaCloudModel
type CommandCodeKey = internalconfig.CommandCodeKey
type CommandCodeModel = internalconfig.CommandCodeModel
type VertexCompatKey = internalconfig.VertexCompatKey
type VertexCompatModel = internalconfig.VertexCompatModel
type OpenAICompatibility = internalconfig.OpenAICompatibility
type OpenAICompatibilityAPIKey = internalconfig.OpenAICompatibilityAPIKey
type OpenAICompatibilityModel = internalconfig.OpenAICompatibilityModel
type ProxyConnectionPoolConfig = internalconfig.ProxyConnectionPoolConfig
type OAuthModelSetting = internalconfig.OAuthModelSetting

type TLS = internalconfig.TLSConfig

const (
	DefaultPanelGitHubRepository = internalconfig.DefaultPanelGitHubRepository
	DefaultBedrockRegion         = internalconfig.DefaultBedrockRegion
	DefaultClineBaseURL          = internalconfig.DefaultClineBaseURL
	DefaultOllamaCloudBaseURL    = internalconfig.DefaultOllamaCloudBaseURL
	DefaultCommandCodeBaseURL    = internalconfig.DefaultCommandCodeBaseURL
	DefaultPprofAddr             = internalconfig.DefaultPprofAddr

	DistributionWeighted     = internalconfig.DistributionWeighted
	DistributionLeastLoad    = internalconfig.DistributionLeastLoad
	DistributionFillFirst    = internalconfig.DistributionFillFirst
	DistributionResetAware   = internalconfig.DistributionResetAware
	DefaultStickyMaxRequests = internalconfig.DefaultStickyMaxRequests
)

func NormalizeDistribution(value string) string {
	return internalconfig.NormalizeDistribution(value)
}

func MigrateLegacyPriorities(priorities map[string]int) map[string]int {
	return internalconfig.MigrateLegacyPriorities(priorities)
}

func LoadConfig(configFile string) (*Config, error) { return internalconfig.LoadConfig(configFile) }

func ParseConfigBytes(data []byte) (*Config, error) { return internalconfig.ParseConfigBytes(data) }

func ResolveOAuthModelSetting(settings []OAuthModelSetting, modelID, metadataModelID, modelName string) *OAuthModelSetting {
	return internalconfig.ResolveOAuthModelSetting(settings, modelID, metadataModelID, modelName)
}

func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	return internalconfig.LoadConfigOptional(configFile, optional)
}

func SaveConfigPreserveComments(configFile string, cfg *Config) error {
	return internalconfig.SaveConfigPreserveComments(configFile, cfg)
}

func SaveConfigPreserveCommentsUpdateNestedScalar(configFile string, path []string, value string) error {
	return internalconfig.SaveConfigPreserveCommentsUpdateNestedScalar(configFile, path, value)
}

func NormalizeCommentIndentation(data []byte) []byte {
	return internalconfig.NormalizeCommentIndentation(data)
}

func NormalizeRoutingStrategy(strategy string) string {
	return internalconfig.NormalizeRoutingStrategy(strategy)
}
