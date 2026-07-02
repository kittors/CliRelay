// Package config hosts SDK bridge aliases for server configuration types.
package config

import internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"

type SDKConfig = internalconfig.SDKConfig

type Config = internalconfig.Config
type APIKeyEntry = internalconfig.APIKeyEntry

type StreamingConfig = internalconfig.StreamingConfig
type RoutingConfig = internalconfig.RoutingConfig
type RoutingChannelGroup = internalconfig.RoutingChannelGroup
type RoutingPathRoute = internalconfig.RoutingPathRoute
type ChannelGroupMatch = internalconfig.ChannelGroupMatch
type TLSConfig = internalconfig.TLSConfig
type PprofConfig = internalconfig.PprofConfig
type RemoteManagement = internalconfig.RemoteManagement
type AmpCode = internalconfig.AmpCode
type OAuthModelAlias = internalconfig.OAuthModelAlias
type PayloadConfig = internalconfig.PayloadConfig
type PayloadRule = internalconfig.PayloadRule
type PayloadFilterRule = internalconfig.PayloadFilterRule
type PayloadModelRule = internalconfig.PayloadModelRule

type GeminiKey = internalconfig.GeminiKey
type CodexKey = internalconfig.CodexKey
type ClaudeKey = internalconfig.ClaudeKey
type BedrockKey = internalconfig.BedrockKey
type BedrockModel = internalconfig.BedrockModel
type OpenCodeGoKey = internalconfig.OpenCodeGoKey
type OpenCodeGoModel = internalconfig.OpenCodeGoModel
type ClineKey = internalconfig.ClineKey
type ClineModel = internalconfig.ClineModel
type VertexCompatKey = internalconfig.VertexCompatKey
type VertexCompatModel = internalconfig.VertexCompatModel
type OpenAICompatibility = internalconfig.OpenAICompatibility
type OpenAICompatibilityAPIKey = internalconfig.OpenAICompatibilityAPIKey
type OpenAICompatibilityModel = internalconfig.OpenAICompatibilityModel

type TLS = internalconfig.TLSConfig

const (
	DefaultPanelGitHubRepository = internalconfig.DefaultPanelGitHubRepository
	DefaultBedrockRegion         = internalconfig.DefaultBedrockRegion
	DefaultClineBaseURL          = internalconfig.DefaultClineBaseURL
	DefaultPprofAddr             = internalconfig.DefaultPprofAddr
)

func LoadConfig(configFile string) (*Config, error) { return internalconfig.LoadConfig(configFile) }

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
