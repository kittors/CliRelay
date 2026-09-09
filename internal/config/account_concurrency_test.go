package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestAccountConcurrency_DefaultsToQueuing(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, "port: 8317\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := cfg.AccountConcurrency.WaitTimeout(); got != DefaultAccountConcurrencyWaitSeconds*time.Second {
		t.Fatalf("expected default queue timeout %ds, got %s", DefaultAccountConcurrencyWaitSeconds, got)
	}
	if depth := cfg.AccountConcurrency.QueueDepth(); depth != 0 {
		t.Fatalf("expected unbounded queue by default, got %d", depth)
	}
}

func TestAccountConcurrency_ExplicitZeroDisablesQueuing(t *testing.T) {
	// An omitted field and an explicit 0 must not mean the same thing: operators
	// need a way to keep the old fail-fast behaviour.
	cfg, err := LoadConfig(writeConfigFile(t, "port: 8317\naccount-concurrency:\n  wait-timeout-seconds: 0\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := cfg.AccountConcurrency.WaitTimeout(); got != 0 {
		t.Fatalf("expected queuing disabled, got %s", got)
	}
}

func TestAccountConcurrency_ReadsConfiguredValues(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, "port: 8317\naccount-concurrency:\n  wait-timeout-seconds: 45\n  max-queue-depth: 8\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := cfg.AccountConcurrency.WaitTimeout(); got != 45*time.Second {
		t.Fatalf("expected 45s queue timeout, got %s", got)
	}
	if depth := cfg.AccountConcurrency.QueueDepth(); depth != 8 {
		t.Fatalf("expected queue depth 8, got %d", depth)
	}
}

func TestAccountConcurrency_NegativeValuesAreClamped(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, "port: 8317\naccount-concurrency:\n  wait-timeout-seconds: -5\n  max-queue-depth: -3\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := cfg.AccountConcurrency.WaitTimeout(); got != 0 {
		t.Fatalf("expected negative timeout to disable queuing, got %s", got)
	}
	if depth := cfg.AccountConcurrency.QueueDepth(); depth != 0 {
		t.Fatalf("expected negative depth to mean unbounded, got %d", depth)
	}
}
