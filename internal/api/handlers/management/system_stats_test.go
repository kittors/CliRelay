package management

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
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

func TestSystemStatsWebSocketMaxAgeUsesSafeBounds(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want time.Duration
	}{
		{name: "default", cfg: &config.Config{}, want: 5 * time.Minute},
		{name: "minimum", cfg: &config.Config{SystemStatsWebSocketMaxAgeSeconds: 1}, want: time.Minute},
		{name: "custom", cfg: &config.Config{SystemStatsWebSocketMaxAgeSeconds: 600}, want: 10 * time.Minute},
		{name: "maximum", cfg: &config.Config{SystemStatsWebSocketMaxAgeSeconds: 7200}, want: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(tt.cfg, filepath.Join(t.TempDir(), "config.yaml"), nil)
			t.Cleanup(h.Close)
			if got := h.systemStatsWebSocketMaxAge(); got != tt.want {
				t.Fatalf("systemStatsWebSocketMaxAge() = %s, want %s", got, tt.want)
			}
		})
	}
}
