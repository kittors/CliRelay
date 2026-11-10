package auth

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

type countingRoundTripper struct{ calls atomic.Int32 }

func (c *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return nil, errors.New("the file store must not reach the network")
}

// Reading an Antigravity credential that has no project must not run project
// discovery. That probe went out through http.DefaultClient — straight from
// this host, past the account's egress proxy — and only the never-expiring
// "no project" flag had kept it quiet; once that flag can expire it would
// fire on every file read. Discovery belongs to the executor (#1032).
func TestFileTokenStoreReadDoesNotProbeAntigravityProject(t *testing.T) {
	recorder := &countingRoundTripper{}
	previous := http.DefaultClient.Transport
	http.DefaultClient.Transport = recorder
	t.Cleanup(func() { http.DefaultClient.Transport = previous })

	dir := t.TempDir()
	path := filepath.Join(dir, "antigravity-a.json")
	// The shape production credentials have: a token, no project, and the
	// September flag that no longer blocks discovery.
	body := `{"type":"antigravity","email":"a@example.com","access_token":"at","refresh_token":"rt",` +
		`"antigravity_project_id_unavailable":true,"antigravity_project_id_probed_at":1757928120000,` +
		`"antigravity_project_id_probe_err":"no project_id in response"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	loaded, err := store.readAuthFile(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.Provider != "antigravity" {
		t.Fatalf("loaded = %#v, want the antigravity credential", loaded)
	}
	if got := recorder.calls.Load(); got != 0 {
		t.Fatalf("reading the credential made %d network request(s), want 0", got)
	}
	if _, ok := loaded.Metadata["project_id"]; ok {
		t.Fatalf("project_id = %#v, want it left for the executor to discover", loaded.Metadata["project_id"])
	}
}
