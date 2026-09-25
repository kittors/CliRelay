package proxypool

import (
	"context"
	"database/sql"
	"slices"
	"testing"

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
