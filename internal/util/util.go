// Package util provides utility functions for the CLI Proxy API server.
// It includes helper functions for logging configuration, file system operations,
// and other common utilities used throughout the application.
package util

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

var functionNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

// SanitizeFunctionName ensures a function name matches the requirements for Gemini/Vertex AI.
// It replaces invalid characters with underscores, ensures it starts with a letter or underscore,
// and truncates it to 64 characters if necessary.
// Regex Rule: [^a-zA-Z0-9_.:-] replaced with _.
func SanitizeFunctionName(name string) string {
	if name == "" {
		return ""
	}

	// Replace invalid characters with underscore
	sanitized := functionNameSanitizer.ReplaceAllString(name, "_")

	// Ensure it starts with a letter or underscore
	// Re-reading requirements: Must start with a letter or an underscore.
	if len(sanitized) > 0 {
		first := sanitized[0]
		if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {
			// If it starts with an allowed character but not allowed at the beginning (digit, dot, colon, dash),
			// we must prepend an underscore.

			// To stay within the 64-character limit while prepending, we must truncate first.
			if len(sanitized) >= 64 {
				sanitized = sanitized[:63]
			}
			sanitized = "_" + sanitized
		}
	} else {
		sanitized = "_"
	}

	// Truncate to 64 characters
	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}
	return sanitized
}

// SetLogLevel configures the logrus log level based on the configuration.
// It sets the log level to DebugLevel if debug mode is enabled, otherwise to InfoLevel.
func SetLogLevel(cfg *config.Config) {
	currentLevel := log.GetLevel()
	var newLevel log.Level
	if cfg.Debug {
		newLevel = log.DebugLevel
	} else {
		newLevel = log.InfoLevel
	}

	if currentLevel != newLevel {
		log.SetLevel(newLevel)
		log.Infof("log level changed from %s to %s (debug=%t)", currentLevel, newLevel, cfg.Debug)
	}
}

// ResolveAuthDir normalizes the auth directory path for consistent reuse throughout the app.
// It expands a leading tilde (~) to the user's home directory and returns a cleaned path.
func ResolveAuthDir(authDir string) (string, error) {
	if authDir == "" {
		return "", nil
	}
	if strings.HasPrefix(authDir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve auth dir: %w", err)
		}
		remainder := strings.TrimPrefix(authDir, "~")
		remainder = strings.TrimLeft(remainder, "/\\")
		if remainder == "" {
			return filepath.Clean(home), nil
		}
		normalized := strings.ReplaceAll(remainder, "\\", "/")
		return filepath.Clean(filepath.Join(home, filepath.FromSlash(normalized))), nil
	}
	return filepath.Clean(authDir), nil
}

// LegacyDockerAuthDirs are container paths used by older Compose defaults.
// They remain candidates when diagnosing empty AuthDir after upgrades.
func LegacyDockerAuthDirs() []string {
	return []string{
		"/root/.cli-proxy-api",
		"/CLIProxyAPI/auths",
	}
}

// CountJSONAuthFiles counts non-empty .json files under dir (recursive).
// Missing directories return 0 without error so callers can treat "empty" uniformly.
func CountJSONAuthFiles(dir string) (int, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return 0, nil
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("auth dir is not a directory: %s", trimmed)
	}
	count := 0
	err = filepath.WalkDir(trimmed, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != trimmed && IsReservedAuthSubdir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		fi, errInfo := d.Info()
		if errInfo != nil {
			return nil
		}
		if fi.Size() > 0 {
			count++
		}
		return nil
	})
	if err != nil {
		return count, err
	}
	return count, nil
}

// LogResolvedAuthDir logs the effective auth directory and file count.
// When the resolved directory is empty but a known legacy Docker auth path still
// has JSON files, it warns about AUTH_PATH / volume misalignment (issue #542).
func LogResolvedAuthDir(authDir string) {
	trimmed := strings.TrimSpace(authDir)
	if trimmed == "" {
		log.Warn("auth directory is empty; file-based credentials will not load")
		return
	}
	authEnv := strings.TrimSpace(os.Getenv("AUTH_PATH"))
	if authEnv != "" {
		log.Infof("auth directory resolved to %s (AUTH_PATH=%s)", trimmed, authEnv)
	} else {
		log.Infof("auth directory resolved to %s", trimmed)
	}
	count, err := CountJSONAuthFiles(trimmed)
	if err != nil {
		log.Warnf("auth directory %s is not fully readable: %v", trimmed, err)
		return
	}
	log.Infof("auth directory %s contains %d non-empty .json file(s)", trimmed, count)
	if count > 0 {
		return
	}
	resolvedClean := filepath.Clean(trimmed)
	for _, candidate := range LegacyDockerAuthDirs() {
		if filepath.Clean(candidate) == resolvedClean {
			continue
		}
		legacyCount, legacyErr := CountJSONAuthFiles(candidate)
		if legacyErr != nil || legacyCount == 0 {
			continue
		}
		log.Warnf(
			"auth directory %s is empty, but %s still has %d .json file(s); "+
				"check AUTH_PATH and the docker auth volume destination so the process reads the mounted directory",
			trimmed, candidate, legacyCount,
		)
	}
}

// CountAuthFiles returns the number of auth records available through the provided Store.
// For filesystem-backed stores, this reflects the number of JSON auth files under the configured directory.
func CountAuthFiles[T any](ctx context.Context, store interface {
	List(context.Context) ([]T, error)
}) int {
	if store == nil {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	entries, err := store.List(ctx)
	if err != nil {
		log.Debugf("countAuthFiles: failed to list auth records: %v", err)
		return 0
	}
	return len(entries)
}

// WritablePath returns the cleaned WRITABLE_PATH environment variable when it is set.
// It accepts both uppercase and lowercase variants for compatibility with existing conventions.
func WritablePath() string {
	for _, key := range []string{"WRITABLE_PATH", "writable_path"} {
		if value, ok := os.LookupEnv(key); ok {
			trimmed := strings.TrimSpace(value)
			if trimmed != "" {
				return filepath.Clean(trimmed)
			}
		}
	}
	return ""
}
