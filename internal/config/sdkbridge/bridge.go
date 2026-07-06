// Package configbridge hosts the internal config bridge consumed by sdkbridge/config.
//
// The package narrows SDK-facing access to config types and YAML helpers while
// the canonical config implementation remains in internal/config.
package configbridge

import coreconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"

type SDKConfig = coreconfig.SDKConfig

type Config = coreconfig.Config
type APIKeyEntry = coreconfig.APIKeyEntry

type StreamingConfig = coreconfig.StreamingConfig
type RoutingConfig = coreconfig.RoutingConfig
type RoutingChannelGroup = coreconfig.RoutingChannelGroup
type RoutingPathRoute = coreconfig.RoutingPathRoute
type ChannelGroupMatch = coreconfig.ChannelGroupMatch
type TLSConfig = coreconfig.TLSConfig
type RemoteManagement = coreconfig.RemoteManagement
type AmpCode = coreconfig.AmpCode
type OAuthModelAlias = coreconfig.OAuthModelAlias
type PayloadConfig = coreconfig.PayloadConfig
type PayloadRule = coreconfig.PayloadRule
type PayloadFilterRule = coreconfig.PayloadFilterRule
type PayloadModelRule = coreconfig.PayloadModelRule

type GeminiKey = coreconfig.GeminiKey
type CodexKey = coreconfig.CodexKey
type ClaudeKey = coreconfig.ClaudeKey
type BedrockKey = coreconfig.BedrockKey
type BedrockModel = coreconfig.BedrockModel
type OpenCodeGoKey = coreconfig.OpenCodeGoKey
type OpenCodeGoModel = coreconfig.OpenCodeGoModel
type ClineKey = coreconfig.ClineKey
type ClineModel = coreconfig.ClineModel
type OllamaCloudKey = coreconfig.OllamaCloudKey
type OllamaCloudModel = coreconfig.OllamaCloudModel
type VertexCompatKey = coreconfig.VertexCompatKey
type VertexCompatModel = coreconfig.VertexCompatModel
type OpenAICompatibility = coreconfig.OpenAICompatibility
type OpenAICompatibilityAPIKey = coreconfig.OpenAICompatibilityAPIKey
type OpenAICompatibilityModel = coreconfig.OpenAICompatibilityModel

type TLS = coreconfig.TLSConfig

const (
	DefaultPanelGitHubRepository = coreconfig.DefaultPanelGitHubRepository
	DefaultBedrockRegion         = coreconfig.DefaultBedrockRegion
	DefaultClineBaseURL          = coreconfig.DefaultClineBaseURL
	DefaultOllamaCloudBaseURL    = coreconfig.DefaultOllamaCloudBaseURL
)

func LoadConfig(configFile string) (*Config, error) { return coreconfig.LoadConfig(configFile) }

func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	return coreconfig.LoadConfigOptional(configFile, optional)
}

func SaveConfigPreserveComments(configFile string, cfg *Config) error {
	return coreconfig.SaveConfigPreserveComments(configFile, cfg)
}

func SaveConfigPreserveCommentsUpdateNestedScalar(configFile string, path []string, value string) error {
	return coreconfig.SaveConfigPreserveCommentsUpdateNestedScalar(configFile, path, value)
}

func NormalizeCommentIndentation(data []byte) []byte {
	return coreconfig.NormalizeCommentIndentation(data)
}

func NormalizeRoutingStrategy(strategy string) string {
	return coreconfig.NormalizeRoutingStrategy(strategy)
}
