package config

// Defaults LoadConfig gives the settings that the management API can change
// and that live in runtime_settings. The settings store treats a value equal
// to its default as "not configured", so a config.yaml that never mentioned a
// key does not create a database row for it.
const (
	DefaultLogsMaxTotalSizeMB = 512
	DefaultErrorLogsMaxFiles  = 10
)

// DefaultRequestLogStorageConfig returns the request-log-storage block a
// config.yaml without one loads as.
func DefaultRequestLogStorageConfig() RequestLogStorageConfig {
	cleanupEnabled := true
	return RequestLogStorageConfig{
		StoreContent:             false,
		RetentionDays:            7,
		ContentRetentionDays:     3,
		CleanupEnabled:           &cleanupEnabled,
		CleanupIntervalMinutes:   60,
		CleanupBatchSize:         1000,
		CleanupMaxRuntimeSeconds: 30,
		MaxRows:                  100000,
		MaxMetadataSizeMB:        256,
		// Caps the compressed request/response bodies only; metadata rows are
		// bounded separately.
		MaxTotalSizeMB: 128,
	}
}
