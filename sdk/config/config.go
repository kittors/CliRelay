// Package config provides the public SDK configuration API.
//
// It re-exports the server configuration types and helpers so external projects can
// embed CLIProxyAPI without importing internal packages.
package config

import bridgeconfig "github.com/router-for-me/CLIProxyAPI/v8/sdkbridge/config"

type SDKConfig = bridgeconfig.SDKConfig
type ClaudeCodeConfig = bridgeconfig.ClaudeCodeConfig
type ClientConfig = bridgeconfig.ClientConfig
type CodexClientConfig = bridgeconfig.CodexClientConfig
type ClaudeClientConfig = bridgeconfig.ClaudeClientConfig
type DisableImageGenerationMode = bridgeconfig.DisableImageGenerationMode

type Config = bridgeconfig.Config
type APIKeyEntry = bridgeconfig.APIKeyEntry
type ModelCatalogs = bridgeconfig.ModelCatalogs

type StreamingConfig = bridgeconfig.StreamingConfig
type ResponsesWebsocketConfig = bridgeconfig.ResponsesWebsocketConfig
type RoutingConfig = bridgeconfig.RoutingConfig
type RoutingChannelGroup = bridgeconfig.RoutingChannelGroup
type RoutingPathRoute = bridgeconfig.RoutingPathRoute
type ChannelGroupMatch = bridgeconfig.ChannelGroupMatch
type GroupScheduling = bridgeconfig.GroupScheduling
type GroupStickyConfig = bridgeconfig.GroupStickyConfig
type TLSConfig = bridgeconfig.TLSConfig
type PprofConfig = bridgeconfig.PprofConfig
type RemoteManagement = bridgeconfig.RemoteManagement
type AmpCode = bridgeconfig.AmpCode
type OAuthModelAlias = bridgeconfig.OAuthModelAlias
type PayloadConfig = bridgeconfig.PayloadConfig
type PayloadRule = bridgeconfig.PayloadRule
type PayloadFilterRule = bridgeconfig.PayloadFilterRule
type PayloadModelRule = bridgeconfig.PayloadModelRule
type PayloadReplaceRule = bridgeconfig.PayloadReplaceRule
type PayloadReplaceTarget = bridgeconfig.PayloadReplaceTarget
type PayloadReplacement = bridgeconfig.PayloadReplacement

type GeminiKey = bridgeconfig.GeminiKey
type CodexKey = bridgeconfig.CodexKey
type ClaudeKey = bridgeconfig.ClaudeKey
type BedrockKey = bridgeconfig.BedrockKey
type XAIKey = bridgeconfig.XAIKey
type XAIModel = bridgeconfig.XAIModel
type MetaKey = bridgeconfig.MetaKey
type BedrockModel = bridgeconfig.BedrockModel
type OpenCodeGoKey = bridgeconfig.OpenCodeGoKey
type OpenCodeGoModel = bridgeconfig.OpenCodeGoModel
type ClineKey = bridgeconfig.ClineKey
type ClineModel = bridgeconfig.ClineModel
type OllamaCloudKey = bridgeconfig.OllamaCloudKey
type OllamaCloudModel = bridgeconfig.OllamaCloudModel
type CommandCodeKey = bridgeconfig.CommandCodeKey
type CommandCodeModel = bridgeconfig.CommandCodeModel
type VertexCompatKey = bridgeconfig.VertexCompatKey
type VertexCompatModel = bridgeconfig.VertexCompatModel
type OpenAICompatibility = bridgeconfig.OpenAICompatibility
type OpenAICompatibilityAPIKey = bridgeconfig.OpenAICompatibilityAPIKey
type OpenAICompatibilityModel = bridgeconfig.OpenAICompatibilityModel
type ProxyConnectionPoolConfig = bridgeconfig.ProxyConnectionPoolConfig
type OAuthModelSetting = bridgeconfig.OAuthModelSetting

type TLS = bridgeconfig.TLSConfig

const (
	DefaultPanelGitHubRepository = bridgeconfig.DefaultPanelGitHubRepository
	DefaultBedrockRegion         = bridgeconfig.DefaultBedrockRegion
	DefaultClineBaseURL          = bridgeconfig.DefaultClineBaseURL
	DefaultOllamaCloudBaseURL    = bridgeconfig.DefaultOllamaCloudBaseURL
	DefaultCommandCodeBaseURL    = bridgeconfig.DefaultCommandCodeBaseURL
	DefaultPprofAddr             = bridgeconfig.DefaultPprofAddr

	DistributionWeighted     = bridgeconfig.DistributionWeighted
	DistributionLeastLoad    = bridgeconfig.DistributionLeastLoad
	DistributionFillFirst    = bridgeconfig.DistributionFillFirst
	DistributionResetAware   = bridgeconfig.DistributionResetAware
	DefaultStickyMaxRequests = bridgeconfig.DefaultStickyMaxRequests
)

func NormalizeDistribution(value string) string {
	return bridgeconfig.NormalizeDistribution(value)
}

func MigrateLegacyPriorities(priorities map[string]int) map[string]int {
	return bridgeconfig.MigrateLegacyPriorities(priorities)
}

func LoadConfig(configFile string) (*Config, error) { return bridgeconfig.LoadConfig(configFile) }

func ParseConfigBytes(data []byte) (*Config, error) { return bridgeconfig.ParseConfigBytes(data) }

func ResolveOAuthModelSetting(settings []OAuthModelSetting, modelID, metadataModelID, modelName string) *OAuthModelSetting {
	return bridgeconfig.ResolveOAuthModelSetting(settings, modelID, metadataModelID, modelName)
}

func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	return bridgeconfig.LoadConfigOptional(configFile, optional)
}

func SaveConfigPreserveComments(configFile string, cfg *Config) error {
	return bridgeconfig.SaveConfigPreserveComments(configFile, cfg)
}

func SaveConfigPreserveCommentsUpdateNestedScalar(configFile string, path []string, value string) error {
	return bridgeconfig.SaveConfigPreserveCommentsUpdateNestedScalar(configFile, path, value)
}

func NormalizeCommentIndentation(data []byte) []byte {
	return bridgeconfig.NormalizeCommentIndentation(data)
}

func NormalizeRoutingStrategy(strategy string) string {
	return bridgeconfig.NormalizeRoutingStrategy(strategy)
}
