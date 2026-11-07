package proxypool

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	_ "modernc.org/sqlite"
)

func TestListEnabledURLsCoversEveryTenant(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Every connection to :memory: is a new database; keep the one with the table.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)

	rows := []struct {
		tenant, id, url string
		enabled         int
	}{
		{"00000000-0000-0000-0000-000000000001", "sys-on", "socks5://u:p@203.0.113.1:1080", 1},
		{"00000000-0000-0000-0000-000000000001", "sys-off", "socks5://203.0.113.2:1080", 0},
		{"11111111-1111-1111-1111-111111111111", "tenant-on", "http://203.0.113.3:3128", 1},
		{"11111111-1111-1111-1111-111111111111", "tenant-off", "http://203.0.113.4:3128", 0},
	}
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO proxy_pool (tenant_id, id, url, enabled) VALUES (?, ?, ?, ?)`,
			row.tenant, row.id, row.url, row.enabled); err != nil {
			t.Fatal(err)
		}
	}

	urls, err := ListEnabledURLs(context.Background(), db)
	if err != nil {
		t.Fatalf("ListEnabledURLs: %v", err)
	}
	slices.Sort(urls)
	if want := []string{"http://203.0.113.3:3128", "socks5://u:p@203.0.113.1:1080"}; !slices.Equal(urls, want) {
		t.Fatalf("urls = %v, want the enabled entries of both tenants %v", urls, want)
	}

	if urls, err := ListEnabledURLs(context.Background(), nil); urls != nil || err != nil {
		t.Fatalf("a missing database should yield nothing, got %v, %v", urls, err)
	}
	_ = db.Close()
	if _, err := ListEnabledURLs(context.Background(), db); err == nil {
		t.Fatal("a failed query must be reported, not read as an empty pool")
	}
}

// ListContext reports what List swallows: List logs a failed query and skips a
// row it cannot scan, so its result cannot tell an empty pool from an
// unreadable one.
func TestListContextReportsWhatListSkips(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)
	store := NewStore(db)
	ctx := context.Background()

	if entries, err := store.ListContext(ctx); err != nil || len(entries) != 0 {
		t.Fatalf("ListContext of an empty pool = %v, %v", entries, err)
	}
	if err := store.Replace([]config.ProxyPoolEntry{
		{ID: "hk-1", URL: "http://hk-1.example:8080", Enabled: true},
		{ID: "sg-1", URL: "socks5://sg-1.example:1080", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if entries, err := store.ListContext(ctx); err != nil || len(entries) != 2 {
		t.Fatalf("ListContext = %v, %v; want both proxies", entries, err)
	}

	if _, err := db.Exec(`UPDATE proxy_pool SET enabled = 'yes' WHERE id = 'sg-1'`); err != nil {
		t.Fatal(err)
	}
	if entries, err := store.ListContext(ctx); err == nil {
		t.Fatalf("ListContext with an unreadable row = %v, want an error", entries)
	}
	if entries := store.List(); len(entries) != 1 || entries[0].ID != "hk-1" {
		t.Fatalf("List = %v, want the readable proxy as before", entries)
	}
	_ = db.Close()
	if _, err := store.ListContext(ctx); err == nil {
		t.Fatal("a failed query must be reported, not read as an empty pool")
	}
}

func TestSystemReplaceDropsGrantsOfRemovedEntries(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)
	ctx := context.Background()
	grants := resourcegrants.NewStore(db)
	system := NewStore(db)
	tenant := NewTenantStore(db, "11111111-1111-1111-1111-111111111111")

	if err := system.Replace([]config.ProxyPoolEntry{
		{ID: "hk-1", URL: "socks5://203.0.113.1:1080", Enabled: true},
		{ID: "sg-1", URL: "socks5://203.0.113.2:1080", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Replace(ctx, resourcegrants.KindProxy, []string{"hk-1", "sg-1"}, []string{resourcegrants.AllTenants}); err != nil {
		t.Fatal(err)
	}

	// A tenant replacing its own pool, even with the same IDs, never touches
	// the platform's grants.
	if err := tenant.Replace([]config.ProxyPoolEntry{{ID: "hk-1", URL: "http://203.0.113.9:3128", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := grants.List(ctx, resourcegrants.KindProxy); len(got) != 2 {
		t.Fatalf("tenant replace changed grants: %v", got)
	}

	if err := system.Replace([]config.ProxyPoolEntry{{ID: "sg-1", URL: "socks5://203.0.113.2:1080", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	got, err := grants.List(ctx, resourcegrants.KindProxy)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["hk-1"]; ok {
		t.Fatalf("grant of the removed entry survived: %v", got)
	}
	if !slices.Equal(got["sg-1"], []string{resourcegrants.AllTenants}) {
		t.Fatalf("grant of the kept entry changed: %v", got)
	}
}
