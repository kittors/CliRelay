package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

const postgresManagementKey = "local-management-key"

type postgresManagementClient struct {
	router *gin.Engine
}

func TestManagementAPIPostgresDataStackRegression(t *testing.T) {
	client := setupPostgresManagementClient(t)

	client.expectStatus(t, http.MethodPut, "/v0/management/api-key-permission-profiles", `{"items":[{
		"id":"profile-route-test",
		"name":"Route Test Profile",
		"allowed-models":["gpt-route-test"],
		"allowed-channel-groups":["codex-group"],
		"system-prompt":"route profile"
	}]}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/api-key-permission-profiles", "", http.StatusOK, "profile-route-test")

	client.expectStatus(t, http.MethodPut, "/v0/management/api-key-entries", `{"items":[{
		"id":"key-route-test",
		"key":"sk-test-postgres-route",
		"name":"Postgres Route Key",
		"permission-profile-id":"profile-route-test"
	}]}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/api-key-entries", "", http.StatusOK, "Postgres Route Key")

	client.expectStatus(t, http.MethodPost, "/v0/management/model-configs", `{
		"id":"gpt-route-test",
		"owned_by":"route-owner",
		"description":"Postgres route model",
		"enabled":true,
		"pricing":{"mode":"token","input_price_per_million":1.25,"output_price_per_million":2.5}
	}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/model-configs?scope=all", "", http.StatusOK, "gpt-route-test")

	client.expectStatus(t, http.MethodPut, "/v0/management/model-pricing", `{"items":[{
		"model_id":"gpt-route-test",
		"input_price_per_million":1.25,
		"output_price_per_million":2.5,
		"cached_price_per_million":0.25
	}]}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/model-pricing", "", http.StatusOK, "gpt-route-test")

	client.expectStatus(t, http.MethodPut, "/v0/management/proxy-pool", `{"items":[{
		"id":"local-proxy",
		"name":"Local Proxy",
		"url":"http://127.0.0.1:7890",
		"enabled":true,
		"description":"route regression"
	}]}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/proxy-pool", "", http.StatusOK, "local-proxy")
	client.expectStatus(t, http.MethodPatch, "/v0/management/proxy-pool/local-proxy", `{
		"name":"Local Proxy Updated",
		"url":"http://127.0.0.1:7891",
		"enabled":false
	}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/proxy-pool", "", http.StatusOK, "Local Proxy Updated")

	client.expectStatus(t, http.MethodPut, "/v0/management/routing-config", `{
		"strategy":"round-robin",
		"include-default-group":true,
		"channel-groups":[{"name":"codex-group","match":{"channels":["codex"]}}],
		"path-routes":[{"path":"/route-codex","group":"codex-group","strip-prefix":true}]
	}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/routing-config", "", http.StatusOK, "codex-group")

	client.expectStatus(t, http.MethodPatch, "/v0/management/usage-statistics-enabled", `{"value":false}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/usage-statistics-enabled", "", http.StatusOK, "false")
	client.expectStatus(t, http.MethodPut, "/v0/management/codex-oauth-admission", `{"allowed_clients":["claude_code"]}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/codex-oauth-admission", "", http.StatusOK, "claude_code")

	client.expectStatus(t, http.MethodPut, "/v0/management/ccswitch-import-configs", `{"items":[{
		"id":"cc-route-test",
		"client-type":"codex",
		"provider-name":"route-provider",
		"default-model":"gpt-route-test",
		"route-path":"/route-test",
		"endpoint-path":"/v1/responses",
		"api-key-field":"apiKey"
	}]}`, http.StatusOK)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/ccswitch-import-configs", "", http.StatusOK, "cc-route-test")
	client.expectBodyContains(t, http.MethodPost, "/v0/management/public/ccswitch-import-configs", `{"api_key":"sk-test-postgres-route"}`, http.StatusOK, "cc-route-test")

	insertPostgresManagementUsageFixture(t)
	logID := client.expectUsageLog(t)
	client.expectBodyContains(t, http.MethodGet, fmt.Sprintf("/v0/management/usage/logs/%d/content", logID), "", http.StatusOK, "route-input")
	client.expectBodyContains(t, http.MethodPost, fmt.Sprintf("/v0/management/public/usage/logs/%d/content", logID), `{"api_key":"sk-test-postgres-route"}`, http.StatusOK, "route-output")
	client.expectBodyContains(t, http.MethodGet, "/v0/management/usage/chart-data?api_key=sk-test-postgres-route&days=1", "", http.StatusOK, "gpt-route-test")
	client.expectBodyContains(t, http.MethodGet, "/v0/management/usage/entity-stats?api_key=sk-test-postgres-route&days=1", "", http.StatusOK, `"requests":1`)
	client.expectBodyContains(t, http.MethodGet, "/v0/management/dashboard-summary?days=1", "", http.StatusOK, "total_requests")
	client.expectBodyContains(t, http.MethodPost, "/v0/management/public/usage/summary", `{"api_key":"sk-test-postgres-route","days":1}`, http.StatusOK, `"total_calls":1`)

	client.expectStatus(t, http.MethodDelete, "/v0/management/api-key-entries?key=sk-test-postgres-route&delete_logs=true", "", http.StatusOK)
	if got := usage.GetAPIKey("sk-test-postgres-route"); got != nil {
		t.Fatalf("api key still exists after DELETE: %#v", got)
	}
}

func TestManagementAPIPostgresAllRoutesSmoke(t *testing.T) {
	client := setupPostgresManagementClient(t)

	for _, key := range expectedManagementRoutes() {
		method, routePath, ok := strings.Cut(key, " ")
		if !ok {
			t.Fatalf("invalid route key: %q", key)
		}
		t.Run(key, func(t *testing.T) {
			path := postgresSmokePath(routePath)
			body := postgresSmokeBody(method, routePath)
			rec := client.request(method, path, body)
			if postgresSmokeAllowsStatus(routePath, rec.Code, rec.Body.String()) {
				return
			}
			if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed || rec.Code >= 500 {
				t.Fatalf("%s %s status=%d body=%s", method, path, rec.Code, rec.Body.String())
			}
		})
	}
}

func setupPostgresManagementClient(t *testing.T) postgresManagementClient {
	t.Helper()
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if strings.TrimSpace(dsn) == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}

	usage.CloseDB()
	t.Cleanup(usage.CloseDB)
	if err := usage.InitPostgres(config.PostgresConfig{
		DSN:          dsn,
		MaxOpenConns: 8,
		MaxIdleConns: 2,
	}, config.RequestLogStorageConfig{StoreContent: true}, time.UTC); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	truncatePostgresManagementTables(t, dsn)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	logDir := filepath.Join(tmpDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "main.log"), []byte("postgres route smoke\n"), 0o600); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}

	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	handler := managementhandlers.NewHandler(&config.Config{
		AuthDir: authDir,
		Routing: config.RoutingConfig{IncludeDefaultGroup: true},
	}, configPath, nil)
	handler.SetLocalPassword(postgresManagementKey)
	handler.SetLogDirectory(logDir)
	t.Cleanup(handler.Close)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterManagement(router, handler, ManagementOptions{})
	return postgresManagementClient{router: router}
}

func truncatePostgresManagementTables(t *testing.T, dsn string) {
	t.Helper()
	db, err := postgresstore.OpenRuntimeDB(context.Background(), config.PostgresConfig{
		DSN:          dsn,
		MaxOpenConns: 4,
		MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatalf("open postgres for truncate: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		TRUNCATE
			request_log_content,
			request_logs,
			auth_file_quota_snapshot_points,
			auth_file_quota_snapshots,
			auth_subject_quota_cycles,
			model_pricing,
			api_keys,
			api_key_permission_profiles,
			model_configs,
			model_owner_presets,
			auth_group_model_owner_mappings,
			model_openrouter_sync_state,
			proxy_pool,
			routing_config,
			runtime_settings,
			identity_fingerprint_account_policies,
			identity_fingerprints,
			ccswitch_import_configs
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatalf("truncate postgres management tables: %v", err)
	}
}

func insertPostgresManagementUsageFixture(t *testing.T) {
	t.Helper()
	usage.InsertLogWithDetailsIdentitySubject(
		"sk-test-postgres-route",
		"key-route-test",
		"subject-route-test",
		"Postgres Route Key",
		"gpt-route-test",
		"codex",
		"codex",
		"auth-route-test",
		false,
		time.Now().UTC(),
		120,
		30,
		usage.TokenStats{InputTokens: 10, OutputTokens: 5, CachedTokens: 2, TotalTokens: 15},
		`{"prompt":"route-input"}`,
		`{"text":"route-output"}`,
		`{"detail":"route-detail"}`,
	)
}

func (client postgresManagementClient) expectUsageLog(t *testing.T) int64 {
	t.Helper()
	rec := client.expectStatus(t, http.MethodGet, "/v0/management/usage/logs?api_key=sk-test-postgres-route&days=1&page=1&size=10", "", http.StatusOK)
	var payload struct {
		Items []struct {
			ID     int64  `json:"id"`
			APIKey string `json:"api_key"`
			Model  string `json:"model"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode usage logs: %v; body=%s", err, rec.Body.String())
	}
	if payload.Total != 1 || len(payload.Items) != 1 {
		t.Fatalf("usage logs total=%d len=%d body=%s", payload.Total, len(payload.Items), rec.Body.String())
	}
	if payload.Items[0].APIKey != "sk-test-postgres-route" || payload.Items[0].Model != "gpt-route-test" {
		t.Fatalf("usage log item = %#v", payload.Items[0])
	}
	return payload.Items[0].ID
}

func postgresSmokePath(routePath string) string {
	replacer := strings.NewReplacer(
		":id", "1",
		":task_id", "task-test",
		":name", "main.log",
		":channel", "codex",
		"*id", "gpt-route-test",
	)
	return replacer.Replace(routePath)
}

func postgresSmokeBody(method, routePath string) string {
	if method == http.MethodGet || method == http.MethodDelete {
		return ""
	}
	if strings.Contains(routePath, "/public/usage") || strings.Contains(routePath, "/public/ccswitch-import-configs") {
		return `{"api_key":"sk-test-postgres-route"}`
	}
	if routePath == "/v0/management/usage/import" {
		return `{"version":1,"usage":{}}`
	}
	return `{}`
}

func postgresSmokeAllowsStatus(routePath string, status int, body string) bool {
	if status == http.StatusNotFound {
		return strings.Contains(body, "not found") &&
			(strings.Contains(routePath, ":") || strings.Contains(routePath, "*"))
	}
	if status == http.StatusBadGateway {
		return routePath == "/v0/management/update/progress" && strings.Contains(body, "update_progress_failed")
	}
	if status != http.StatusServiceUnavailable {
		return false
	}
	allowed := []string{
		"auth manager unavailable",
		"core auth manager unavailable",
		"image generation service unavailable",
	}
	for _, text := range allowed {
		if strings.Contains(body, text) {
			return true
		}
	}
	return false
}

func (client postgresManagementClient) expectBodyContains(t *testing.T, method, path, body string, status int, needle string) {
	t.Helper()
	rec := client.expectStatus(t, method, path, body, status)
	if !strings.Contains(rec.Body.String(), needle) {
		t.Fatalf("%s %s body does not contain %q: %s", method, path, needle, rec.Body.String())
	}
}

func (client postgresManagementClient) expectStatus(t *testing.T, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	rec := client.request(method, path, body)
	if rec.Code != status {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, rec.Code, status, rec.Body.String())
	}
	return rec
}

func (client postgresManagementClient) request(method, path, body string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+postgresManagementKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	client.router.ServeHTTP(rec, req)
	return rec
}
