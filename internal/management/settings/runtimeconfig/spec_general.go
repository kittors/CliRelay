package runtimeconfig

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// Settings the management API can change that used to live only in the local
// config.yaml. In a cluster a panel save lands on whichever node served the
// request, so a YAML-only value would silently differ between nodes; storing
// it in runtime_settings like the provider settings makes it cluster-wide.
// They follow the same lifecycle: imported from config.yaml once when the
// database has no row, applied over the YAML value on every load, and removed
// from config.yaml afterwards.
//
// Deliberately NOT stored here, because they describe the node rather than the
// service and must be able to differ between nodes: host, port, tls, auth-dir,
// postgres, redis, cluster, pprof, remote-management, request-body cache paths,
// trusted-proxies, timezone and auto-update (it drives the updater sidecar of
// the node that serves the request). Those stay in each node's config.yaml.
const (
	RuntimeSettingDebug              = "debug"
	RuntimeSettingRequestRetry       = "request-retry"
	RuntimeSettingMaxRetryInterval   = "max-retry-interval"
	RuntimeSettingQuotaExceeded      = "quota-exceeded"
	RuntimeSettingForceModelPrefix   = "force-model-prefix"
	RuntimeSettingWebsocketAuth      = "ws-auth"
	RuntimeSettingAmpCode            = "ampcode"
	RuntimeSettingRequestLog         = "request-log"
	RuntimeSettingUsageStatistics    = "usage-statistics-enabled"
	RuntimeSettingLoggingToFile      = "logging-to-file"
	RuntimeSettingLogsMaxTotalSizeMB = "logs-max-total-size-mb"
	RuntimeSettingErrorLogsMaxFiles  = "error-logs-max-files"
	RuntimeSettingRequestLogStorage  = "request-log-storage"
	RuntimeSettingProxyURL           = "proxy-url"
)

// Specs lists every runtime setting a config.Config is built from.
func Specs() []Spec {
	return append(providerAndRuntimeSpecs(), generalSpecs()...)
}

// SpecByKey returns the spec stored under key.
func SpecByKey(key string) (Spec, bool) {
	for _, spec := range Specs() {
		if spec.Key == key {
			return spec, true
		}
	}
	return Spec{}, false
}

// Canonical renders the value spec stores for cfg. Two configs hold the same
// setting exactly when their canonical forms are equal.
func Canonical(spec Spec, cfg *config.Config) ([]byte, error) {
	return json.Marshal(spec.Value(cfg))
}

func generalSpecs() []Spec {
	return []Spec{
		valueSpec(RuntimeSettingDebug, func(c *config.Config) *bool { return &c.Debug }, isTrue),
		valueSpec(RuntimeSettingRequestRetry, func(c *config.Config) *int { return &c.RequestRetry }, isNonZero),
		valueSpec(RuntimeSettingMaxRetryInterval, func(c *config.Config) *int { return &c.MaxRetryInterval }, isNonZero),
		valueSpec(RuntimeSettingQuotaExceeded, func(c *config.Config) *config.QuotaExceeded { return &c.QuotaExceeded },
			func(v config.QuotaExceeded) bool { return v.SwitchProject || v.SwitchPreviewModel }),
		valueSpec(RuntimeSettingForceModelPrefix, func(c *config.Config) *bool { return &c.ForceModelPrefix }, isTrue),
		valueSpec(RuntimeSettingWebsocketAuth, func(c *config.Config) *bool { return &c.WebsocketAuth }, isTrue),
		valueSpec(RuntimeSettingAmpCode, func(c *config.Config) *config.AmpCode { return &c.AmpCode }, ampCodeMeaningful),
		valueSpec(RuntimeSettingRequestLog, func(c *config.Config) *bool { return &c.RequestLog }, isTrue),
		valueSpec(RuntimeSettingUsageStatistics, func(c *config.Config) *bool { return &c.UsageStatisticsEnabled }, isTrue),
		valueSpec(RuntimeSettingLoggingToFile, func(c *config.Config) *bool { return &c.LoggingToFile }, isTrue),
		valueSpec(RuntimeSettingLogsMaxTotalSizeMB, func(c *config.Config) *int { return &c.LogsMaxTotalSizeMB },
			func(v int) bool { return v != config.DefaultLogsMaxTotalSizeMB }),
		valueSpec(RuntimeSettingErrorLogsMaxFiles, func(c *config.Config) *int { return &c.ErrorLogsMaxFiles },
			func(v int) bool { return v != config.DefaultErrorLogsMaxFiles }),
		valueSpec(RuntimeSettingRequestLogStorage, func(c *config.Config) *config.RequestLogStorageConfig { return &c.RequestLogStorage },
			requestLogStorageMeaningful),
		valueSpec(RuntimeSettingProxyURL, func(c *config.Config) *string { return &c.ProxyURL },
			func(v string) bool { return strings.TrimSpace(v) != "" }),
	}
}

// valueSpec builds the spec of a setting that is one config field, stored as
// that field's JSON. meaningful reports whether a value differs from what a
// config.yaml without the key loads, which is when importing it is needed.
func valueSpec[T any](key string, field func(*config.Config) *T, meaningful func(T) bool) Spec {
	return Spec{
		Key:        key,
		Meaningful: func(cfg *config.Config) bool { return meaningful(*field(cfg)) },
		Value:      func(cfg *config.Config) any { return *field(cfg) },
		Apply: func(cfg *config.Config, raw json.RawMessage) bool {
			var value T
			if err := json.Unmarshal(raw, &value); err != nil {
				log.Warnf("runtimeconfig: decode %s: %v", key, err)
				return false
			}
			*field(cfg) = value
			return true
		},
	}
}

func isTrue(v bool) bool   { return v }
func isNonZero(v int) bool { return v != 0 }

func ampCodeMeaningful(v config.AmpCode) bool {
	return strings.TrimSpace(v.UpstreamURL) != "" ||
		strings.TrimSpace(v.UpstreamAPIKey) != "" ||
		len(v.UpstreamAPIKeys) > 0 ||
		v.RestrictManagementToLocalhost ||
		len(v.ModelMappings) > 0 ||
		v.ForceModelMappings
}

// requestLogStorageMeaningful treats the zero value as "not configured" as
// well: a config built without LoadConfig has never seen the defaults, and
// importing its zeroes would pin retention to the minimum on every node.
func requestLogStorageMeaningful(v config.RequestLogStorageConfig) bool {
	if reflect.DeepEqual(v, config.RequestLogStorageConfig{}) {
		return false
	}
	return !reflect.DeepEqual(v, config.DefaultRequestLogStorageConfig())
}

// ReloadScope says what a node has to rebuild after a runtime setting it did
// not write itself changed.
type ReloadScope int

const (
	// ReloadLight: apply the value to the live config; nothing captures it at
	// bind time, so request handling picks it up immediately.
	ReloadLight ReloadScope = iota
	// ReloadRuntime: executors or config-derived credentials are built from
	// it, so they have to be rebuilt (a full config reload).
	ReloadRuntime
	// ReloadWithModels: it also shapes the model lists registered for
	// credentials, so the reload re-registers models as well.
	ReloadWithModels
)

// ReloadScopeForKey classifies a runtime_settings key. Keys that are not
// config fields (image presets, the IP access policy) are read from the
// database when used and need no reload, which ReloadLight covers.
func ReloadScopeForKey(key string) ReloadScope {
	switch key {
	case RuntimeSettingOAuthExcludedModels, RuntimeSettingOAuthModelAlias, RuntimeSettingForceModelPrefix:
		return ReloadWithModels
	case RuntimeSettingGeminiKeys, RuntimeSettingCodexKeys, RuntimeSettingClaudeKeys, RuntimeSettingBedrockKeys,
		RuntimeSettingOpenCodeGoKeys, RuntimeSettingClineKeys, RuntimeSettingOllamaCloudKeys, RuntimeSettingCommandCodeKeys,
		RuntimeSettingOpenAICompatibility, RuntimeSettingVertexCompatKeys,
		RuntimeSettingClaudeHeaderDefaults, RuntimeSettingKimiHeaderDefaults, RuntimeSettingIdentityFingerprint,
		RuntimeSettingCodexOAuthAdmission, RuntimeSettingPayload, RuntimeSettingProxyURL:
		return ReloadRuntime
	default:
		return ReloadLight
	}
}

// IsProviderListKey reports whether key holds provider credentials, from which
// config-derived credentials are synthesised.
func IsProviderListKey(key string) bool {
	switch key {
	case RuntimeSettingGeminiKeys, RuntimeSettingCodexKeys, RuntimeSettingClaudeKeys, RuntimeSettingBedrockKeys,
		RuntimeSettingOpenCodeGoKeys, RuntimeSettingClineKeys, RuntimeSettingOllamaCloudKeys, RuntimeSettingCommandCodeKeys,
		RuntimeSettingOpenAICompatibility, RuntimeSettingVertexCompatKeys:
		return true
	}
	return false
}
