package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	configaccess "github.com/router-for-me/CLIProxyAPI/v6/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlite/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	_ "modernc.org/sqlite"
)

func TestAdminEndUserKeyRenameAndRotate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() {
		enduser.SetDefault(nil)
		_ = db.Close()
	})
	sqlapikey.InitTable(db)
	if _, err = db.Exec(`
		CREATE TABLE end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}

	tenantID := uuid.NewString()
	userID := uuid.NewString()
	if _, err = db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash)
		VALUES (?, ?, 'yuanwei', 'yuanwei', '袁蔚', 'unused')
	`, userID, tenantID); err != nil {
		t.Fatalf("insert end user: %v", err)
	}

	svc := enduser.NewService(db)
	enduser.SetDefault(svc)
	created, err := svc.CreateKey(context.Background(), tenantID, userID, "original")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	oldSecret := created.PlaintextKey
	keyID := created.APIKey.ID

	h := &Handler{}
	principal := identity.Principal{
		PlatformAdmin:   true,
		EffectiveTenant: identity.Tenant{ID: tenantID},
	}

	renameRecorder := httptest.NewRecorder()
	renameContext, _ := gin.CreateTestContext(renameRecorder)
	renameContext.Set(managementPrincipalKey, principal)
	renameContext.Params = gin.Params{{Key: "id", Value: userID}, {Key: "key_id", Value: keyID}}
	renameContext.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/end-users/"+userID+"/api-keys/"+keyID, bytes.NewBufferString(`{"name":"renamed"}`))
	renameContext.Request.Header.Set("Content-Type", "application/json")
	h.PatchEndUserAPIKey(renameContext)
	if renameRecorder.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want %d; body=%s", renameRecorder.Code, http.StatusOK, renameRecorder.Body.String())
	}
	var renamed enduser.APIKey
	if err = json.Unmarshal(renameRecorder.Body.Bytes(), &renamed); err != nil {
		t.Fatalf("decode rename response: %v", err)
	}
	if renamed.ID != keyID || renamed.Name != "renamed" {
		t.Fatalf("renamed key = %#v, want id=%s name=renamed", renamed, keyID)
	}

	rotateRecorder := httptest.NewRecorder()
	rotateContext, _ := gin.CreateTestContext(rotateRecorder)
	rotateContext.Set(managementPrincipalKey, principal)
	rotateContext.Params = gin.Params{{Key: "id", Value: userID}, {Key: "key_id", Value: keyID}}
	rotateContext.Request = httptest.NewRequest(http.MethodPost, "/v0/management/end-users/"+userID+"/api-keys/"+keyID+"/rotate", nil)
	h.PostEndUserAPIKeyRotate(rotateContext)
	if rotateRecorder.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, want %d; body=%s", rotateRecorder.Code, http.StatusOK, rotateRecorder.Body.String())
	}
	var rotated enduser.CreateKeyResult
	if err = json.Unmarshal(rotateRecorder.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotate response: %v", err)
	}
	if rotated.APIKey.ID != keyID {
		t.Fatalf("rotated id = %q, want stable id %q", rotated.APIKey.ID, keyID)
	}
	if rotated.PlaintextKey == "" || rotated.PlaintextKey == oldSecret {
		t.Fatalf("rotate did not return a new plaintext key")
	}

	var oldCount, newCount int
	if err = db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND key = ?`, tenantID, oldSecret).Scan(&oldCount); err != nil {
		t.Fatalf("query old key: %v", err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND id = ? AND key = ? AND end_user_id = ?`, tenantID, keyID, rotated.PlaintextKey, userID).Scan(&newCount); err != nil {
		t.Fatalf("query rotated key: %v", err)
	}
	if oldCount != 0 || newCount != 1 {
		t.Fatalf("rotation persistence old=%d new=%d, want old=0 new=1", oldCount, newCount)
	}
}

func TestAdminEndUserKeyRotateRejectsWrongOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() {
		enduser.SetDefault(nil)
		_ = db.Close()
	})
	sqlapikey.InitTable(db)
	if _, err = db.Exec(`
		CREATE TABLE end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}

	tenantID := uuid.NewString()
	ownerID := uuid.NewString()
	otherUserID := uuid.NewString()
	if _, err = db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash)
		VALUES (?, ?, 'owner', 'owner', 'Owner', 'unused'), (?, ?, 'other', 'other', 'Other', 'unused')
	`, ownerID, tenantID, otherUserID, tenantID); err != nil {
		t.Fatalf("insert end users: %v", err)
	}

	svc := enduser.NewService(db)
	enduser.SetDefault(svc)
	created, err := svc.CreateKey(context.Background(), tenantID, ownerID, "owner-key")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(managementPrincipalKey, identity.Principal{
		PlatformAdmin:   true,
		EffectiveTenant: identity.Tenant{ID: tenantID},
	})
	c.Params = gin.Params{{Key: "id", Value: otherUserID}, {Key: "key_id", Value: created.APIKey.ID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/end-users/"+otherUserID+"/api-keys/"+created.APIKey.ID+"/rotate", nil)
	(&Handler{}).PostEndUserAPIKeyRotate(c)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("wrong-owner rotate status = %d, want %d; body=%s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}

	var storedSecret string
	if err = db.QueryRow(`SELECT key FROM api_keys WHERE tenant_id = ? AND id = ?`, tenantID, created.APIKey.ID).Scan(&storedSecret); err != nil {
		t.Fatalf("query stored key: %v", err)
	}
	if storedSecret != created.PlaintextKey {
		t.Fatal("wrong-owner rotation changed the key")
	}
}

func TestAdminEndUserKeyRotateRefreshesLiveAuthCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		configaccess.Register(nil)
		enduser.SetDefault(nil)
		usage.CloseDB()
	})
	db := usage.RuntimeDB()
	if db == nil {
		t.Fatal("usage runtime DB is nil")
	}
	if _, err := db.Exec(`
		CREATE TABLE end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	if err := usage.EnsureEndUserQuotaColumns(db); err != nil {
		t.Fatalf("EnsureEndUserQuotaColumns: %v", err)
	}

	tenantID := uuid.NewString()
	userID := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash)
		VALUES (?, ?, 'cache-user', 'cache-user', 'Cache User', 'unused')
	`, userID, tenantID); err != nil {
		t.Fatalf("insert end user: %v", err)
	}

	svc := enduser.NewService(db)
	enduser.SetDefault(svc)
	created, err := svc.CreateKey(context.Background(), tenantID, userID, "cache-key")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	cfg := &config.Config{}
	accessManager := sdkaccess.NewManager()
	configaccess.Register(&cfg.SDKConfig)
	accessManager.SetProviders(sdkaccess.RegisteredProviders())
	authenticate := func(secret string) *sdkaccess.AuthError {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		_, authErr := accessManager.Authenticate(context.Background(), req)
		return authErr
	}
	if authErr := authenticate(created.PlaintextKey); authErr != nil {
		t.Fatalf("old key should authenticate before rotation: %v", authErr)
	}

	h := NewHandler(cfg, "", nil)
	h.SetAccessManager(accessManager)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(managementPrincipalKey, identity.Principal{
		PlatformAdmin:   true,
		EffectiveTenant: identity.Tenant{ID: tenantID},
	})
	c.Params = gin.Params{{Key: "id", Value: userID}, {Key: "key_id", Value: created.APIKey.ID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/end-users/"+userID+"/api-keys/"+created.APIKey.ID+"/rotate", nil)
	h.PostEndUserAPIKeyRotate(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var rotated enduser.CreateKeyResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotate response: %v", err)
	}
	if authErr := authenticate(created.PlaintextKey); authErr == nil {
		t.Fatal("old key still authenticates after rotation")
	}
	if authErr := authenticate(rotated.PlaintextKey); authErr != nil {
		t.Fatalf("rotated key does not authenticate immediately: %v", authErr)
	}
}
