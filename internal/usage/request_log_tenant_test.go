package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRequestLogsTenantIsolation(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})

	tenantA := "00000000-0000-0000-0000-00000000000a"
	tenantB := "00000000-0000-0000-0000-00000000000b"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	resultA, err := getDB().Exec(`INSERT INTO request_logs(tenant_id,timestamp,api_key,model) VALUES(?,?,?,?)`, tenantA, now, "sk-a", "model-a")
	if err != nil {
		t.Fatalf("insert tenant A log: %v", err)
	}
	idA, _ := resultA.LastInsertId()
	if _, err = getDB().Exec(`INSERT INTO request_logs(tenant_id,timestamp,api_key,model) VALUES(?,?,?,?)`, tenantB, now, "sk-b", "model-b"); err != nil {
		t.Fatalf("insert tenant B log: %v", err)
	}
	if _, err = getDB().Exec(`INSERT INTO request_log_content(tenant_id,log_id,timestamp,input_content) VALUES(?,?,?,?)`, tenantA, idA, now, []byte("input-a")); err != nil {
		t.Fatalf("insert tenant A content: %v", err)
	}

	logsA, err := QueryLogs(LogQueryParams{TenantID: tenantA, Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("query tenant A: %v", err)
	}
	if logsA.Total != 1 || len(logsA.Items) != 1 || logsA.Items[0].Model != "model-a" {
		t.Fatalf("tenant A logs = %#v", logsA)
	}
	logsB, err := QueryLogs(LogQueryParams{TenantID: tenantB, Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("query tenant B: %v", err)
	}
	if logsB.Total != 1 || logsB.Items[0].Model != "model-b" {
		t.Fatalf("tenant B logs = %#v", logsB)
	}
	if _, err = QueryLogContentForTenant(tenantB, idA); err == nil {
		t.Fatal("tenant B read tenant A log content")
	}
	if _, err = ClearAllRequestLogsForTenant(tenantB); err != nil {
		t.Fatalf("clear tenant B: %v", err)
	}
	logsA, _ = QueryLogs(LogQueryParams{TenantID: tenantA, Page: 1, Size: 10, Days: 1})
	if logsA.Total != 1 {
		t.Fatalf("clearing tenant B removed tenant A logs: %#v", logsA)
	}
}
