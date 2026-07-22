package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func setupOwnedPublicUsageDB(t *testing.T) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "owned-public-usage-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	dbPath := tmpFile.Name()
	_ = tmpFile.Close()
	t.Cleanup(func() {
		enduser.SetDefault(nil)
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := usage.RuntimeDB()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tenants (
			id TEXT PRIMARY KEY, status TEXT NOT NULL, type TEXT NOT NULL, expires_at TIMESTAMP,
			access_token_ttl_seconds INTEGER, refresh_token_ttl_seconds INTEGER
		);
		CREATE TABLE IF NOT EXISTS end_users (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL, password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active', must_change_password INTEGER NOT NULL DEFAULT 0,
			password_changed_at TIMESTAMP, last_login_at TIMESTAMP, failed_login_count INTEGER NOT NULL DEFAULT 0,
			lock_stage INTEGER NOT NULL DEFAULT 0, locked_until TIMESTAMP, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, version INTEGER NOT NULL DEFAULT 1,
			permission_profile_id TEXT NOT NULL DEFAULT '', daily_limit INTEGER NOT NULL DEFAULT 0,
			total_quota INTEGER NOT NULL DEFAULT 0, spending_limit REAL NOT NULL DEFAULT 0,
			daily_spending_limit REAL NOT NULL DEFAULT 0, concurrency_limit INTEGER NOT NULL DEFAULT 0,
			rpm_limit INTEGER NOT NULL DEFAULT 0, tpm_limit INTEGER NOT NULL DEFAULT 0,
			allowed_models TEXT NOT NULL DEFAULT '[]', allowed_channels TEXT NOT NULL DEFAULT '[]',
			allowed_channel_groups TEXT NOT NULL DEFAULT '[]', system_prompt TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS end_user_sessions (
			id TEXT PRIMARY KEY, end_user_id TEXT NOT NULL, tenant_id TEXT NOT NULL,
			access_token_hash TEXT NOT NULL UNIQUE, refresh_token_hash TEXT NOT NULL UNIQUE,
			access_expires_at TIMESTAMP NOT NULL, refresh_expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, last_seen_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			revoked_at TIMESTAMP, revoke_reason TEXT NOT NULL DEFAULT '', user_agent_hash TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		t.Fatalf("create portal auth tables: %v", err)
	}
}

func getPublicUsageSnapshot(t *testing.T, h *Handler, apiKey string) struct {
	Found bool `json:"found"`
	Usage struct {
		APIs map[string]usage.APISnapshot `json:"apis"`
	} `json:"usage"`
} {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/public/usage", bytes.NewReader([]byte(`{"api_key":"`+apiKey+`"}`)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.GetPublicUsageByAPIKey(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Found bool `json:"found"`
		Usage struct {
			APIs map[string]usage.APISnapshot `json:"apis"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return response
}

func getPortalPublicUsageSnapshot(t *testing.T, h *Handler, bearer string) struct {
	Found bool `json:"found"`
	Usage struct {
		APIs map[string]usage.APISnapshot `json:"apis"`
	} `json:"usage"`
} {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/public/usage", bytes.NewReader([]byte(`{}`)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer "+bearer)
	h.GetPublicUsageByAPIKey(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("portal status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Found bool `json:"found"`
		Usage struct {
			APIs map[string]usage.APISnapshot `json:"apis"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal portal response: %v", err)
	}
	return response
}

func TestGetPublicUsageByAPIKeyAggregatesOwnedCurrentSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupOwnedPublicUsageDB(t)
	tenantID := uuid.NewString()
	endUserID := uuid.NewString()
	keyA := usage.APIKeyRow{ID: uuid.NewString(), Key: "sk-memory-owned-a", Name: "A", EndUserID: endUserID}
	keyB := usage.APIKeyRow{ID: uuid.NewString(), Key: "sk-memory-owned-b", Name: "B", EndUserID: endUserID}
	standalone := usage.APIKeyRow{ID: uuid.NewString(), Key: "sk-memory-standalone", Name: "Standalone"}
	for _, row := range []usage.APIKeyRow{keyA, keyB, standalone} {
		if err := usage.UpsertAPIKeyForTenant(tenantID, row); err != nil {
			t.Fatalf("UpsertAPIKeyForTenant(%s): %v", row.Key, err)
		}
	}

	stats := usage.NewRequestStatistics()
	stats.MergeSnapshot(usage.StatisticsSnapshot{APIs: map[string]usage.APISnapshot{
		keyA.Key: {
			TotalRequests: 1,
			TotalTokens:   10,
			Models:        map[string]usage.ModelSnapshot{"gpt-5.4": {TotalRequests: 1, TotalTokens: 10}},
		},
		keyB.Key: {
			TotalRequests: 2,
			TotalTokens:   20,
			Models:        map[string]usage.ModelSnapshot{"grok-4": {TotalRequests: 2, TotalTokens: 20}},
		},
		standalone.Key: {
			TotalRequests: 4,
			TotalTokens:   40,
			Models:        map[string]usage.ModelSnapshot{"codex": {TotalRequests: 4, TotalTokens: 40}},
		},
	}})
	h := NewHandler(&config.Config{}, "", nil)
	h.SetUsageStatistics(stats)

	if _, err := usage.RuntimeDB().Exec(`
		INSERT INTO tenants (id, status, type) VALUES (?, 'active', 'standard')
	`, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := usage.RuntimeDB().Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status)
		VALUES (?, ?, 'portal-user', 'portal-user', 'Portal User', 'unused', 'active')
	`, endUserID, tenantID); err != nil {
		t.Fatalf("insert portal user: %v", err)
	}
	portalToken := "cpt_portal-memory-test"
	portalTokenSum := sha256.Sum256([]byte(portalToken))
	if _, err := usage.RuntimeDB().Exec(`
		INSERT INTO end_user_sessions (
			id, end_user_id, tenant_id, access_token_hash, refresh_token_hash, access_expires_at, refresh_expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, uuid.NewString(), endUserID, tenantID, hex.EncodeToString(portalTokenSum[:]), "unused-refresh-hash",
		time.Now().UTC().Add(time.Hour), time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatalf("insert portal session: %v", err)
	}
	enduser.SetDefault(enduser.NewService(usage.RuntimeDB()))

	// Raw secret is key-scoped (key A only), not the sibling pool.
	owned := getPublicUsageSnapshot(t, h, keyA.Key)
	if !owned.Found || len(owned.Usage.APIs) != 1 {
		t.Fatalf("owned response = %#v", owned)
	}
	for _, snapshot := range owned.Usage.APIs {
		if snapshot.TotalRequests != 1 || snapshot.TotalTokens != 10 {
			t.Fatalf("owned key-scoped = %+v, want requests=1 tokens=10", snapshot)
		}
	}

	// Portal cpt_ session remains account-wide across owned keys.
	portal := getPortalPublicUsageSnapshot(t, h, portalToken)
	if !portal.Found || len(portal.Usage.APIs) != 1 {
		t.Fatalf("portal response = %#v", portal)
	}
	for _, snapshot := range portal.Usage.APIs {
		if snapshot.TotalRequests != 3 || snapshot.TotalTokens != 30 {
			t.Fatalf("portal aggregate = %+v, want requests=3 tokens=30", snapshot)
		}
	}

	standaloneResponse := getPublicUsageSnapshot(t, h, standalone.Key)
	for _, snapshot := range standaloneResponse.Usage.APIs {
		if snapshot.TotalRequests != 4 || snapshot.TotalTokens != 40 {
			t.Fatalf("standalone aggregate = %+v, want requests=4 tokens=40", snapshot)
		}
	}

	rotated := keyA
	rotated.Key = "sk-memory-owned-a-rotated"
	if err := usage.UpdateAPIKeyByIDForTenant(tenantID, rotated); err != nil {
		t.Fatalf("UpdateAPIKeyByIDForTenant: %v", err)
	}
	// Rotated secret has no in-memory snapshot under the new value; sibling key B stays independent.
	afterRotate := getPublicUsageSnapshot(t, h, rotated.Key)
	for _, snapshot := range afterRotate.Usage.APIs {
		if snapshot.TotalRequests != 0 || snapshot.TotalTokens != 0 {
			t.Fatalf("post-rotate rotated secret = %+v, want empty key-scoped snapshot", snapshot)
		}
	}
	sibling := getPublicUsageSnapshot(t, h, keyB.Key)
	for _, snapshot := range sibling.Usage.APIs {
		if snapshot.TotalRequests != 2 || snapshot.TotalTokens != 20 {
			t.Fatalf("sibling key B = %+v, want requests=2 tokens=20", snapshot)
		}
	}
}

func TestGetPublicUsageByAPIKeyMasksCredentialEverywhere(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const apiKey = "sk-test-public-usage-secret-123456"
	stats := usage.NewRequestStatistics()
	wasEnabled := usage.StatisticsEnabled()
	usage.SetStatisticsEnabled(true)
	t.Cleanup(func() { usage.SetStatisticsEnabled(wasEnabled) })
	stats.Record(context.Background(), coreusage.Record{
		APIKey: apiKey,
		Model:  "gpt-4o-mini",
		Detail: coreusage.Detail{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage", bytes.NewReader([]byte(`{"api_key":"`+apiKey+`"}`)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h := NewHandler(&config.Config{}, "", nil)
	h.SetUsageStatistics(stats)
	h.GetPublicUsageByAPIKey(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), apiKey) {
		t.Fatalf("public response leaked full API key: %s", rec.Body.String())
	}

	var got struct {
		APIKey string `json:"api_key"`
		Usage  struct {
			APIs map[string]usage.APISnapshot `json:"apis"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.APIKey == apiKey || got.APIKey == "" {
		t.Fatalf("api_key = %q, want masked non-empty value", got.APIKey)
	}
	if _, exists := got.Usage.APIs[apiKey]; exists {
		t.Fatalf("usage.apis contains raw API key")
	}
	if _, exists := got.Usage.APIs[got.APIKey]; !exists {
		t.Fatalf("usage.apis does not contain masked key %q: %#v", got.APIKey, got.Usage.APIs)
	}
}

func TestGetPublicUsageChartDataReturnsPortalKeyDistributionWithoutSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupOwnedPublicUsageDB(t)

	tenantID := uuid.NewString()
	endUserID := uuid.NewString()
	otherUserID := uuid.NewString()
	keyA := usage.APIKeyRow{ID: uuid.NewString(), Key: "sk-chart-secret-a", Name: "Laptop", EndUserID: endUserID}
	keyB := usage.APIKeyRow{ID: uuid.NewString(), Key: "sk-chart-secret-b", Name: "Automation", EndUserID: endUserID}
	otherKey := usage.APIKeyRow{ID: uuid.NewString(), Key: "sk-chart-secret-other", Name: "Other", EndUserID: otherUserID}
	standalone := usage.APIKeyRow{ID: uuid.NewString(), Key: "sk-chart-secret-standalone", Name: "Standalone"}
	for _, row := range []usage.APIKeyRow{keyA, keyB, otherKey, standalone} {
		if err := usage.UpsertAPIKeyForTenant(tenantID, row); err != nil {
			t.Fatalf("UpsertAPIKeyForTenant(%s): %v", row.Name, err)
		}
	}

	db := usage.RuntimeDB()
	if _, err := db.Exec(`INSERT INTO tenants (id, status, type) VALUES (?, 'active', 'standard')`, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status)
		VALUES (?, ?, 'chart-user', 'chart-user', 'Portal Chart User', 'unused', 'active')
	`, endUserID, tenantID); err != nil {
		t.Fatalf("insert portal user: %v", err)
	}
	portalToken := "cpt_portal-chart-test"
	portalTokenSum := sha256.Sum256([]byte(portalToken))
	if _, err := db.Exec(`
		INSERT INTO end_user_sessions (
			id, end_user_id, tenant_id, access_token_hash, refresh_token_hash, access_expires_at, refresh_expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, uuid.NewString(), endUserID, tenantID, hex.EncodeToString(portalTokenSum[:]), "unused-chart-refresh-hash",
		time.Now().UTC().Add(time.Hour), time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatalf("insert portal session: %v", err)
	}
	enduser.SetDefault(enduser.NewService(db))

	insert := func(row usage.APIKeyRow, tokens int64) {
		usage.InsertLogWithDetailsIdentity(
			row.Key, row.ID, row.Name, "gpt-test", "test", "channel", "auth", false,
			time.Now().UTC(), 1, 0, usage.TokenStats{TotalTokens: tokens}, "", "", "",
		)
	}
	insert(keyA, 3)
	insert(keyB, 7)
	insert(keyB, 11)
	insert(otherKey, 100)
	insert(standalone, 200)

	h := NewHandler(&config.Config{}, "", nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/public/usage/chart-data", bytes.NewReader([]byte(`{"days":7}`)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer "+portalToken)
	h.UsageLogs().GetPublicUsageChartData(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("portal chart status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, secret := range []string{keyA.Key, keyB.Key, otherKey.Key, standalone.Key} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("portal chart response leaked secret %q: %s", secret, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), `"api_key":`) {
		t.Fatalf("portal chart response contains raw api_key field: %s", rec.Body.String())
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal portal chart response: %v", err)
	}
	var distribution []map[string]json.RawMessage
	if err := json.Unmarshal(payload["api_key_distribution"], &distribution); err != nil {
		t.Fatalf("unmarshal api_key_distribution: %v; body=%s", err, rec.Body.String())
	}
	if len(distribution) != 2 {
		t.Fatalf("api_key_distribution len = %d, want 2; body=%s", len(distribution), rec.Body.String())
	}
	allowedFields := map[string]bool{"api_key_id": true, "name": true, "requests": true, "tokens": true}
	points := make(map[string]struct {
		Name     string
		Requests int64
		Tokens   int64
	}, len(distribution))
	for _, item := range distribution {
		if len(item) != len(allowedFields) {
			t.Fatalf("public distribution point fields = %v, want exactly %v", item, allowedFields)
		}
		for field := range item {
			if !allowedFields[field] {
				t.Fatalf("public distribution point contains unexpected field %q", field)
			}
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal distribution point: %v", err)
		}
		var point struct {
			APIKeyID string `json:"api_key_id"`
			Name     string `json:"name"`
			Requests int64  `json:"requests"`
			Tokens   int64  `json:"tokens"`
		}
		if err := json.Unmarshal(encoded, &point); err != nil {
			t.Fatalf("unmarshal distribution point: %v", err)
		}
		points[point.APIKeyID] = struct {
			Name     string
			Requests int64
			Tokens   int64
		}{Name: point.Name, Requests: point.Requests, Tokens: point.Tokens}
	}
	if point := points[keyA.ID]; point.Name != keyA.Name || point.Requests != 1 || point.Tokens != 3 {
		t.Fatalf("key A public point = %+v, want own name and requests/tokens 1/3", point)
	}
	if point := points[keyB.ID]; point.Name != keyB.Name || point.Requests != 2 || point.Tokens != 18 {
		t.Fatalf("key B public point = %+v, want own name and requests/tokens 2/18", point)
	}

	standaloneRec := httptest.NewRecorder()
	standaloneContext, _ := gin.CreateTestContext(standaloneRec)
	standaloneContext.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/public/usage/chart-data",
		bytes.NewReader([]byte(`{"api_key":"`+standalone.Key+`","days":7}`)),
	)
	standaloneContext.Request.Header.Set("Content-Type", "application/json")
	h.UsageLogs().GetPublicUsageChartData(standaloneContext)
	if standaloneRec.Code != http.StatusOK {
		t.Fatalf("standalone chart status = %d, want %d; body=%s", standaloneRec.Code, http.StatusOK, standaloneRec.Body.String())
	}
	var standalonePayload map[string]json.RawMessage
	if err := json.Unmarshal(standaloneRec.Body.Bytes(), &standalonePayload); err != nil {
		t.Fatalf("unmarshal standalone chart response: %v", err)
	}
	if got := string(standalonePayload["api_key_distribution"]); got != "[]" {
		t.Fatalf("standalone api_key_distribution = %s, want []", got)
	}
}
