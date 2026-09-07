package auth

import (
	"strconv"
	"strings"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// Selection-scoped metadata keys. The Selector interface is fixed by the SDK
// contract, so per-group scheduling settings travel with the request options
// rather than through the selector constructor. That also keeps the selectors
// themselves singletons, which is required for their cursors, sticky bindings
// and pressure counters to survive across requests.
const (
	distributionMetadataKey  = "scheduling_distribution"
	stickyMaxRequestsKey     = "sticky_max_requests"
	stickyReleaseAtLoadKey   = "sticky_release_at_load"
	stickyEnabledMetadataKey = "sticky_enabled"
)

type stickyLimits struct {
	maxRequests   int
	releaseAtLoad float64
}

func distributionFromMetadata(meta map[string]any) string {
	return sdkconfig.NormalizeDistribution(metadataStringValue(meta, distributionMetadataKey))
}

func stickyEnabledFromMetadata(meta map[string]any) bool {
	value := strings.TrimSpace(metadataStringValue(meta, stickyEnabledMetadataKey))
	if value == "" {
		return false
	}
	enabled, err := strconv.ParseBool(value)
	return err == nil && enabled
}

func stickyLimitsFromMetadata(meta map[string]any) stickyLimits {
	limits := stickyLimits{}
	if raw := strings.TrimSpace(metadataStringValue(meta, stickyMaxRequestsKey)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limits.maxRequests = parsed
		}
	}
	if raw := strings.TrimSpace(metadataStringValue(meta, stickyReleaseAtLoadKey)); raw != "" {
		if parsed, err := strconv.ParseFloat(raw, 64); err == nil && parsed > 0 {
			limits.releaseAtLoad = parsed
		}
	}
	return limits
}
