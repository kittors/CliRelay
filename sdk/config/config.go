// Package config provides the public SDK configuration API.
//
// It re-exports the server configuration types and helpers so external projects can
// embed CLIProxyAPI without importing internal packages.
package config

import bridgeconfig "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/config"

type SDKConfig = bridgeconfig.SDKConfig

type Config = bridgeconfig.Config
type APIKeyEntry = bridgeconfig.APIKeyEntry

type StreamingConfig = bridgeconfig.StreamingConfig
type RoutingConfig = bridgeconfig.RoutingConfig
type RoutingChannelGroup = bridgeconfig.RoutingChannelGroup
type RoutingPathRoute = bridgeconfig.RoutingPathRoute
type ChannelGroupMatch = bridgeconfig.ChannelGroupMatch
type TLSConfig = bridgeconfig.TLSConfig
type PprofConfig = bridgeconfig.PprofConfig
type RemoteManagement = bridgeconfig.RemoteManagement
type AmpCode = bridgeconfig.AmpCode
type OAuthModelAlias = bridgeconfig.OAuthModelAlias
type PayloadConfig = bridgeconfig.PayloadConfig
type PayloadRule = bridgeconfig.PayloadRule
type PayloadFilterRule = bridgeconfig.PayloadFilterRule
type PayloadModelRule = bridgeconfig.PayloadModelRule

type GeminiKey = bridgeconfig.GeminiKey
type CodexKey = bridgeconfig.CodexKey
type ClaudeKey = bridgeconfig.ClaudeKey
type BedrockKey = bridgeconfig.BedrockKey
type BedrockModel = bridgeconfig.BedrockModel
type OpenCodeGoKey = bridgeconfig.OpenCodeGoKey
type OpenCodeGoModel = bridgeconfig.OpenCodeGoModel
type ClineKey = bridgeconfig.ClineKey
type ClineModel = bridgeconfig.ClineModel
type VertexCompatKey = bridgeconfig.VertexCompatKey
type VertexCompatModel = bridgeconfig.VertexCompatModel
type OpenAICompatibility = bridgeconfig.OpenAICompatibility
type OpenAICompatibilityAPIKey = bridgeconfig.OpenAICompatibilityAPIKey
type OpenAICompatibilityModel = bridgeconfig.OpenAICompatibilityModel

type TLS = bridgeconfig.TLSConfig

const (
	DefaultPanelGitHubRepository = bridgeconfig.DefaultPanelGitHubRepository
	DefaultBedrockRegion         = bridgeconfig.DefaultBedrockRegion
	DefaultClineBaseURL          = bridgeconfig.DefaultClineBaseURL
	DefaultPprofAddr             = bridgeconfig.DefaultPprofAddr
)

func LoadConfig(configFile string) (*Config, error) { return bridgeconfig.LoadConfig(configFile) }

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
