package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"
)

// Issue #748: two overlapping saves used to splice config.yaml. Both wrote from offset 0
// into the same inode via os.Create (O_TRUNC), so the shorter write left the longer
// write's tail behind. In config.example.yaml that tail lands inside the big commented
// payload example, which is why the reported artifact was the back half of a long
// comment line with its leading '#' gone, followed by a duplicated "# filter:" block.
//
// The size delta between the two rendered configs is what sets the splice point, so the
// test manufactures one deliberately.
func TestConcurrentSavesNeverCorruptConfig(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}

	const iterations = 40
	for i := 0; i < iterations; i++ {
		configPath := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(configPath, example, 0o600); err != nil {
			t.Fatalf("seed config: %v", err)
		}

		short, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		long, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		// Make the two renderings differ in length by a few hundred bytes.
		short.OpenAICompatibility = nil
		short.APIKeys = nil
		long.APIKeys = append(long.APIKeys, strings.Repeat("x", 470))

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = SaveConfigPreserveComments(configPath, short)
		}()
		go func() {
			defer wg.Done()
			_ = SaveConfigPreserveComments(configPath, long)
		}()
		wg.Wait()

		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("read config after concurrent saves: %v", err)
		}
		var parsed yaml.Node
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("iteration %d: config.yaml is unparseable after concurrent saves: %v", i, err)
		}
		// A splice duplicates whatever followed the seam, so a repeated unique example
		// comment is corruption even when the result still happens to parse.
		if got := strings.Count(string(data), "#   filter: # Filter rules remove specified parameters from the payload."); got > 1 {
			t.Fatalf("iteration %d: filter example comment appears %d times, want at most 1", i, got)
		}
	}
}

// The nested-scalar writer shares the same lock, so it must not splice against a full
// save either.
func TestConcurrentNestedScalarSaveNeverCorruptsConfig(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}

	for i := 0; i < 40; i++ {
		configPath := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(configPath, example, 0o600); err != nil {
			t.Fatalf("seed config: %v", err)
		}
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		cfg.APIKeys = append(cfg.APIKeys, strings.Repeat("y", 512))

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = SaveConfigPreserveComments(configPath, cfg)
		}()
		go func() {
			defer wg.Done()
			_ = SaveConfigPreserveCommentsUpdateNestedScalar(configPath, []string{"remote-management", "secret-key"}, "rotated-secret")
		}()
		wg.Wait()

		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("read config: %v", err)
		}
		var parsed yaml.Node
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("iteration %d: config.yaml is unparseable: %v", i, err)
		}
	}
}

func TestWriteYAMLFileAtomicRejectsUnparseableYAML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	const original = "port: 8317\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	// The exact shape issue #748 produced: a stray unquoted JSON fragment.
	broken := []byte("port: 8317\n\":{\\\"name\\\":\\\"answer\\\"}}}\"\n")
	if err := WriteYAMLFileAtomic(configPath, broken); err == nil {
		t.Fatal("WriteYAMLFileAtomic accepted unparseable YAML, want error")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(data) != original {
		t.Fatalf("config was modified by a rejected write:\n%s", string(data))
	}
}

func TestWriteYAMLFileAtomicPreservesPermissions(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o640); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := WriteYAMLFileAtomic(configPath, []byte("port: 8318\n")); err != nil {
		t.Fatalf("WriteYAMLFileAtomic: %v", err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}
}

// Bind mounts and cross-device layouts (Docker, some NAS setups) reject rename; those
// deployments must keep working via the in-place fallback.
func TestWriteYAMLFileAtomicFallsBackWhenRenameUnsupported(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("routing:\n  strategy: round-robin\n"), 0o640); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	renameCalled := false
	err := writeYAMLFileAtomicWithRename(configPath, []byte("port: 8318\nlogging-to-file: true\n"), func(oldPath, newPath string) error {
		renameCalled = true
		if filepath.Dir(oldPath) != filepath.Dir(configPath) {
			t.Fatalf("temp file dir = %s, want %s", filepath.Dir(oldPath), filepath.Dir(configPath))
		}
		if newPath != configPath {
			t.Fatalf("rename target = %s, want %s", newPath, configPath)
		}
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EBUSY}
	})
	if err != nil {
		t.Fatalf("writeYAMLFileAtomicWithRename returned error: %v", err)
	}
	if !renameCalled {
		t.Fatal("rename was not attempted")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if got := string(data); strings.Contains(got, "routing:") || !strings.Contains(got, "logging-to-file: true") {
		t.Fatalf("config was not rewritten in place:\n%s", got)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}
}

// A read-only directory holding a writable config.yaml must keep saving: the previous
// in-place write only needed a writable file, so failing here would turn an occasional
// corruption into a permanent inability to save.
func TestWriteYAMLFileAtomicFallsBackWhenDirectoryIsReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := WriteYAMLFileAtomic(configPath, []byte("port: 8318\n")); err != nil {
		t.Fatalf("WriteYAMLFileAtomic in read-only dir: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(data) != "port: 8318\n" {
		t.Fatalf("config not updated: %q", string(data))
	}
}
