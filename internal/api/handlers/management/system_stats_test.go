package management

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestCachedExpensiveSystemStatsAvoidsRepeatedLogDirectoryWalks(t *testing.T) {
	logDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(logDir, "first.log"), []byte("abc"), 0o600); err != nil {
		t.Fatalf("write first log: %v", err)
	}
	h := NewHandler(&config.Config{SystemStatsCacheSeconds: 3600}, filepath.Join(t.TempDir(), "config.yaml"), nil)
	t.Cleanup(h.Close)
	h.logDir = logDir

	first := h.cachedExpensiveSystemStats()
	if first.LogDirSizeBytes != 3 {
		t.Fatalf("first log size = %d, want 3", first.LogDirSizeBytes)
	}
	if err := os.WriteFile(filepath.Join(logDir, "second.log"), []byte("defgh"), 0o600); err != nil {
		t.Fatalf("write second log: %v", err)
	}
	cached := h.cachedExpensiveSystemStats()
	if cached.LogDirSizeBytes != first.LogDirSizeBytes {
		t.Fatalf("cached log size = %d, want %d", cached.LogDirSizeBytes, first.LogDirSizeBytes)
	}

	h.systemStatsCacheMu.Lock()
	h.systemStatsCache.cachedAt = time.Now().Add(-2 * time.Hour)
	h.systemStatsCacheMu.Unlock()
	refreshed := h.cachedExpensiveSystemStats()
	if refreshed.LogDirSizeBytes != 8 {
		t.Fatalf("refreshed log size = %d, want 8", refreshed.LogDirSizeBytes)
	}
}
