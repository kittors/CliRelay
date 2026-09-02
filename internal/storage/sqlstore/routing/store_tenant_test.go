package routing

import (
	"database/sql"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "modernc.org/sqlite"
)

func TestStoreTenantIsolation(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)

	a := NewTenantStore(db, "tenant-a")
	b := NewTenantStore(db, "tenant-b")
	if err := a.Upsert(config.RoutingConfig{Strategy: "fill-first"}); err != nil {
		t.Fatalf("upsert tenant A: %v", err)
	}
	if err := b.Upsert(config.RoutingConfig{Strategy: "round-robin"}); err != nil {
		t.Fatalf("upsert tenant B: %v", err)
	}
	if got := a.Get(); got == nil || got.Strategy != "fill-first" {
		t.Fatalf("tenant A routing = %#v", got)
	}
	if got := b.Get(); got == nil || got.Strategy != "round-robin" {
		t.Fatalf("tenant B routing = %#v", got)
	}
}
