package synthesizer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

func TestFileSynthesizerSkipsClusterBackupDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{
		"live.json",
		filepath.Join(util.PreClusterBackupDirPrefix+"20260924-120000", "old.json"),
		filepath.Join(util.ClusterStrayDirPrefix+"20260924-120000", "stray.json"),
	} {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"type":"claude","email":"x@example.com"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	auths, err := NewFileSynthesizer().Synthesize(&SynthesisContext{Config: &config.Config{}, AuthDir: dir, Now: time.Now(), IDGenerator: NewStableIDGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || auths[0].ID != "live.json" {
		t.Fatalf("synthesized %d auths, want only live.json", len(auths))
	}
}
